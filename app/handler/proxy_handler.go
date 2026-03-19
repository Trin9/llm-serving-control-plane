package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gate-service/app/billing"
	"gate-service/app/monitor"

	"github.com/gin-gonic/gin"
)

// lineBufferPool 用于复用 bytes.Buffer，减少 GC 压力（内存优化 W11）
// 在高并发长连接场景下，避免频繁分配新的字节切片
var lineBufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 8192)) // 8KB 初始缓冲
	},
}

// TokenStats 用于跟踪流式响应中的 token 统计信息
type TokenStats struct {
	model           string
	route           string
	startTime       time.Time
	firstTokenTime  time.Time
	tokenCount      int
	firstTokenFound bool
}

// NewTokenStats 创建一个新的 TokenStats 实例
func NewTokenStats(model, route string) *TokenStats {
	return &TokenStats{
		model:     model,
		route:     route,
		startTime: time.Now(),
	}
}

// ProcessFirstToken 处理第一个 token，记录时间并计算 TTFT
func (ts *TokenStats) ProcessFirstToken() {
	if !ts.firstTokenFound {
		ts.firstTokenFound = true
		ts.firstTokenTime = time.Now()

		// 计算 TTFT 并记录指标
		ttft := ts.firstTokenTime.Sub(ts.startTime).Seconds()
		monitor.AITimeToFirstToken.WithLabelValues(ts.model, ts.route).Observe(ttft)
	}
}

// IncrementTokenCount 增加 token 计数
func (ts *TokenStats) IncrementTokenCount() {
	ts.tokenCount++
}

// RecordTPOT 在流结束时记录 TPOT 指标
func (ts *TokenStats) RecordTPOT() {
	if ts.firstTokenFound && ts.tokenCount > 1 {
		endTime := time.Now()

		// 计算总的有效 token 生成时间（排除首字时间）
		totalTokenTime := endTime.Sub(ts.firstTokenTime).Seconds()

		// 计算 TPOT（Time Per Output Token）- 排除第一个 token
		tpot := totalTokenTime / float64(ts.tokenCount-1)

		// 记录 TPOT 指标
		monitor.AITimePerOutputToken.WithLabelValues(ts.model, ts.route).Observe(tpot)
	}
}

// PhaseSSEDataAndReport 检查并处理 SSE 数据行（W12 优化版）
//
// OpenAI 标准 SSE 响应结构示例：
//
//  1. 普通 Token 数据块 (每行开头有 "data: "):
//     data: {"id":"...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}
//
//  2. 包含统计信息的最后一个数据块 (vLLM 默认在最后一个 chunk 返回 usage):
//     data: {"id":"...","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}
//
//  3. 结束标志:
//     data: [DONE]
//
// 只对包含 usage 字段的最后一个 Chunk 做完整 JSON 解析，避免每个 Chunk 都消耗 CPU
func PhaseSSEDataAndReport(line []byte, stats *TokenStats) bool {
	lineStr := string(line)

	if strings.HasPrefix(lineStr, "data: ") {
		_, dataContent, _ := strings.Cut(lineStr, "data: ")

		// 优化点：先快速检查是否包含 "usage" 字段（字符串匹配比 JSON 解析快得多）
		if strings.Contains(dataContent, `"usage"`) {
			// 只有包含 usage 的最后一个 Chunk 才做完整 JSON 解析
			var sseData map[string]any
			if err := json.Unmarshal([]byte(dataContent), &sseData); err == nil {
				// 提取官方 token 统计（如果存在）
				if usage, ok := sseData["usage"].(map[string]any); ok {
					if totalTokens, ok := usage["total_tokens"].(float64); ok {
						// 使用官方统计覆盖手动计数（更准确）
						stats.tokenCount = int(totalTokens)
					}
				}
			}
			// 不返回 true，因为这是最后一个 Chunk，不需要继续处理
		} else if !strings.Contains(dataContent, "[DONE]") {
			// 对于普通数据行，只做轻量级检查，不解析 JSON
			// 快速检查是否有 choices 字段（字符串级别）
			if strings.Contains(dataContent, `"choices"`) {
				// 这是一个有效的 token 数据，增加计数
				stats.IncrementTokenCount()

				if !stats.firstTokenFound {
					// 处理第一个 token
					stats.ProcessFirstToken()
				}

				return true
			}
		}
	}

	return false
}

// ProxyHandlerFactory 返回一个注入了 BillingService 和 Router 的 gin.HandlerFunc
func ProxyHandlerFactory(billingSvc billing.BillingService, router Router) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A. 读取客户端请求体
		bodyBytes, _ := io.ReadAll(c.Request.Body)

		// 1. 根据请求内容动态选择后端 (W13 语义路由)
		targetURL := router.Route(bodyBytes)
		if targetURL == "" {
			c.JSON(503, gin.H{"error": "No available inference backends"})
			return
		}

		// 解析请求体获取模型名称 (用于统计)
		var requestBody map[string]any
		model := "unknown"
		if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
			if modelName, ok := requestBody["model"].(string); ok {
				model = modelName
			}
		}

		// 创建 TokenStats 实例用于跟踪指标
		stats := NewTokenStats(model, c.Request.URL.Path)

		// B. 构建发往 vLLM 的请求
		// 使用路由选择的 targetURL
		proxyReq, err := http.NewRequestWithContext(c.Request.Context(), "POST", targetURL, bytes.NewBuffer(bodyBytes))
		if err != nil {
			fmt.Printf("🔥 CRITICAL ERROR: %v\n", err)
			c.JSON(500, gin.H{"error": "Upstream error", "details": err.Error()})
			return
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		proxyReq.Header.Set("Authorization", "Bearer your-vllm-api-key") // 如果 vLLM 设置了 key

		// C. 发送请求
		client := &http.Client{}
		resp, err := client.Do(proxyReq)
		if err != nil {
			// 这里处理如果是 Context Cancelled 导致的错误
			c.JSON(500, gin.H{"error": "Upstream error", "details": err.Error()})
			return
		}
		defer resp.Body.Close()

		// D. 处理响应
		// 设置流式响应头
		contentType := resp.Header.Get("Content-Type")
		c.Writer.Header().Set("Content-Type", contentType)

		// 如果是流，才设置 Connection: keep-alive
		if strings.Contains(contentType, "event-stream") {
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.Header().Set("Transfer-Encoding", "chunked")
		}

		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("Transfer-Encoding", "chunked")

		// E. 核心循环：读取 vLLM 的流，实时写回 Client
		// 使用 32KB Reader 缓冲减少系统调用次数
		reader := bufio.NewReaderSize(resp.Body, 32*1024)

		for {
			// 使用 ReadBytes('\n') 确保每次读取都是完整的一行
			// 这样 PhaseSSEDataAndReport 才能准确识别 "data: " 前缀和 "usage" 字段
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Printf("🔥 ERROR reading from vLLM: %v\n", err)
				}
				// 处理最后可能剩下的数据（如果没有以 \n 结尾）
				if len(line) > 0 {
					PhaseSSEDataAndReport(line, stats)
					c.Writer.Write(line)
					c.Writer.Flush()
				}
				break
			}

			if len(line) == 0 {
				continue
			}

			// 检查 SSE 数据行并处理指标统计
			PhaseSSEDataAndReport(line, stats)

			// 实时写回 Client
			_, err = c.Writer.Write(line)
			if err != nil {
				// 客户端断开连接
				break
			}
			c.Writer.Flush() // 关键！必须立即刷新缓冲区，否则前端看不到打字机效果
		}

		// 流结束后记录 TPOT 指标
		stats.RecordTPOT()

		// F. 异步计费上报 (W12)
		// 构造 UsageRecord 并投递到 Channel
		// 注意：这里的 tokenCount 可能是估算值，也可能是官方 usage 值（如果 vLLM 返回了）
		if stats.tokenCount > 0 {
			record := billing.UsageRecord{
				RequestID:   c.Writer.Header().Get("X-Request-ID"), // 假设有，如果没有为空
				Model:       stats.model,
				User:        "anonymous", // 这里留坑，未来接 API Key 鉴权
				TotalTokens: stats.tokenCount,
				Timestamp:   time.Now(),
			}
			// 非阻塞调用，不会影响 HTTP 响应时间
			_ = billingSvc.ReportUsage(record)
		}
	}
}

// 💡 修改 HealthCheckHandler 以接受 *gin.Context
func HealthCheckHandler(c *gin.Context) { // 注意：参数现在是 c *gin.Context
	// Gin 框架中，我们不再直接使用 w http.ResponseWriter 和 r *http.Request
	// 而是通过 c.Writer 和 c.Request 来访问它们，但通常不需要直接操作它们。

	// 使用 Gin 推荐的 c.String() 或 c.JSON() 方法来返回响应
	// 这样它会自动设置状态码和响应头
	c.String(http.StatusOK, "Status: OK")

	// 如果想要返回 JSON:
	// c.JSON(http.StatusOK, gin.H{"status": "ok"})

	// log.Println("Health check accessed.")
	// 注意：Gin 默认集成了 Logger 中间件，日志记录会更自动化
}
