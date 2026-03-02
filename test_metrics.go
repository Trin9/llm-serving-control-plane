// Load test tool: matches curl request format, for Grafana TTFT/TPOT/QPS/GPU metrics
// Usage: export TOKEN=your-key; go run test_metrics.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ChatCompletionRequest matches curl format
type ChatCompletionRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	MaxTokens  int       `json:"max_tokens"`
	Stream     bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func fetchMetrics(url string) {
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Error fetching metrics: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading metrics: %v\n", err)
		return
	}

	metrics := string(body)
	for _, key := range []string{"ai_ttft_seconds", "ai_tpot_seconds"} {
		if strings.Contains(metrics, key) {
			fmt.Printf("[OK] Found %s\n", key)
		} else {
			fmt.Printf("[--] %s not found\n", key)
		}
	}

	lines := strings.Split(metrics, "\n")
	for _, line := range lines {
		if strings.Contains(line, "ai_ttft_seconds") || strings.Contains(line, "ai_tpot_seconds") {
			fmt.Println("  ", strings.TrimSpace(line))
		}
	}
}

func main() {
	gateServiceURL := getEnv("GATE_URL", "http://localhost:8080/v1/chat/completions")
	metricsURL := getEnv("METRICS_URL", "http://localhost:8080/metrics")
	token := getEnv("TOKEN", "your-vllm-api-key")
	numWorkers := getEnvInt("WORKERS", 5)

	prompts := []string{
		"Introduce Grafana",
		"What is Prometheus?",
		"Explain vLLM continuous batching",
	}

	fmt.Println("=== AI Metrics Load Test ===")
	fmt.Printf("Gate URL: %s\n", gateServiceURL)
	fmt.Printf("Workers: %d\n", numWorkers)
	fmt.Printf("Model: Qwen1.5-4B-Chat, max_tokens: 500, stream: true\n\n")

	fmt.Println("Initial metrics:")
	fetchMetrics(metricsURL)

	var wg sync.WaitGroup
	fmt.Printf("\nSending %d concurrent requests...\n", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sendOne(gateServiceURL, token, id, prompts)
		}(i)
		time.Sleep(100 * time.Millisecond)
	}
	wg.Wait()

	time.Sleep(2 * time.Second)
	fmt.Println("\nMetrics after requests:")
	fetchMetrics(metricsURL)
	fmt.Println("\nDone! Check Grafana for TTFT/TPOT/QPS/GPU curves.")
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return defaultVal
}

func sendOne(url, token string, workerID int, prompts []string) {
	content := "Introduce Grafana"
	if len(prompts) > 0 {
		content = prompts[workerID%len(prompts)]
	}

	reqBody := ChatCompletionRequest{
		Model:     "Qwen1.5-4B-Chat",
		MaxTokens: 500,
		Stream:    true,
		Messages:  []Message{Message{Role: "user", Content: content}},
	}
	jsonData, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Worker %d: %v\n", workerID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("Worker %d: status %d %s\n", workerID, resp.StatusCode, string(body))
		return
	}

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), "[DONE]") {
			break
		}
		if err != nil {
			break
		}
	}
	fmt.Printf("Worker %d: Completed\n", workerID)
}

