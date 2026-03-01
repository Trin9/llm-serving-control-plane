package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gate-service/app/monitor"

	"github.com/gin-gonic/gin"
)

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

// isSSEDataLine 检查并处理 SSE 数据行
func PhaseSSEDataAndReport(line []byte, stats *TokenStats) bool {
	lineStr := string(line)

	if strings.HasPrefix(lineStr, "data: ") {
		_, dataContent, _ := strings.Cut(lineStr, "data: ")

		// 检查是否是有效的数据行（非 [DONE]）
		if !strings.Contains(dataContent, "[DONE]") {
			// 解析 JSON 数据以检查是否包含实际的 token 内容
			var sseData map[string]any
			if err := json.Unmarshal([]byte(dataContent), &sseData); err == nil {
				// 检查是否有 choices 字段，这是实际的 token 数据
				if choices, ok := sseData["choices"].([]any); ok && len(choices) > 0 {
					// 这是一个有效的 token 数据
					stats.IncrementTokenCount()

					if !stats.firstTokenFound {
						// 处理第一个 token
						stats.ProcessFirstToken()
					}

					return true
				}
			}
		}
	}

	return false
}

func ProxyHandler(c *gin.Context) {
	// A. 读取客户端请求体
	bodyBytes, _ := io.ReadAll(c.Request.Body)

	// 解析请求体获取模型名称
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
	// 重点：使用 c.Request.Context()，这样客户端断开时，vLLM 请求也会被 Cancel
	proxyReq, err := http.NewRequestWithContext(c.Request.Context(), "POST", GetVllmURL(), bytes.NewBuffer(bodyBytes))
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
		c.JSON(500, gin.H{"error": "Upstream error"})
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
	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			// Log error
			break
		}

		// 检查 SSE 数据行并处理指标统计
		PhaseSSEDataAndReport(line, stats)

		// 这里可以直接转发，也可以做一些处理（比如记录日志）
		// line 格式通常是 "data: {...}\n\n"

		fmt.Fprintf(c.Writer, "%s", line)
		c.Writer.Flush() // 关键！必须立即刷新缓冲区，否则前端看不到打字机效果
	}

	// 流结束后记录 TPOT 指标
	stats.RecordTPOT()
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
