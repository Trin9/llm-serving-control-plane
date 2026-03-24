# 监控栈快速启动指南

## 架构说明

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│ gate-service│ ──► │ Prometheus  │ ──► │   Grafana   │
│  :8080      │     │   :9090     │     │   :3000     │
│  /metrics   │     │  (scrape)   │     │  (dashboard)│
└─────────────┘     └─────────────┘     └─────────────┘
```

## 启动步骤

### 1. 启动 Prometheus

```bash
# 使用 Docker 启动 Prometheus
docker run -d \
  --name prometheus \
  -p 9090:9090 \
  -v $(pwd)/monitor/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml \
  prom/prometheus:latest
```

### 2. 启动 Grafana

```bash
# 使用 Docker 启动 Grafana
docker run -d \
  --name grafana \
  -p 3000:3000 \
  grafana/grafana:latest
```

### 3. 配置 Grafana 数据源

1. 打开浏览器访问 `http://localhost:3000`
2. 登录 (默认账号 `admin` / `admin`)
3. 进入 **Configuration** → **Data sources** → **Add data source**
4. 选择 **Prometheus**
5. 设置 URL: `http://prometheus:9090` (如果用 Docker Compose) 或 `http://localhost:9090` (如果直接运行)
6. 点击 **Save & test**

### 4. 导入 Dashboard

1. 点击 **Dashboards** → **New** → **Import**
2. 上传 `monitor/grafana/ai-performance-dashboard.json`
3. 选择 Prometheus 数据源
4. 点击 **Import**

## 使用 Docker Compose 一键启动

```bash
# 启动所有服务
docker-compose -f monitor/docker-compose.yml up -d

# 查看日志
docker-compose -f monitor/docker-compose.yml logs -f
```

## 验证数据流

1. **检查 gate-service 指标**:
   ```bash
   curl http://localhost:8080/metrics | grep ai_
   ```

2. **检查 Prometheus 抓取**:
   - 访问 `http://localhost:9090/targets`
   - 确认 `gate-service` 状态为 **UP**

3. **在 Grafana 中查询**:
   - 进入 **Explore** 页面
   - 输入 PromQL: `ai_ttft_seconds_sum`
   - 应该能看到数据

## 常用 PromQL 查询

```promql
# TTFT 平均值
rate(ai_ttft_seconds_sum[5m]) / rate(ai_ttft_seconds_count[5m])

# TTFT P95
histogram_quantile(0.95, rate(ai_ttft_seconds_bucket[5m]))

# TPOT 平均值
rate(ai_tpot_seconds_sum[5m]) / rate(ai_tpot_seconds_count[5m])

# QPS
rate(http_requests_total[1m])
```

## 停止服务

```bash
docker stop prometheus grafana
docker rm prometheus grafana