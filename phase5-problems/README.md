# Phase 5 问题记录与修复建议

本文档记录当前架构中存在的三个核心设计问题，以及对应的修复建议。

---

## 问题 1：vLLM Service URL 断层（gate-service 无法自动发现后端）

### 问题描述

Controller 在 `reconcileService` / `updateStatus` 完成后，会将 Service 的集群内 DNS 地址写入 CR 的 Status 字段：

```go
// operator/internal/controller/inferenceservice_controller.go
serviceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000",
    inferSvc.Name,
    inferSvc.Namespace,
)
inferSvc.Status.URL = serviceURL
```

`<service-name>.<namespace>.svc.cluster.local` 是 K8s CoreDNS 为每个 Service 自动注册的内部域名，仅在集群内部有效。

但 gate-service 目前通过**硬编码的环境变量**获取后端地址，并没有去读取 CR 的 `Status.URL`：

```go
// app/cmd/main.go
vllmURLs := os.Getenv("VLLM_URLS")
if vllmURLs == "" {
    vllmURLs = "http://localhost:8000/v1/chat/completions"  // 硬编码 fallback
}
backendList := strings.Split(vllmURLs, ",")
routerSvc := handler.NewConsistentHashRouter(backendList)
```

**结果**：Controller 知道有哪些 InferenceService 及其 URL，gate-service 却完全不知道，两者之间存在信息断层，必须靠人工维护环境变量来打通。

### 影响

- 新增 InferenceService 后，需要手动更新 gate-service 的 `VLLM_URLS` 环境变量并重启 Pod，无法实现动态服务发现
- 删除 InferenceService 后，gate-service 仍会持有失效的后端地址，导致持续 502 错误
- 完全无法实现多模型自动路由（需人工维护"模型名 → URL"的映射）

### 修复建议

gate-service 启动时，通过 K8s `controller-runtime` 或 `client-go` 的 Informer 机制 Watch 集群内所有 `InferenceService` 对象，当 CR 的 `Status.URL` 非空（即 Controller 已完成 Deployment + Service 创建）时，自动将该 URL 加入路由后端列表；CR 删除时自动移除。

```
gate-service 启动
  → List + Watch InferenceService 对象（所有 namespace 或指定 namespace）
  → 对每个 Status.URL 非空的 CR，将其 URL 注册进 ConsistentHashRouter
  → CR 新增/删除/Status 变更时，动态更新路由表
```

核心代码改造点：
1. gate-service 引入 `sigs.k8s.io/controller-runtime` 或 `k8s.io/client-go`
2. 在 `main.go` 中初始化 K8s InCluster 配置（`rest.InClusterConfig()`）
3. 将路由表更新逻辑抽象为 `BackendRegistry`，支持并发安全的增删操作

---

## 问题 2：架构、扩容与路由的设计混淆

### 问题描述

当前系统存在两种扩容方式，但代码中没有明确区分，导致路由层的行为不可预期。

#### 两种扩容方式的本质差异

**方式 A：单 InferenceService 扩副本（replicas 增加）**

```
InferenceService: qwen
  spec.replicas: 2 → 4
  Service: qwen.default.svc.cluster.local  ← 地址不变
  Pod1, Pod2, Pod3, Pod4                   ← K8s Service 随机负载均衡
```

- gate-service 路由表**无需更新**，Service 地址不变
- K8s kube-proxy 自动将请求分散到 4 个 Pod
- **语义路由完全失效**：相同 Prompt 前缀的请求会被随机分到不同 Pod，KV Cache 无法复用

**方式 B：创建多个同模型 InferenceService**

```
InferenceService: qwen-instance-1  → Service: qwen-instance-1.default.svc...
InferenceService: qwen-instance-2  → Service: qwen-instance-2.default.svc...
InferenceService: qwen-instance-3  → Service: qwen-instance-3.default.svc...
```

- gate-service 路由表**必须更新**，新增了 3 个独立后端地址
- 一致性哈希可以将相同 Prompt 前缀的请求稳定路由到同一个 InferenceService 实例
- **语义路由有意义**：KV Cache 命中率可以有效提升

#### 当前代码的实际架构

```
外部请求
    │
gate-service pod（1个或多个，无状态水平扩容）
    │
    └── VLLM_URLS 环境变量（静态配置）
         │
         └── qwen.default.svc.cluster.local:8000  ← 一个 Service，背后多个 Pod 随机路由
```

这个架构下，一致性哈希路由的 KV Cache 收益为零。

### 正确的多模型分发架构

```
外部请求（携带 model 字段）
    │
gate-service（多副本，无状态）
    │
    ├── model=qwen    → qwen-instance-1, qwen-instance-2（一致性哈希选一个）
    └── model=deepseek → deepseek-instance-1, deepseek-instance-2（一致性哈希选一个）
```

### 修复建议

1. **明确扩容策略**：语义路由 + KV Cache 优化场景下，应采用**方式 B**（多 InferenceService 实例），而非单纯扩 replicas
2. **按模型名分组路由**：路由表从"单一 URL 池"改为"模型名 → URL 池"的 Map 结构
3. **自动服务发现联动**：结合问题 1 的修复，Watch InferenceService 时按 `spec.modelName` 字段分组注册到对应的路由池

```go
// 改造后的路由表结构（伪代码）
type ModelRouter struct {
    // key: modelName (如 "Qwen2.5-7B-Instruct")
    // value: 该模型的所有 InferenceService URL 列表
    pools map[string]*ConsistentHashRouter
    mu    sync.RWMutex
}

func (r *ModelRouter) Route(reqBody []byte) string {
    model := extractModelName(reqBody)
    r.mu.RLock()
    pool, ok := r.pools[model]
    r.mu.RUnlock()
    if !ok {
        return ""  // 没有对应模型的后端
    }
    return pool.Route(reqBody)
}
```

---

## 问题 3：一致性哈希应采用 Pod IP 而非 Service 地址

### 问题描述

当前 `ConsistentHashRouter` 的后端节点是 **K8s Service 地址**（`xxx.svc.cluster.local`）：

```go
// app/cmd/main.go
vllmURLs := os.Getenv("VLLM_URLS")
backendList := strings.Split(vllmURLs, ",")
routerSvc := handler.NewConsistentHashRouter(backendList)
// backendList 示例: ["http://qwen-inst-1.default.svc.cluster.local:8000"]
```

**K8s Service 本质上是一个带随机负载均衡的 VIP**（虚拟 IP），kube-proxy 会将到达 Service 的请求随机分发到背后的任意 Pod。

因此，即使 `ConsistentHashRouter` 将相同 Prompt 前缀的请求路由到了同一个 Service 地址，该请求仍然会被 K8s 随机分配到不同的 Pod，**KV Cache 的 Prefix Cache 命中率无法提升**。

### 语义路由有效的前提条件

一致性哈希路由要真正发挥 KV Cache 优化效果，必须满足：

> **相同 Prompt 前缀的请求 → 稳定路由到同一个 Pod**

这要求路由层直接使用 **Pod IP** 作为哈希环的节点，绕过 K8s Service 的随机负载均衡层。

### 当前 vs 目标对比

```
当前（无效）：
  请求 → ConsistentHashRouter → Service VIP → kube-proxy → 随机 Pod
                                                            ↑ KV Cache 无法命中

目标（有效）：
  请求 → ConsistentHashRouter → 直接访问 Pod IP → 固定 Pod
                                                   ↑ KV Cache 高命中率
```

### 修复建议

将路由后端列表从 Service 地址改为动态获取的 **Pod IP 列表**，通过 Watch K8s `Endpoints`（或 `EndpointSlices`）对象实现：

```
Watch InferenceService 对应的 Endpoints 对象
  → Pod 就绪时：将新 Pod IP 加入哈希环
  → Pod 终止时：从哈希环移除对应 Pod IP
  → 路由时：直接向 Pod IP:8000 发送请求，不经过 Service
```

核心代码改造点：

```go
// 改造后的后端发现逻辑（伪代码）
func watchEndpoints(modelName, namespace, serviceName string, router *ConsistentHashRouter) {
    // Watch Endpoints 对象
    informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            endpoints := obj.(*corev1.Endpoints)
            podIPs := extractReadyPodIPs(endpoints) // 只取 Ready 状态的 Pod
            urls := toPodURLs(podIPs, 8000)
            router.UpdateBackends(urls)
        },
        UpdateFunc: func(old, new interface{}) {
            // 同 AddFunc
        },
        DeleteFunc: func(obj interface{}) {
            router.UpdateBackends([]string{})
        },
    })
}

func extractReadyPodIPs(endpoints *corev1.Endpoints) []string {
    var ips []string
    for _, subset := range endpoints.Subsets {
        for _, addr := range subset.Addresses { // Addresses = Ready 的 Pod
            ips = append(ips, addr.IP)
        }
    }
    return ips
}
```

### 参考实现

- **llm-d（Red Hat）**：通过 `epp`（Endpoint Picker Plugin）实现 KV Cache 感知的 Pod 级别路由，是目前业界最接近此设计的开源实现
- **KServe**：通过 `InferenceGraph` + `Knative` 实现请求级别的精细路由

### 注意事项

- 直连 Pod IP 需要 gate-service 与 vLLM Pod 在同一 K8s 网络（通常满足）
- Pod IP 在 Pod 重启后会变化，必须依赖 Endpoints Watch 动态更新，不能静态配置
- 需要处理 Pod 正在终止（Terminating）但 IP 尚未从 Endpoints 移除的窗口期，可通过检查 `subset.NotReadyAddresses` 排除

---

## 问题汇总

| # | 问题 | 严重程度 | 影响范围 |
|---|------|----------|----------|
| 1 | gate-service 无法自动发现 InferenceService 后端 | 高 | 运维效率、多模型支持 |
| 2 | 扩容方式混淆导致架构语义不清晰 | 中 | 路由有效性、KV Cache 收益 |
| 3 | 一致性哈希节点为 Service VIP 而非 Pod IP，语义路由实际无效 | 高 | KV Cache 命中率、路由核心价值 |

三个问题存在依赖关系，修复优先级建议：**问题 1 → 问题 2 → 问题 3**（服务发现是路由优化的前提）。
