# Monitoring Stack Quick Start Guide

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ gate-service│ ──► │ Prometheus  │ ──► │   Grafana   │
│  :8080      │     │   :9090     │     │   :3000     │
│  /metrics   │     │  (scrape)   │     │  (dashboard)│
└─────────────┘     └─────────────┘     └─────────────┘
```

## Startup Steps

### 1. Start Prometheus

```bash
# Start Prometheus with Docker
docker run -d \
  --name prometheus \
  -p 9090:9090 \
  -v $(pwd)/monitor/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml \
  prom/prometheus:latest
```

### 2. Start Grafana

```bash
# Start Grafana with Docker
docker run -d \
  --name grafana \
  -p 3000:3000 \
  grafana/grafana:latest
```

### 3. Configure the Grafana data source

1. Open a browser and visit `http://localhost:3000`
2. Log in (default credentials `admin` / `admin`)
3. Go to **Configuration** → **Data sources** → **Add data source**
4. Select **Prometheus**
5. Set URL: `http://prometheus:9090` (if using Docker Compose) or `http://localhost:9090` (if running directly)
6. Click **Save & test**

### 4. Import the Dashboard

1. Click **Dashboards** → **New** → **Import**
2. Upload `monitor/grafana/ai-performance-dashboard.json`
3. Select the Prometheus data source
4. Click **Import**

## One-click startup with Docker Compose

```bash
# Start all services
docker-compose -f monitor/docker-compose.yml up -d

# View logs
docker-compose -f monitor/docker-compose.yml logs -f
```

## Verify the data flow

1. **Check gate-service metrics**:
   ```bash
   curl http://localhost:8080/metrics | grep ai_
   ```

2. **Check Prometheus scraping**:
   - Visit `http://localhost:9090/targets`
   - Confirm the `gate-service` status is **UP**

3. **Query in Grafana**:
   - Go to the **Explore** page
   - Enter the PromQL: `ai_ttft_seconds_sum`
   - You should see data

## Common PromQL queries

```promql
# TTFT average
rate(ai_ttft_seconds_sum[5m]) / rate(ai_ttft_seconds_count[5m])

# TTFT P95
histogram_quantile(0.95, rate(ai_ttft_seconds_bucket[5m]))

# TPOT average
rate(ai_tpot_seconds_sum[5m]) / rate(ai_tpot_seconds_count[5m])

# QPS
rate(http_requests_total[1m])
```

## Stop the services

```bash
docker stop prometheus grafana
docker rm prometheus grafana