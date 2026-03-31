package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gate-service/app/billing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockBillingService implements the billing.BillingService interface for testing.
type MockBillingService struct {
	mock.Mock
}

// ReportUsage mocks the ReportUsage method of BillingService.
func (m *MockBillingService) ReportUsage(record billing.UsageRecord) error {
	args := m.Called(record)
	return args.Error(0)
}

// Start mocks the Start method of BillingService.
func (m *MockBillingService) Start() {
	m.Called()
}

// Stop mocks the Stop method of BillingService.
func (m *MockBillingService) Stop() {
	m.Called()
}

// MockRouter implements the Router interface for testing.
type MockRouter struct {
	mock.Mock
}

// Route mocks the Route method of Router.
func (m *MockRouter) Route(requestBody []byte) string {
	args := m.Called(requestBody)
	return args.String(0)
}

// UpdateBackends mocks the UpdateBackends method of Router.
func (m *MockRouter) UpdateBackends(urls []string) {
	m.Called(urls)
}

// TestProxyHandlerFactory_UpstreamNon200Response tests if non-200 responses from upstream are correctly proxied.
func TestProxyHandlerFactory_UpstreamNon200Response(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock upstream server to return a non-200 status.
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Error", "true")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Upstream Not Found"))
	}))
	defer upstreamServer.Close()

	mockBilling := new(MockBillingService)
	// We don't expect ReportUsage to be called for non-2xx responses in this test.
	mockBilling.On("ReportUsage", mock.Anything).Return(nil).Maybe()

	mockRouter := new(MockRouter)
	mockRouter.On("Route", mock.Anything).Return(upstreamServer.URL)

	router := gin.Default()
	router.POST("/v1/chat/completions", ProxyHandlerFactory(mockBilling, mockRouter))

	// Create a request to the proxy.
	reqBody := []byte(`{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`)
	req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "Upstream Not Found", recorder.Body.String())
	assert.Equal(t, "true", recorder.Header().Get("X-Upstream-Error"))
	mockBilling.AssertNotCalled(t, "ReportUsage", mock.Anything)
}

// TestProxyHandlerFactory_ClientDisconnect tests handling of client disconnection before upstream responds.
func TestProxyHandlerFactory_ClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock upstream server that delays before responding.
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay before writing any response to allow client to cancel first.
		time.Sleep(200 * time.Millisecond)

		// By the time we get here, the client should have disconnected.
		flusher, ok := w.(http.Flusher)
		assert.True(t, ok, "ResponseWriter does not implement http.Flusher")

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Write data (will likely fail due to client disconnect).
		_, _ = fmt.Fprint(w, "data: should not reach client\n\n")
		flusher.Flush()
	}))
	defer upstreamServer.Close()

	mockBilling := new(MockBillingService)
	// Billing should not be called because the client disconnected before response completion.
	mockBilling.On("ReportUsage", mock.Anything).Return(nil).Maybe()

	mockRouter := new(MockRouter)
	mockRouter.On("Route", mock.Anything).Return(upstreamServer.URL)

	router := gin.Default()
	router.POST("/v1/chat/completions", ProxyHandlerFactory(mockBilling, mockRouter))

	reqBody := []byte(`{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`)

	// Create a context that can be cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond) // Cancel before upstream responds.
		cancel()                          // Cancel the client context.
	}()

	router.ServeHTTP(recorder, req)

	// Expect a 408 Request Timeout status because the client disconnected before the upstream responded.
	assert.Equal(t, http.StatusRequestTimeout, recorder.Code)
	// The body should not contain any data since the client disconnected early.
	assert.Empty(t, recorder.Body.String())
}

// TestProxyHandlerFactory_UpstreamTimeout tests handling of upstream timeouts.
func TestProxyHandlerFactory_UpstreamTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock upstream server that delays longer than context timeout.
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // Longer than context timeout
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Should not be reached"))
	}))
	defer upstreamServer.Close()

	mockBilling := new(MockBillingService)
	mockBilling.On("ReportUsage", mock.Anything).Return(nil).Maybe()

	mockRouter := new(MockRouter)
	mockRouter.On("Route", mock.Anything).Return(upstreamServer.URL)

	router := gin.Default()
	router.POST("/v1/chat/completions", ProxyHandlerFactory(mockBilling, mockRouter))

	reqBody := []byte(`{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`)

	// Create a context with a short timeout to simulate upstream timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadGateway, recorder.Code) // Upstream timeout results in 502
	assert.Contains(t, recorder.Body.String(), "Upstream service error")
	assert.Contains(t, recorder.Body.String(), "context deadline exceeded")
	mockBilling.AssertNotCalled(t, "ReportUsage", mock.Anything)
}

// TestPhaseSSEDataAndReport_MalformedSSE tests handling of malformed SSE lines.
func TestPhaseSSEDataAndReport_MalformedSSE(t *testing.T) {
	stats := NewTokenStats("test-model", "/test")

	// Test malformed line (not starting with "data: ")
	assert.False(t, PhaseSSEDataAndReport([]byte("invalid line"), stats))
	assert.Equal(t, 0, stats.tokenCount)

	// Test line with "data: " but not valid JSON
	assert.False(t, PhaseSSEDataAndReport([]byte("data: {malformed json}"), stats))
	assert.Equal(t, 0, stats.tokenCount)

	// Test line with "data: " and valid JSON but no "choices" or "usage"
	assert.False(t, PhaseSSEDataAndReport([]byte(`data: {"foo":"bar"}`), stats))
	assert.Equal(t, 0, stats.tokenCount)
}

// TestPhaseSSEDataAndReport_UsageDegradation tests usage calculation when official usage is missing.
func TestPhaseSSEDataAndReport_UsageDegradation(t *testing.T) {
	stats := NewTokenStats("test-model", "/test")

	// First token, no usage in this chunk.
	assert.True(t, PhaseSSEDataAndReport([]byte(`data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hello"}}],"finish_reason":null}`), stats))
	assert.Equal(t, 1, stats.tokenCount)
	assert.True(t, stats.firstTokenFound)

	// Second token, no usage.
	assert.True(t, PhaseSSEDataAndReport([]byte(`data: {"id":"1","choices":[{"index":0,"delta":{"content":" World"}}],"finish_reason":null}`), stats))
	assert.Equal(t, 2, stats.tokenCount)

	// Final chunk with [DONE] and no usage (simulating a scenario where usage is never provided by upstream).
	assert.False(t, PhaseSSEDataAndReport([]byte(`data: [DONE]`), stats))
	assert.Equal(t, 2, stats.tokenCount) // Should still be 2 from manual counting.

	// Now a chunk with usage, should override.
	assert.False(t, PhaseSSEDataAndReport([]byte(`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}`), stats))
	assert.Equal(t, 15, stats.tokenCount) // Should be overridden to 15.
}

// TestProxyHandlerFactory_RequestIDPropagation tests Request ID generation and propagation.
func TestProxyHandlerFactory_RequestIDPropagation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock upstream server to echo back headers.
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Request-ID", r.Header.Get("X-Request-ID"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer upstreamServer.Close()

	mockBilling := new(MockBillingService)
	// Expect ReportUsage to be called and capture the record.
	var capturedRecord billing.UsageRecord
	mockBilling.On("ReportUsage", mock.AnythingOfType("billing.UsageRecord")).Return(nil).Run(func(args mock.Arguments) {
		capturedRecord = args.Get(0).(billing.UsageRecord)
	})

	mockRouter := new(MockRouter)
	mockRouter.On("Route", mock.Anything).Return(upstreamServer.URL)

	router := gin.Default()
	router.POST("/v1/chat/completions", ProxyHandlerFactory(mockBilling, mockRouter))

	reqBody := []byte(`{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`)

	// Test 1: No incoming X-Request-ID, should generate one.
	req1, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	recorder1 := httptest.NewRecorder()
	router.ServeHTTP(recorder1, req1)

	assert.Equal(t, http.StatusOK, recorder1.Code)
	clientReqID1 := recorder1.Header().Get("X-Request-ID")
	assert.NotEmpty(t, clientReqID1)
	assert.Equal(t, clientReqID1, recorder1.Header().Get("X-Echo-Request-ID"))
	mockBilling.AssertCalled(t, "ReportUsage", mock.AnythingOfType("billing.UsageRecord"))
	assert.Equal(t, clientReqID1, capturedRecord.RequestID)
	mockBilling.Calls = []mock.Call{} // Clear calls for next test

	// Test 2: With incoming X-Request-ID, should propagate it.
	expectedRequestID := "test-request-123"
	req2, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Request-ID", expectedRequestID)
	recorder2 := httptest.NewRecorder()
	router.ServeHTTP(recorder2, req2)

	assert.Equal(t, http.StatusOK, recorder2.Code)
	assert.Equal(t, expectedRequestID, recorder2.Header().Get("X-Request-ID"))
	assert.Equal(t, expectedRequestID, recorder2.Header().Get("X-Echo-Request-ID"))
	mockBilling.AssertCalled(t, "ReportUsage", mock.AnythingOfType("billing.UsageRecord"))
	assert.Equal(t, expectedRequestID, capturedRecord.RequestID)
}

// TestProxyHandlerFactory_SSEHeaders tests that SSE specific headers are correctly set.
func TestProxyHandlerFactory_SSEHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock upstream server to return SSE headers.
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Custom-Header", "custom-value") // Test propagation of other headers
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: chunk1\n\n"))
	}))
	defer upstreamServer.Close()

	mockBilling := new(MockBillingService)
	mockBilling.On("ReportUsage", mock.Anything).Return(nil).Maybe()

	mockRouter := new(MockRouter)
	mockRouter.On("Route", mock.Anything).Return(upstreamServer.URL)

	router := gin.Default()
	router.POST("/v1/chat/completions", ProxyHandlerFactory(mockBilling, mockRouter))

	reqBody := []byte(`{"model": "test-model", "messages": [{"role": "user", "content": "Hello"}]}`)
	req, _ := http.NewRequest("POST", "/v1/chat/completions", bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", recorder.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", recorder.Header().Get("Connection"))
	assert.Equal(t, "chunked", recorder.Header().Get("Transfer-Encoding"))
	assert.Equal(t, "custom-value", recorder.Header().Get("X-Custom-Header")) // Check custom header propagation
}
