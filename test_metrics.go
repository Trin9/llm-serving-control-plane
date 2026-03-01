package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ChatCompletionRequest represents the request structure for chat completion
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func sendTestRequest(url string, wg *sync.WaitGroup, workerID int) {
	defer wg.Done()

	request := ChatCompletionRequest{
		Model: "Qwen1.5-4B-Chat",
		Messages: []Message{
			{
				Role:    "user",
				Content: fmt.Sprintf("Hello, this is test message from worker %d", workerID),
			},
		},
		Stream: true,
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		fmt.Printf("Worker %d: Error marshaling JSON: %v\n", workerID, err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Worker %d: Error creating request: %v\n", workerID, err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer your-vllm-api-key")

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Worker %d: Error sending request: %v\n", workerID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Worker %d: Non-OK status code: %d\n", workerID, resp.StatusCode)
		return
	}

	// Read the streaming response
	reader := resp.Body
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			if strings.Contains(data, "[DONE]") {
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Printf("Worker %d: Error reading response: %v\n", workerID, err)
			break
		}
	}

	fmt.Printf("Worker %d: Completed request\n", workerID)
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

	// Check for our custom metrics
	if strings.Contains(metrics, "ai_ttft_seconds") {
		fmt.Println("✓ Found ai_ttft_seconds metric")
	} else {
		fmt.Println("✗ ai_ttft_seconds metric not found")
	}

	if strings.Contains(metrics, "ai_tpot_seconds") {
		fmt.Println("✓ Found ai_tpot_seconds metric")
	} else {
		fmt.Println("✗ ai_tpot_seconds metric not found")
	}

	// Print sample lines for verification
	lines := strings.Split(metrics, "\n")
	for _, line := range lines {
		if strings.Contains(line, "ai_ttft_seconds") || strings.Contains(line, "ai_tpot_seconds") {
			fmt.Println("Sample metric line:", line)
		}
	}
}

func main() {
	gateServiceURL := "http://localhost:8080/v1/chat/completions"
	metricsURL := "http://localhost:8080/metrics"

	fmt.Println("Starting AI metrics test...")

	// Wait a bit to see initial metrics
	time.Sleep(2 * time.Second)
	fmt.Println("\nInitial metrics:")
	fetchMetrics(metricsURL)

	// Send concurrent requests to generate metrics
	var wg sync.WaitGroup
	numWorkers := 3

	fmt.Printf("\nSending %d concurrent requests...\n", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go sendTestRequest(gateServiceURL, &wg, i)
		time.Sleep(100 * time.Millisecond) // Stagger requests
	}

	wg.Wait()

	// Wait for metrics to be processed
	time.Sleep(2 * time.Second)

	fmt.Println("\nFinal metrics after requests:")
	fetchMetrics(metricsURL)

	fmt.Println("\nTest completed!")
}
