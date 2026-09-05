package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Router defines the routing strategy interface for AI requests.
type Router interface {
	// Route returns a backend URL based on the request content
	Route(reqBody []byte) string
	// UpdateBackends updates the list of backend Pods
	UpdateBackends(urls []string)
}

// ModelRouteValidator reports model-aware routing errors before proxying.
// Static routers intentionally do not implement this interface.
type ModelRouteValidator interface {
	ValidateModelRoute(reqBody []byte) error
}

// ConsistentHashRouter implements a consistent hash routing strategy (W13).
type ConsistentHashRouter struct {
	mu       sync.RWMutex
	backends []string
	replicas int // number of virtual nodes for smooth distribution
	nodes    map[uint32]string
	keys     []uint32
}

// ModelConsistentHashRouter buckets requests by model and maintains an independent hash ring per model.
// This keeps different models from sharing a single global backend ring, keeps routing boundaries
// clear, and allows different backend pools to be bound to each model in the future.
type ModelConsistentHashRouter struct {
	mu                  sync.RWMutex
	defaultRouter       *ConsistentHashRouter
	modelRouters        map[string]*ConsistentHashRouter
	modelRoutingEnabled bool
}

func NewConsistentHashRouter(urls []string) *ConsistentHashRouter {
	r := &ConsistentHashRouter{
		backends: urls,
		replicas: 50, // map each real node to 50 virtual nodes
		nodes:    make(map[uint32]string),
	}
	r.UpdateBackends(urls)
	return r
}

func NewModelConsistentHashRouter(defaultURLs []string) *ModelConsistentHashRouter {
	return &ModelConsistentHashRouter{
		defaultRouter: NewConsistentHashRouter(defaultURLs),
		modelRouters:  make(map[string]*ConsistentHashRouter),
	}
}

func (r *ModelConsistentHashRouter) RegisterModelBackends(model string, urls []string) {
	model = normalizeModelKey(model)
	if model == "" {
		r.UpdateBackends(urls)
		return
	}

	normalized := normalizeURLs(urls)
	pool := NewConsistentHashRouter(normalized)

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(normalized) == 0 {
		delete(r.modelRouters, model)
		return
	}
	if r.defaultRouter == nil {
		r.defaultRouter = NewConsistentHashRouter(normalized)
	}
	if pool == nil {
		delete(r.modelRouters, model)
		return
	}
	r.modelRouters[model] = pool
	r.modelRoutingEnabled = true
}

func (r *ModelConsistentHashRouter) UpdateModelBackends(modelBackends map[string][]string) {
	normalizedBackends := make(map[string][]string, len(modelBackends))
	for model, urls := range modelBackends {
		normalizedBackends[normalizeModelKey(model)] = normalizeURLs(urls)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.modelRouters == nil {
		r.modelRouters = make(map[string]*ConsistentHashRouter)
	}

	for model, urls := range normalizedBackends {
		if model == "" || model == "default" {
			if r.defaultRouter == nil {
				r.defaultRouter = NewConsistentHashRouter([]string{})
			}
			r.defaultRouter.UpdateBackends(urls)
			continue
		}
		r.modelRouters[model] = NewConsistentHashRouter(normalizeURLs(urls))
		r.modelRoutingEnabled = true
	}

	for model := range r.modelRouters {
		if _, ok := normalizedBackends[model]; !ok {
			delete(r.modelRouters, model)
		}
	}
}

func (r *ModelConsistentHashRouter) UpdateBackends(urls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.defaultRouter == nil {
		r.defaultRouter = NewConsistentHashRouter(urls)
		return
	}
	r.defaultRouter.UpdateBackends(urls)
}

func (r *ModelConsistentHashRouter) Route(reqBody []byte) string {
	model := extractModelName(reqBody)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if model != "" {
		if pool, ok := r.modelRouters[model]; ok && pool != nil && len(pool.backends) > 0 {
			return pool.Route(reqBody)
		}
	}
	if r.modelRoutingEnabled {
		return ""
	}
	if r.defaultRouter == nil {
		return ""
	}
	return r.defaultRouter.Route(reqBody)
}

// ValidateModelRoute enforces model isolation after dynamic model discovery is active.
// A default-only static configuration remains backward compatible.
func (r *ModelConsistentHashRouter) ValidateModelRoute(reqBody []byte) error {
	model := extractModelName(reqBody)

	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.modelRoutingEnabled {
		return nil
	}
	if model == "" {
		return fmt.Errorf("model is required when model-aware routing is enabled")
	}
	if pool, ok := r.modelRouters[model]; !ok || pool == nil || len(pool.backends) == 0 {
		return fmt.Errorf("model %q is not registered", model)
	}
	return nil
}

func extractModelName(reqBody []byte) string {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(reqBody, &body); err != nil {
		return ""
	}
	return normalizeModelKey(body.Model)
}

// hash maps a string to a uint32
func (r *ConsistentHashRouter) hash(key string) uint32 {
	h := sha256.New()
	h.Write([]byte(key))
	sum := h.Sum(nil)
	return uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
}

// UpdateBackends updates and rebuilds the hash ring
func (r *ConsistentHashRouter) UpdateBackends(urls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends = normalizeURLs(urls)
	r.nodes = make(map[uint32]string)
	r.keys = nil

	for _, url := range r.backends {
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

// Route implements: extract the prompt prefix -> hash -> select a backend
func (r *ConsistentHashRouter) Route(reqBody []byte) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.backends) == 0 {
		return ""
	}

	// 1. Extract the semantic feature (prefix)
	feature := r.extractFeature(reqBody)

	// 2. Look up via consistent hashing
	hash := r.hash(feature)
	idx := sort.Search(len(r.keys), func(i int) bool {
		return r.keys[i] >= hash
	})

	if idx == len(r.keys) {
		idx = 0
	}

	return r.nodes[r.keys[idx]]
}

// extractFeature extracts a prefix feature from the prompt for routing.
// Strategy: use prior conversation/context (excluding the latest question) to maximize prefix-cache hit rate.
func (r *ConsistentHashRouter) extractFeature(reqBody []byte) string {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	if err := json.Unmarshal(reqBody, &body); err != nil {
		return "default" // fall back to the default route on parse failure
	}

	feature := body.Model

	if len(body.Messages) > 0 {
		var contentBuilder strings.Builder

		// If there is only one message, use it as the feature
		if len(body.Messages) == 1 {
			contentBuilder.WriteString(body.Messages[0].Content)
		} else {
			// With multiple messages, concatenate all prior messages except the last (the current question),
			// so subsequent turns of the same multi-turn conversation produce the same feature hash.
			for i := 0; i < len(body.Messages)-1; i++ {
				contentBuilder.WriteString(body.Messages[i].Role)
				contentBuilder.WriteString(":")
				contentBuilder.WriteString(body.Messages[i].Content)
				contentBuilder.WriteString("|")
			}
		}

		content := contentBuilder.String()
		// Truncate to the first 200 characters to keep hashing fast while still
		// capturing the core system prompt and early history.
		if len(content) > 200 {
			content = content[:200]
		}
		feature += ":" + strings.TrimSpace(content)
	}

	if feature == "" {
		feature = "default"
	}

	return feature
}
