# AI 指标采集验证指南

## 概述

此项目已成功实现 AI 核心指标采集功能，包括：

- **TTFT (Time To First Token)**: 首字延迟，从请求开始到收到第一个 token 的时间
- **TPOT (Time Per Output Token)**: 单 token 生成速度，生成每个后续 token 的平均耗时

## 实现详情

### 1. 指标定义 (`app/monitor/const.go`)

新增了两个 Prometheus HistogramVec 指标：

```go
// TTFT - 首字延迟
AITimeToFirstToken = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Name:    "ai_ttft_seconds",
        Help:    "Time to first token in seconds",
        Buckets: []float64{0.1, 0.5, 1, 2, 5, 10},  // 适配 AI 场景
    },
    []string{"model", "route"},  // 标签：模型名、路由
)

// TPOT - 单token生成速度
AITimePerOutputToken = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Name:    "ai_tpot_seconds",
        Help:    "Time per output token in seconds",
        Buckets: []float64{0.01, 0.05, 0.1, 0.2, 0.5},  // 更细粒度
    },
    []string{"model", "route"},
)
```

### 2. 埋点逻辑 (`app/handler/proxy_handler.go`)

在 SSE 流式循环中实现了精确的时间测量：

- **Start Time**: 发起 vLLM 请求前记录
- **First Token Time**: 读取到流的第一个有效数据时记录，计算 TTFT
- **Token Counting**: 统计有效 token chunk 数量
- **End Time**: 流结束时记录，计算 TPOT

使用了独立的 `TokenStats` 结构体来管理指标收集逻辑，保持代码整洁。

### 3. 代码重构改进

- **SSE数据解析函数**: 将SSE数据分析逻辑提取到独立的 `isSSEDataLine()` 函数中，提高代码可读性和维护性
- **使用CutPrefix**: 使用 `strings.Cut()` 替代 `strings.HasPrefix()` + `strings.TrimPrefix()`，提高性能
- **类型改进**: 使用 `any` 类型替代 `interface{}`，符合Go语言最佳实践

## 验证方法

### 1. 启动服务

```bash
go run app/cmd/main.go
```

### 2. 发送测试请求

发送流式请求以触发指标收集：

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-vllm-api-key" \
  -d '{
    "model": "Qwen1.5-4B-Chat",
    "messages": [
      {
        "role": "user",
        "content": "Hello, how are you?"
      }
    ],
    "stream": true
  }'
```

### 3. 查看指标

访问指标端点验证数据：

```bash
curl http://localhost:8080/metrics | grep ai_
```

预期输出示例：
```
# HELP ai_ttft_seconds Time to first token in seconds
# TYPE ai_ttft_seconds histogram
ai_ttft_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="0.1"} 0
ai_ttft_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="0.5"} 1
ai_ttft_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="1"} 1
...

# HELP ai_tpot_seconds Time per output token in seconds
# TYPE ai_tpot_seconds histogram
ai_tpot_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="0.01"} 0
ai_tpot_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="0.05"} 5
ai_tpot_seconds_bucket{model="Qwen1.5-4B-Chat",route="/v1/chat/completions",le="0.1"} 10
...
```

## 技术特点

1. **精确测量**: 在 SSE 流的适当位置进行时间戳记录
2. **标签化**: 使用 `model` 和 `route` 标签进行多维度分析
3. **性能优化**: 使用专用结构体管理指标收集，避免在主逻辑中混杂指标代码
4. **错误处理**: 适当处理流中断等异常情况
5. **兼容性**: 与标准 SSE 格式的 vLLM 响应兼容
6. **代码分离**: 将SSE数据解析逻辑独立成函数，提高代码可读性
7. **现代化语法**: 使用Go最新特性如`any`类型和`strings.Cut`函数

## 验证脚本

项目中包含了 `test_metrics.go` 验证脚本，可用于批量测试指标收集功能。
