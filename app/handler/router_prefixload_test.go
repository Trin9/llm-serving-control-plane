package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeStrategy_PrefixLoad(t *testing.T) {
	s, ok := NormalizeStrategy("prefix-load")
	assert.True(t, ok)
	assert.Equal(t, StrategyPrefixLoad, s)
	s, ok = NormalizeStrategy("PREFIX-LOAD")
	assert.True(t, ok)
	assert.Equal(t, StrategyPrefixLoad, s)
}

// TestPrefixLoad_LowLoadKeepsAffinity verifies that under low load the
// prefix-load strategy returns the same backend as pure prefix-hash affinity.
func TestPrefixLoad_LowLoadKeepsAffinity(t *testing.T) {
	r := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	r.SetStrategy(StrategyPrefixLoad)
	r.SetInflightThreshold(10)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello world"}]}`)
	owner := r.Route(body) // Route() under prefix-load falls back to pure affinity

	url, release := r.RouteWithLease(body)
	require.NotNil(t, release)
	release()
	assert.Equal(t, owner, url, "low load must keep the affinity owner")
}

// TestPrefixLoad_SpillsWhenOwnerOverloaded verifies a request spills off its
// affinity owner once the owner's in-flight count reaches the threshold.
func TestPrefixLoad_SpillsWhenOwnerOverloaded(t *testing.T) {
	r := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
		"http://c:8000/v1/chat/completions",
	})
	r.SetStrategy(StrategyPrefixLoad)
	r.SetInflightThreshold(1)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	owner := r.Route(body)

	url1, release1 := r.RouteWithLease(body) // owner at 0 < 1 -> owner, inflight -> 1
	assert.Equal(t, owner, url1)

	url2, release2 := r.RouteWithLease(body) // owner at 1 >= 1 -> must spill
	assert.NotEqual(t, owner, url2, "overloaded owner should spill to another backend")

	release1()
	release2()
}

// TestPrefixLoad_ReleaseRestoresAffinity verifies affinity is restored once the
// overloaded owner's lease is released.
func TestPrefixLoad_ReleaseRestoresAffinity(t *testing.T) {
	r := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	r.SetStrategy(StrategyPrefixLoad)
	r.SetInflightThreshold(1)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)
	owner := r.Route(body)

	url1, release1 := r.RouteWithLease(body)
	assert.Equal(t, owner, url1)

	url2, release2 := r.RouteWithLease(body)
	assert.NotEqual(t, owner, url2, "owner should spill while its lease is held")

	release1() // owner in-flight -> 0

	url3, release3 := r.RouteWithLease(body)
	assert.Equal(t, owner, url3, "affinity should be restored after release")

	release2()
	release3()
}

// TestPrefixLoad_NoBackendsReturnsEmpty verifies empty pool yields no lease.
func TestPrefixLoad_NoBackendsReturnsEmpty(t *testing.T) {
	r := NewConsistentHashRouter([]string{})
	r.SetStrategy(StrategyPrefixLoad)
	url, release := r.RouteWithLease([]byte(`{"model":"m"}`))
	assert.Empty(t, url)
	assert.Nil(t, release)
}

// TestPrefixLoad_ReleaseIsIdempotent verifies double-release is a safe no-op.
func TestPrefixLoad_ReleaseIsIdempotent(t *testing.T) {
	r := NewConsistentHashRouter([]string{"http://a:8000/v1/chat/completions"})
	r.SetStrategy(StrategyPrefixLoad)
	url, release := r.RouteWithLease([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	assert.NotEmpty(t, url)
	release()
	release() // must not panic or double-decrement
}

// TestPrefixLoad_RandomStillWorksWithLease verifies the random strategy
// acquires/releases a lease on its (randomly chosen) backend.
func TestPrefixLoad_RandomStillWorksWithLease(t *testing.T) {
	r := NewConsistentHashRouter([]string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	r.SetStrategy(StrategyRandom)
	url, release := r.RouteWithLease([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	assert.NotEmpty(t, url)
	release()
}

// TestModelRouter_RouteWithLease_DelegatesToModelPool verifies the model-bucketed
// router forwards lease acquisition to the per-model pool.
func TestModelRouter_RouteWithLease_DelegatesToModelPool(t *testing.T) {
	r := NewModelConsistentHashRouter(nil)
	r.RegisterModelBackends("m", []string{
		"http://a:8000/v1/chat/completions",
		"http://b:8000/v1/chat/completions",
	})
	r.SetStrategy(StrategyPrefixLoad)
	r.SetInflightThreshold(1)

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	url, release := r.RouteWithLease(body)
	assert.NotEmpty(t, url)
	require.NotNil(t, release)
	release()
}
