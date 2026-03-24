# LLM Serving: Local and Kubernetes Deployment Guide

This guide explains how to set up and test the LLM Serving stack in both local development (Docker Compose) and Kubernetes (Helm) environments.

## Prerequisites

*   **Local Mode:** Docker and Docker Compose installed.
*   **Kubernetes Mode:** `kubectl` and `helm` CLI installed, and a configured Kubernetes context (e.g., Kind, Minikube, or cloud provider).
*   **GPU Access:** For tests involving GPU metrics, ensure your environment has access to NVIDIA GPUs and drivers.
*   **Go Environment:** For building `gate-service` if needed locally.

## Project Structure Overview

*   `scripts/`: Contains general lifecycle scripts (`all-service-start.sh`, `all-service-stop.sh`) that detect the environment and orchestrate deployments.
*   `helm/monitoring-stack/`: Helm chart for deploying Prometheus and Grafana in Kubernetes.
*   `helm/llm-operator/`: Helm chart for deploying the LLM Operator and Gate Service in Kubernetes.
*   `docker-compose.yml`: Defines the local development environment stack (mock-vllm, gate-service, prometheus, grafana).
*   `test/`: Contains environment-specific test scripts and configuration.
    *   `start-local.sh`, `stop-local.sh`: For Docker Compose local development.
    *   `start-k8s.sh`, `stop-k8s.sh`: For Kubernetes Helm deployments.
    *   `run-stress-test.sh`: Standardized script for load testing.
    *   `stress-test-body.json`: Request payload for stress tests.
*   `monitor/grafana/llm-serving-monitor.json`: The canonical Grafana dashboard JSON.

## 1. Local Development (Docker Compose)

This mode is ideal for rapid local testing and development without a Kubernetes cluster.

### 1.1. Starting the Local Environment

1.  **Ensure Docker and Docker Compose are installed.**
2.  **Navigate to the project root directory** in your terminal.
3.  **Run the local startup script:**
    ```bash
    ./scripts/all-service-start.sh
    ```
    *This script will detect the local environment and use `docker-compose up -d`.* 
    *It will also attempt to start `dcgm-exporter` manually if not in Docker Compose and if found in PATH.* 

### 1.2. Accessing Services Locally

*   **vLLM (Mock):** `http://localhost:8000`
*   **Gate Service:** `http://localhost:8080`
    *   Metrics: `http://localhost:8080/metrics`
*   **Prometheus:** `http://localhost:9090`
*   **Grafana:** `http://localhost:3000` (Login with `admin`/`admin`)

### 1.3. Verifying Functionality

1.  **Send a test request to the Gate Service:**
    ```bash
    curl -X POST http://localhost:8080/v1/chat/completions 
      -H "Content-Type: application/json" 
      -d @test/stress-test-body.json
    ```
    You should receive a streamed response.

2.  **Check Metrics in Prometheus:**
    *   Access `http://localhost:9090/targets`.
    *   Ensure `gate-service` and `prometheus` jobs are in an `UP` state.
    *   In Prometheus UI, query for metrics like `ai_ttft_seconds` or `http_requests_total`.

3.  **Verify Dashboard in Grafana:**
    *   Access `http://localhost:3000`.
    *   Log in with `admin`/`admin`.
    *   Navigate to the dashboard named "LLM Serving Monitor (AI + Service + GPU)".
    *   Ensure all panels are displaying data (you may need to send requests to `gate-service` first).

### 1.4. Stopping the Local Environment

1.  **Run the local shutdown script:**
    ```bash
    ./scripts/all-service-stop.sh
    ```
    *This script will detect the local environment and use `docker-compose down`.* 

