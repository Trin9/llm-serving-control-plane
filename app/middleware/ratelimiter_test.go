package middleware

import (
	"sync"
	"testing"
)

func TestGetLimiter_ConcurrentAccess(t *testing.T) {
	const key = "test-user-concurrent"

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = getLimiter(key)
		}()
	}

	wg.Wait()

	if got := getLimiter(key); got == nil {
		t.Fatal("expected limiter for key to be created")
	}
}
