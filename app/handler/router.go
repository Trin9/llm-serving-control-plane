package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Router 定义了 AI 请求的路由策略接口
type Router interface {
	// Route 根据请求内容返回一个后端的 URL
	Route(reqBody []byte) string
	// UpdateBackends 更新后端 Pod 列表
	UpdateBackends(urls []string)
}

// ConsistentHashRouter 实现了一致性哈希路由策略 (W13)
type ConsistentHashRouter struct {
	mu       sync.RWMutex
	backends []string
	replicas int // 虚拟节点数，用于平滑分布
	nodes    map[uint32]string
	keys     []uint32
}

func NewConsistentHashRouter(urls []string) *ConsistentHashRouter {
	r := &ConsistentHashRouter{
		backends: urls,
		replicas: 50, // 每个真实节点映射 50 个虚拟节点
		nodes:    make(map[uint32]string),
	}
	r.UpdateBackends(urls)
	return r
}

// hash 将字符串映射为 uint32
func (r *ConsistentHashRouter) hash(key string) uint32 {
	h := sha256.New()
	h.Write([]byte(key))
	sum := h.Sum(nil)
	return uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
}

// UpdateBackends 更新并重新构建哈希环
func (r *ConsistentHashRouter) UpdateBackends(urls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends = urls
	r.nodes = make(map[uint32]string)
	r.keys = nil

	for _, url := range urls {
		for i := 0; i < r.replicas; i++ {
			hash := r.hash(fmt.Sprintf("%s#%d", url, i))
			r.keys = append(r.keys, hash)
			r.nodes[hash] = url
		}
	}
	sort.Slice(r.keys, func(i, j int) bool {
		return r.keys[i] < r.keys[j]
	})
}

// Route 实现逻辑：解析 Prompt 前缀 -> 哈希 -> 选择后端
func (r *ConsistentHashRouter) Route(reqBody []byte) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.backends) == 0 {
		return ""
	}

	// 1. 提取语义特征 (Prefix)
	feature := r.extractFeature(reqBody)

	// 2. 一致性哈希查找
	hash := r.hash(feature)
	idx := sort.Search(len(r.keys), func(i int) bool {
		return r.keys[i] >= hash
	})

	if idx == len(r.keys) {
		idx = 0
	}

	return r.nodes[r.keys[idx]]
}

// extractFeature 提取 Prompt 的前缀特征用于路由
// 策略：提取历史对话/上下文（排除最新的提问），以最大化 Prefix Cache 命中率
func (r *ConsistentHashRouter) extractFeature(reqBody []byte) string {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(reqBody, &body); err != nil {
		return "default" // 解析失败则走默认路由
	}

	feature := body.Model
	
	if len(body.Messages) > 0 {
		var contentBuilder strings.Builder
		
		// 如果只有一条消息，就用这一条作为特征
		if len(body.Messages) == 1 {
			contentBuilder.WriteString(body.Messages[0].Content)
		} else {
			// 如果有多条消息，拼接除了最后一条（当前新提问）之外的所有历史消息
			// 这样，同一个多轮对话的后续请求，都会得到相同的特征哈希
			for i := 0; i < len(body.Messages)-1; i++ {
				contentBuilder.WriteString(body.Messages[i].Role)
				contentBuilder.WriteString(":")
				contentBuilder.WriteString(body.Messages[i].Content)
				contentBuilder.WriteString("|")
			}
		}

		content := contentBuilder.String()
		// 截取前 200 个字符，避免哈希计算过长，同时也能抓住核心的 System Prompt 和早期历史
		if len(content) > 200 {
			content = content[:200]
		}
		feature += ":" + strings.TrimSpace(content)
	}

	return feature
}
