package handler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubBackendSource struct {
	urls []string
}

func (s *stubBackendSource) Discover() ([]string, error) {
	return append([]string{}, s.urls...), nil
}

type stubModelBackendSource struct {
	mu            sync.RWMutex
	modelBackends map[string][]string
}

func (s *stubModelBackendSource) Discover() ([]string, error) {
	return nil, nil
}

func (s *stubModelBackendSource) DiscoverByModel() (map[string][]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string][]string, len(s.modelBackends))
	for model, urls := range s.modelBackends {
		result[model] = append([]string{}, urls...)
	}
	return result, nil
}

func (s *stubModelBackendSource) SetModelBackends(modelBackends map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelBackends = modelBackends
}

func TestStaticBackendSourceFromEnv_ParsesAndNormalizesURLs(t *testing.T) {
	t.Setenv("VLLM_URLS", "http://a:8000/v1/chat/completions, ,http://b:8000/v1/chat/completions,http://a:8000/v1/chat/completions/")

	source := NewStaticBackendSourceFromEnv("VLLM_URLS", []string{"http://fallback:8000/v1/chat/completions"})
	urls, err := source.Discover()
	require.NoError(t, err)
	assert.Equal(t, []string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	}, urls)
}

func TestKubernetesBackendSource_DiscoverUsesReadyPodIPs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/namespaces/default/endpoints", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"subsets": []map[string]any{{
					"addresses": []map[string]string{{"ip": "10.0.0.11"}, {"ip": "10.0.0.12"}},
					"ports":     []map[string]int32{{"port": 8000}},
				}},
			}},
		})
	}))
	defer server.Close()

	tokenFile, err := os.CreateTemp(t.TempDir(), "token")
	require.NoError(t, err)
	_, err = tokenFile.WriteString("test-token\n")
	require.NoError(t, err)
	require.NoError(t, tokenFile.Close())

	source := NewKubernetesBackendSource("default", server.URL, 8000)
	source.tokenPath = tokenFile.Name()
	source.httpClient = server.Client()
	source.httpClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	urls, err := source.Discover()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"http://10.0.0.11:8000/v1/chat/completions",
		"http://10.0.0.12:8000/v1/chat/completions",
	}, urls)
}

func TestStartBackendRefresh_UpdatesRouter(t *testing.T) {
	source := &stubBackendSource{urls: []string{"http://10.0.0.11:8000/v1/chat/completions"}}
	router := NewConsistentHashRouter([]string{"http://old:8000/v1/chat/completions"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartBackendRefresh(ctx, source, router, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		router.mu.RLock()
		defer router.mu.RUnlock()
		return len(router.backends) == 1 && router.backends[0] == "http://10.0.0.11:8000/v1/chat/completions"
	}, 500*time.Millisecond, 10*time.Millisecond)
}

func TestModelConsistentHashRouter_RoutesWithinModelPool(t *testing.T) {
	router := NewModelConsistentHashRouter([]string{"http://default:8000/v1/chat/completions"})
	router.RegisterModelBackends("model-a", []string{
		"http://a1:8000/v1/chat/completions",
		"http://a2:8000/v1/chat/completions",
	})
	router.RegisterModelBackends("model-b", []string{
		"http://b1:8000/v1/chat/completions",
		"http://b2:8000/v1/chat/completions",
	})

	requestA := []byte(`{"model":"model-a","messages":[{"role":"user","content":"hello world"}]}`)
	requestB := []byte(`{"model":"model-b","messages":[{"role":"user","content":"hello world"}]}`)
	requestFallback := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)

	backendA := router.Route(requestA)
	backendB := router.Route(requestB)
	backendFallback := router.Route(requestFallback)

	assert.Contains(t, []string{"http://a1:8000/v1/chat/completions", "http://a2:8000/v1/chat/completions"}, backendA)
	assert.Contains(t, []string{"http://b1:8000/v1/chat/completions", "http://b2:8000/v1/chat/completions"}, backendB)
	assert.Empty(t, backendFallback)
	assert.NotEqual(t, backendA, backendB)
	assert.Equal(t, backendA, router.Route(requestA))
	assert.Error(t, router.ValidateModelRoute(requestFallback))
	assert.Error(t, router.ValidateModelRoute([]byte(`{"model":"unknown"}`)))
	assert.NoError(t, router.ValidateModelRoute(requestA))
}

func TestModelConsistentHashRouter_DefaultOnlyConfigurationAllowsMissingModel(t *testing.T) {
	router := NewModelConsistentHashRouter([]string{"http://default:8000/v1/chat/completions"})
	router.UpdateModelBackends(map[string][]string{
		"default": {"http://default:8000/v1/chat/completions"},
	})

	assert.NoError(t, router.ValidateModelRoute([]byte(`{"messages":[]}`)))
}

func TestModelConsistentHashRouter_UpdateNormalizesAndRemovesStalePools(t *testing.T) {
	router := NewModelConsistentHashRouter(nil)
	router.UpdateModelBackends(map[string][]string{
		"Qwen/Qwen2.5-7B": {"http://qwen:8000/v1/chat/completions"},
	})

	assert.NoError(t, router.ValidateModelRoute([]byte(`{"model":"Qwen/Qwen2.5-7B"}`)))
	router.UpdateModelBackends(map[string][]string{})
	assert.Error(t, router.ValidateModelRoute([]byte(`{"model":"Qwen/Qwen2.5-7B"}`)))
}

func TestRefreshBackends_ClearsModelPoolsAfterEmptyDiscovery(t *testing.T) {
	source := &stubModelBackendSource{modelBackends: map[string][]string{
		"model-a": {"http://a:8000/v1/chat/completions"},
	}}
	router := NewModelConsistentHashRouter(nil)

	refreshBackends(source, router)
	require.NoError(t, router.ValidateModelRoute([]byte(`{"model":"model-a"}`)))

	source.SetModelBackends(map[string][]string{})
	refreshBackends(source, router)
	assert.Error(t, router.ValidateModelRoute([]byte(`{"model":"model-a"}`)))
}

func TestKubernetesBackendSource_DiscoverByModelUsesServiceNameAndLabels(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/services":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{
					"metadata": map[string]any{
						"name":   "model-a",
						"labels": map[string]string{"llm-model": "model-a"},
					},
				}},
			})
		case "/api/v1/namespaces/default/endpoints":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{
					"metadata": map[string]any{"name": "model-a"},
					"subsets": []map[string]any{{
						"addresses": []map[string]string{{"ip": "10.0.0.11"}, {"ip": "10.0.0.12"}},
						"ports":     []map[string]int32{{"port": 8000}},
					}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tokenFile, err := os.CreateTemp(t.TempDir(), "token")
	require.NoError(t, err)
	_, err = tokenFile.WriteString("test-token\n")
	require.NoError(t, err)
	require.NoError(t, tokenFile.Close())

	source := NewKubernetesBackendSource("default", server.URL, 8000)
	source.tokenPath = tokenFile.Name()
	source.httpClient = server.Client()
	source.httpClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	backends, err := source.DiscoverByModel()
	require.NoError(t, err)
	assert.Contains(t, backends, "model-a")
	assert.Len(t, backends["model-a"], 2)
	assert.ElementsMatch(t, []string{
		"http://10.0.0.11:8000/v1/chat/completions",
		"http://10.0.0.12:8000/v1/chat/completions",
	}, backends["model-a"])
}

func TestKubernetesBackendSource_DiscoverByModelIgnoresUnlabelledEndpoints(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/services":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case "/api/v1/namespaces/default/endpoints":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{
					"metadata": map[string]any{"name": "unrelated-service"},
					"subsets": []map[string]any{{
						"addresses": []map[string]string{{"ip": "10.0.0.11"}},
						"ports":     []map[string]int32{{"port": 8000}},
					}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tokenFile, err := os.CreateTemp(t.TempDir(), "token")
	require.NoError(t, err)
	_, err = tokenFile.WriteString("test-token\n")
	require.NoError(t, err)
	require.NoError(t, tokenFile.Close())

	source := NewKubernetesBackendSource("default", server.URL, 8000)
	source.tokenPath = tokenFile.Name()
	source.httpClient = server.Client()
	source.httpClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	backends, err := source.DiscoverByModel()
	require.NoError(t, err)
	assert.Empty(t, backends)
}

// ---------------------------------------------------------------------------
// Phase 6 (T-A2): routing strategy switch (prefix-hash vs random)
// ---------------------------------------------------------------------------

// prefixHashBody is a request whose feature hash is stable across calls: the
// system message is constant and extractFeature only reads model + prior
// messages (truncated to 200 chars).
const prefixHashBody = `{"model":"Qwen/Qwen2.5-0.5B-Instruct","messages":[{"role":"system","content":"You are a helpful assistant. SHARED-SYSTEM-PREFIX-0123456789"},{"role":"user","content":"Question #1"}]}`

func TestNormalizeStrategy(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "", want: StrategyPrefixHash, wantOK: true},
		{in: "prefix-hash", want: StrategyPrefixHash, wantOK: true},
		{in: " prefix-hash ", want: StrategyPrefixHash, wantOK: true},
		{in: "RANDOM", want: StrategyRandom, wantOK: true},
		{in: "random", want: StrategyRandom, wantOK: true},
		{in: "bogus", want: StrategyPrefixHash, wantOK: false},
	}
	for _, tc := range tests {
		got, ok := NormalizeStrategy(tc.in)
		assert.Equal(t, tc.want, got, "input %q", tc.in)
		assert.Equal(t, tc.wantOK, ok, "input %q", tc.in)
	}
}

func TestConsistentHashRouter_PrefixHashPinsSameFeatureToOneBackend(t *testing.T) {
	router := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	assert.Equal(t, StrategyPrefixHash, router.Strategy(), "prefix-hash is the default strategy")

	first := router.Route([]byte(prefixHashBody))
	require.NotEmpty(t, first)
	for i := 0; i < 200; i++ {
		assert.Equal(t, first, router.Route([]byte(prefixHashBody)),
			"same prompt prefix must always route to the same backend")
	}
}

func TestConsistentHashRouter_RandomSpreadsSameFeatureAcrossBackends(t *testing.T) {
	router := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	router.SetStrategy(StrategyRandom)
	assert.Equal(t, StrategyRandom, router.Strategy())

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		counts[router.Route([]byte(prefixHashBody))]++
	}
	require.Len(t, counts, 2, "random strategy must reach both backends")
	// 400 uniform draws: floor of 120 is ~8 sigma below the 200 mean, so a
	// healthy RNG never trips this.
	for url, n := range counts {
		assert.Greater(t, n, 120, "backend %s received %d/400 draws", url, n)
	}
}

func TestConsistentHashRouter_RandomWithNoBackendsReturnsEmpty(t *testing.T) {
	router := NewConsistentHashRouter(nil)
	router.SetStrategy(StrategyRandom)
	assert.Empty(t, router.Route([]byte(prefixHashBody)))
}

func TestModelConsistentHashRouter_StrategyInheritedByExistingAndFuturePools(t *testing.T) {
	router := NewModelConsistentHashRouter(nil)
	router.UpdateModelBackends(map[string][]string{
		"model-a": {"http://a1:8000/v1/chat/completions", "http://a2:8000/v1/chat/completions"},
	})

	// Flip to random after the pool already exists: it must apply to that pool.
	router.SetStrategy(StrategyRandom)
	assert.Equal(t, StrategyRandom, router.Strategy())

	bodyA := []byte(`{"model":"model-a","messages":[{"role":"user","content":"same body"}]}`)
	countsA := map[string]int{}
	for i := 0; i < 400; i++ {
		countsA[router.Route(bodyA)]++
	}
	require.Len(t, countsA, 2, "existing pool must follow the strategy switch")

	// A pool registered AFTER the switch must inherit random as well.
	router.RegisterModelBackends("model-b", []string{
		"http://b1:8000/v1/chat/completions",
		"http://b2:8000/v1/chat/completions",
	})
	bodyB := []byte(`{"model":"model-b","messages":[{"role":"user","content":"same body"}]}`)
	countsB := map[string]int{}
	for i := 0; i < 400; i++ {
		countsB[router.Route(bodyB)]++
	}
	require.Len(t, countsB, 2, "new pool must inherit the active strategy")
}
