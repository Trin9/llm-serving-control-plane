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
2.  **Navigate to the project root directory** (`llm-serving-control-plane/`) in your terminal.
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
    *   Ensure `gate-service` is in an `UP` state. (Note: `dcgm-exporter` may not be scraped if not running locally).
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

## 2. Kubernetes Deployment (Helm)

This mode is for deploying to a Kubernetes cluster.

### 2.1. Prerequisites for Kubernetes Mode

*   **`kubectl` configured** to point to your Kubernetes cluster.
*   **`helm` CLI installed**.
*   **Ensure Helm repositories are updated:**
    ```bash
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm repo add grafana https://grafana.github.io/helm-charts
    helm repo add nvdp https://nvidia.github.io/helm-charts # For DCGM exporter
    helm repo update
    ```

### 2.2. Starting the Kubernetes Environment

1.  **Navigate to the project root directory** (`llm-serving-control-plane/`) in your terminal.
2.  **Run the K8s startup script:**
    ```bash
    ./scripts/all-service-start.sh
    ```
    *This script will detect the Kubernetes environment and use `helm upgrade --install` for `llm-operator` and `monitoring-stack`. It will also attempt to deploy `nvidia-dcgm-exporter` via Helm.* 

### 2.3. Accessing Services in Kubernetes

Services are typically accessed via `kubectl port-forward` or Ingress.

*   **Grafana:**
    ```bash
    kubectl port-forward service/monitoring-stack-grafana 3000:80
    ```
    Access Grafana at `http://localhost:3000` (Login: `admin`/`admin`).

*   **Prometheus:**
    ```bash
    kubectl port-forward service/monitoring-stack-prometheus-server 9090:9090
    ```
    Access Prometheus at `http://localhost:9090`.

*   **Gate Service Metrics:**
    ```bash
    kubectl port-forward service/llm-operator-gate-service 8080:8080
    ```
    Access metrics at `http://localhost:8080/metrics`.

### 2.4. Verifying Functionality in Kubernetes

1.  **Check Pod Status:**
    ```bash
    kubectl get pods
    ```
    Ensure all pods related to `llm-operator`, `gate-service`, `prometheus`, and `grafana` are in `Running` state.

2.  **Send a test request to the Gate Service:**
    *   Ensure `kubectl port-forward` for the gate service is running.
    *   Run the following command:
        ```bash
        curl -X POST http://localhost:8080/v1/chat/completions 
          -H "Content-Type: application/json" 
          -d @test/stress-test-body.json
        ```
    You should receive a streamed response.

3.  **Check Metrics in Prometheus:**
    *   Access `http://localhost:9090/targets`.
    *   Confirm `gate-service` and `dcgm-exporter` jobs are `UP`.
    *   In Prometheus UI, query for metrics like `ai_ttft_seconds` or `http_requests_total`.

4.  **Verify Dashboard in Grafana:**
    *   Access Grafana via `kubectl port-forward` (as described above).
    *   Log in with `admin`/`admin`.
    *   Navigate to the dashboard named "LLM Serving Monitor (AI + Service + GPU)".
    *   Ensure all panels are displaying data.

### 2.5. Stopping the Kubernetes Environment

1.  **Run the K8s shutdown script:**
    ```bash
    ./scripts/all-service-stop.sh
    ```
    *This script will detect the Kubernetes environment and use `helm uninstall` for the deployed releases.* 

## 3. Stress Testing

Use the `run-stress-test.sh` script for load testing. It supports both `ghz` (preferred) and a `curl`-based fallback.

### 3.1. Running Stress Tests

*   **Local Mode (Docker Compose):**
    ```bash
    ./scripts/run-stress-test.sh -c 10 -n 100 -u http://localhost:8080/v1/chat/completions
    ```

*   **Kubernetes Mode:**
    *   Ensure your Kubernetes services are running and accessible (e.g., via `kubectl port-forward`).
    *   Use the appropriate URL for your Kubernetes deployment:
        ```bash
        # Example for a service exposed via port-forward
        ./scripts/run-stress-test.sh -c 10 -n 100 -u http://localhost:8080/v1/chat/completions 
        ```

### 3.2. Test Payload

The request body for stress tests is defined in `test/stress-test-body.json`.

### 3.3. Verifying Results

*   **`run-stress-test.sh` Output:** Provides basic statistics like QPS and total time.
*   **Prometheus:** Check metrics like `http_requests_total`, `ai_ttft_seconds_bucket`, `ai_tpot_seconds_bucket` for detailed performance data.
*   **Grafana:** Observe the dashboard for trends in TTFT, TPOT, QPS, Latency, and GPU utilization during the test.
