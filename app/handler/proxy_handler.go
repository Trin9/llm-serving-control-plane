package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gate-service/app/billing"
	"gate-service/app/monitor"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// lineBufferPool reuses bytes.Buffer instances to reduce GC pressure (memory optimization W11).
// It avoids repeatedly allocating new byte slices in high-concurrency, long-lived connection scenarios.
var lineBufferPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 8192)) // 8KB initial buffer
	},
}

// TokenStats tracks token statistics across a streaming response.
type TokenStats struct {
	model            string
	route            string
	startTime        time.Time
	firstTokenTime   time.Time
	tokenCount       int
	promptTokens     int
	completionTokens int
	usageSource      string // "official" (vLLM usage) or "estimated" (chunk-based)
	firstTokenFound  bool
}

// NewTokenStats creates a new TokenStats instance.
func NewTokenStats(model, route string) *TokenStats {
	return &TokenStats{
		model:     model,
		route:     route,
		startTime: time.Now(),
	}
}

// ProcessFirstToken handles the first token: it records the time and computes TTFT.
func (ts *TokenStats) ProcessFirstToken() {
	if !ts.firstTokenFound {
		ts.firstTokenFound = true
		ts.firstTokenTime = time.Now()

		// Compute TTFT and record the metric
		ttft := ts.firstTokenTime.Sub(ts.startTime).Seconds()
		monitor.AITimeToFirstToken.WithLabelValues(ts.model, ts.route).Observe(ttft)
	}
}

// IncrementTokenCount increments the token count.
func (ts *TokenStats) IncrementTokenCount() {
	ts.tokenCount++
}

// RecordTPOT records the TPOT metric at the end of the stream.
func (ts *TokenStats) RecordTPOT() {
	if ts.firstTokenFound && ts.tokenCount > 1 {
		endTime := time.Now()

		// Compute total effective token generation time (excluding the time to first token)
		totalTokenTime := endTime.Sub(ts.firstTokenTime).Seconds()

		// Compute TPOT (time per output token) - excludes the first token
		tpot := totalTokenTime / float64(ts.tokenCount-1)

		// Record the TPOT metric
		monitor.AITimePerOutputToken.WithLabelValues(ts.model, ts.route).Observe(tpot)
	}
}

// PhaseSSEDataAndReport inspects a Server-Sent Events (SSE) data line and updates
// the token statistics carried by TokenStats. The gateway consumes the OpenAI
// compatible Chat Completions streaming format, which vLLM also serves.
//
// Protocol references:
//   - OpenAI Chat Completions API (streaming):
//     https://platform.openai.com/docs/api-reference/chat/create
//   - vLLM OpenAI-compatible server:
//     https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html
//
// SSE response structure (each "data: " line is followed by a blank line "\n\n"):
//
//  1. Regular token chunk:
//     data: {"id":"...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}
//
//  2. Final chunk carrying usage (vLLM returns usage on the last chunk before [DONE]):
//     data: {"id":"...","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}
//
//  3. Stream end marker:
//     data: [DONE]
//
// Only the chunk that contains "usage" is fully parsed as JSON; other chunks are
// matched cheaply at the string level to keep the hot path fast.
func PhaseSSEDataAndReport(line []byte, stats *TokenStats) bool {
	lineStr := string(line)

	if strings.HasPrefix(lineStr, "data: ") {
		_, dataContent, _ := strings.Cut(lineStr, "data: ")

		// Optimization: first quickly check whether the "usage" field is present
		// (string matching is far faster than JSON parsing)
		if strings.Contains(dataContent, `"usage"`) {
			// Only the final chunk containing "usage" is fully parsed as JSON
			var sseData map[string]any
			if err := json.Unmarshal([]byte(dataContent), &sseData); err == nil {
				// Extract official token statistics if present
				if usage, ok := sseData["usage"].(map[string]any); ok {
					if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
						stats.promptTokens = int(promptTokens)
					}
					if completionTokens, ok := usage["completion_tokens"].(float64); ok {
						stats.completionTokens = int(completionTokens)
					}
					if totalTokens, ok := usage["total_tokens"].(float64); ok {
						// Override manual counting with the official stats (more accurate)
						stats.tokenCount = int(totalTokens)
					} else if stats.completionTokens > 0 {
						stats.tokenCount = stats.promptTokens + stats.completionTokens
					}
					stats.usageSource = "official"
				}
			} else {
				fmt.Printf("⚠️ WARN: Failed to parse SSE data with usage: %v, content: %s\n", err, dataContent)
			}
			// Do not return true: this is the final chunk and needs no further processing
		} else if !strings.Contains(dataContent, "[DONE]") {
			// For regular data lines, only do a lightweight check without parsing JSON
			// Quickly check for a "choices" field (string level)
			if strings.Contains(dataContent, `"choices"`) {
				// Valid token data; increment the counter
				stats.IncrementTokenCount()
				if stats.usageSource == "" {
					stats.usageSource = "estimated"
				}

				if !stats.firstTokenFound {
					// Handle the first token
					stats.ProcessFirstToken()
				}

				return true
			}
		}
	}

	return false
}

// ProxyHandlerFactory returns a gin.HandlerFunc with injected BillingService and Router
func ProxyHandlerFactory(billingSvc billing.BillingService, router Router) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A. Read client request and setup Request ID
		requestID := c.Request.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		traceID := c.Request.Header.Get("X-Trace-ID")
		if traceID == "" {
			traceID = uuid.New().String()
		}
		c.Set("X-Request-ID", requestID)
		c.Writer.Header().Set("X-Request-ID", requestID)
		c.Writer.Header().Set("X-Trace-ID", traceID)

		// B. Authentication & Quota Pre-check (Already handled by middleware)
		// Extract IDs set by AuthMiddleware
		orgID, _ := c.Get("orgID")
		projectID, _ := c.Get("projectID")

		// Convert to strings
		orgIDStr, _ := orgID.(string)
		projectIDStr, _ := projectID.(string)

		bodyBytes, _ := io.ReadAll(c.Request.Body)
		if validator, ok := router.(ModelRouteValidator); ok {
			if err := validator.ValidateModelRoute(bodyBytes); err != nil {
				if strings.Contains(err.Error(), "required") {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				} else {
					c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				}
				return
			}
		}

		// 1. Route selection based on request content
		targetURL := router.Route(bodyBytes)
		if targetURL == "" {
			c.JSON(503, gin.H{"error": "No available inference backends"})
			return
		}

		// Parse request body to extract model name
		var requestBody map[string]any
		model := "unknown"
		if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
			if modelName, ok := requestBody["model"].(string); ok {
				model = modelName
			}
		} else {
			fmt.Printf("⚠️ WARN: Failed to parse request body for model name: %v\n", err)
		}

		// Create TokenStats instance for metrics tracking
		stats := NewTokenStats(model, c.Request.URL.Path)

		// B. Build the request to vLLM
		// Use the target URL selected by the router
		proxyReq, err := http.NewRequestWithContext(c.Request.Context(), "POST", targetURL, bytes.NewBuffer(bodyBytes))
		if err != nil {
			fmt.Printf("🔥 CRITICAL ERROR: %v\n", err)
			c.JSON(500, gin.H{"error": "Upstream error", "details": err.Error()})
			return
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if upstreamAPIKey := strings.TrimSpace(os.Getenv("VLLM_API_KEY")); upstreamAPIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+upstreamAPIKey)
		}
		proxyReq.Header.Set("X-Request-ID", requestID) // pass the Request ID upstream
		proxyReq.Header.Set("X-Trace-ID", traceID)     // pass the Trace ID upstream

		// C. Send the request
		client := &http.Client{
			Timeout: 300 * time.Second, // set a reasonable timeout, e.g. 300 seconds (5 minutes)
		}
		resp, err := client.Do(proxyReq)
		if err != nil {
			// Handle errors caused by a cancelled context (client disconnect or gateway timeout)
			if c.Request.Context().Err() == context.Canceled {
				fmt.Printf("👋 Client disconnected or context cancelled: %v\n", err)
				c.Status(http.StatusRequestTimeout) // return 408 Request Timeout / 499 Client Closed Request
				return
			}
			// Other upstream errors
			fmt.Printf("🔥 Upstream request failed: %v\n", err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "Upstream service error", "details": err.Error()})
			return
		}
		defer resp.Body.Close()

		// D. Handle the response
		// Set response headers uniformly
		// to avoid duplicate headers and semantic conflicts
		for header, values := range resp.Header {
			for _, value := range values {
				c.Writer.Header().Add(header, value)
			}
		}

		// Keep our own X-Request-ID header (prevent it from being overwritten upstream)
		c.Writer.Header().Set("X-Request-ID", requestID)

		// Pass through directly if the upstream returns a non-2xx status code
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.Writer.WriteHeader(resp.StatusCode)
			_, err = io.Copy(c.Writer, resp.Body)
			if err != nil {
				fmt.Printf("🔥 ERROR copying upstream error response body: %v\n", err)
			}
			return
		}

		// For successful 2xx SSE streams, add extra streaming response headers.
		// Fix SSE headers and chunked behavior to avoid setting them twice.
		if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.Header().Set("Connection", "keep-alive")
			c.Writer.Header().Set("Transfer-Encoding", "chunked") // explicitly enable chunked transfer
		}

		c.Writer.WriteHeader(resp.StatusCode) // write the upstream 2xx status code

		// E. Core loop: read the vLLM stream and write it back to the client in real time
		// Use a 32KB reader buffer to reduce syscall count
		reader := bufio.NewReaderSize(resp.Body, 32*1024)
		requestStatus := "completed"

		for {
			// Use ReadBytes('\n') so each read returns a complete line.
			// This lets PhaseSSEDataAndReport accurately detect the "data: " prefix and "usage" field.
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Printf("🔥 ERROR reading from vLLM: %v\n", err)
				}
				// Handle any trailing data (if the stream did not end with \n)
				if len(line) > 0 {
					PhaseSSEDataAndReport(line, stats)
					if _, wErr := c.Writer.Write(line); wErr != nil {
						requestStatus = "client_disconnected"
					}
					c.Writer.Flush()
				}
				break
			}

			if len(line) == 0 {
				continue
			}

			// Inspect the SSE data line and update metric statistics
			PhaseSSEDataAndReport(line, stats)

			// Write back to the client in real time
			_, err = c.Writer.Write(line)
			if err != nil {
				// Client disconnected
				requestStatus = "client_disconnected"
				break
			}
			c.Writer.Flush() // critical: flush immediately, otherwise the frontend won't see the streaming (typewriter) effect
		}

		// Stream finished, record TPOT metrics
		stats.RecordTPOT()

		// F. Report usage for billing
		// Construct UsageRecord and report it
		// tokenCount is official (vLLM usage) when available, otherwise a chunk-based estimate.
		if stats.tokenCount > 0 {
			usageSource := stats.usageSource
			if usageSource == "" {
				usageSource = "estimated"
			}
			record := billing.UsageRecord{
				RequestID:        requestID,
				TraceID:          traceID,
				Model:            stats.model,
				User:             "anonymous", // Placeholder for future user tracking
				OrgID:            orgIDStr,    // From context (set by middleware)
				ProjectID:        projectIDStr,
				PromptTokens:     stats.promptTokens,
				CompletionTokens: stats.completionTokens,
				TotalTokens:      stats.tokenCount,
				UsageSource:      usageSource,
				RequestStatus:    requestStatus,
				Timestamp:        time.Now(),
			}
			// Non-blocking call, won't affect HTTP response time
			if err := billingSvc.ReportUsage(record); err != nil {
				// Log billing errors but don't fail the request
				// The user already got their response
				if err != billing.ErrAlreadyProcessed {
					fmt.Printf("⚠️ [BILLING] Failed to report usage: %v\n", err)
				}
			}
		}
	}
}

// 💡 HealthCheckHandler uses *gin.Context.
func HealthCheckHandler(c *gin.Context) { // note: the parameter is now c *gin.Context
	// In Gin we no longer use w http.ResponseWriter and r *http.Request directly.
	// They are accessed via c.Writer and c.Request, but usually there is no need to manipulate them directly.

	// Use Gin's recommended c.String() or c.JSON() methods to return a response.
	// They set the status code and response headers automatically.
	c.String(http.StatusOK, "Status: OK")

	// To return JSON:
	// c.JSON(http.StatusOK, gin.H{"status": "ok"})

	// log.Println("Health check accessed.")
	// Note: Gin ships with a Logger middleware by default, so logging is handled automatically.
}
