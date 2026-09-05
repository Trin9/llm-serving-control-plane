package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	servingv1 "github.com/trin/llm-serving-control-plane/operator/api/v1"
)

// 定义 Finalizer 名称
const finalizerName = "serving.trin.io/finalizer"

// InferenceServiceReconciler reconciles a InferenceService object
type InferenceServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. 获取 InferenceService CR
	var inferSvc servingv1.InferenceService
	if err := r.Get(ctx, req.NamespacedName, &inferSvc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("InferenceService not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	// 2.检查是否正在删除 (DeletionTimestamp 不为空)
	if !inferSvc.ObjectMeta.DeletionTimestamp.IsZero() {
		// 如果有我们的 Finalizer，说明需要执行清理
		if controllerutil.ContainsFinalizer(&inferSvc, finalizerName) {
			logger.Info("Executing finalizer cleanup...")

			// TODO: 在这里执行外部资源清理 (如 AWS 资源、DNS 等)
			// 本阶段暂时留空，因为 K8s 资源会自动级联删除

			// 移除 Finalizer，允许 K8s 删除对象
			controllerutil.RemoveFinalizer(&inferSvc, finalizerName)
			if err := r.Update(ctx, &inferSvc); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("Finalizer removed, object can be deleted now")
		}
		// 停止 Reconcile
		return ctrl.Result{}, nil
	}

	// 如果没被删除，确保 Finalizer 存在
	if !controllerutil.ContainsFinalizer(&inferSvc, finalizerName) {
		controllerutil.AddFinalizer(&inferSvc, finalizerName)
		if err := r.Update(ctx, &inferSvc); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("Reconciling InferenceService", "name", inferSvc.Name, "namespace", inferSvc.Namespace)

	// 3. 同步 Deployment
	if err := r.reconcileDeployment(ctx, &inferSvc); err != nil {
		logger.Error(err, "Failed to reconcile Deployment")
		_ = r.updateLifecycleStatus(ctx, &inferSvc, lifecycleState{
			available:   metav1.ConditionFalse,
			ready:       metav1.ConditionFalse,
			progressing: metav1.ConditionFalse,
			degraded:    metav1.ConditionTrue,
			unknown:     metav1.ConditionFalse,
			reason:      "DeploymentFailed",
			message:     err.Error(),
		})
		return ctrl.Result{}, err
	}

	// 4. 同步 Service
	if err := r.reconcileService(ctx, &inferSvc); err != nil {
		logger.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	// 5. 更新 Status (根据真实状态)
	if err := r.updateStatus(ctx, &inferSvc); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled InferenceService")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil // 定期 Requeue 以检查状态变化
}

// reconcileDeployment 确保 Deployment 存在且与 CR 期望状态一致
func (r *InferenceServiceReconciler) reconcileDeployment(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	logger := log.FromContext(ctx)

	// 构建期望的 Deployment
	deployment := r.buildDeployment(inferSvc)

	// 设置 Owner Reference，实现级联删除
	if err := ctrl.SetControllerReference(inferSvc, deployment, r.Scheme); err != nil {
		return err
	}

	// 检查 Deployment 是否已存在
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(deployment), &existing)

	if err != nil && errors.IsNotFound(err) {
		// 不存在，创建新的 Deployment
		logger.Info("Creating Deployment", "name", deployment.Name)
		return r.Create(ctx, deployment)
	} else if err != nil {
		return err
	}

	changed := !reflect.DeepEqual(existing.Labels, deployment.Labels) ||
		!reflect.DeepEqual(existing.Spec.Replicas, deployment.Spec.Replicas) ||
		!reflect.DeepEqual(existing.Spec.Strategy, deployment.Spec.Strategy) ||
		!reflect.DeepEqual(existing.Spec.Template.Labels, deployment.Spec.Template.Labels) ||
		!reflect.DeepEqual(existing.Spec.Template.Spec.Containers, deployment.Spec.Template.Spec.Containers) ||
		!reflect.DeepEqual(existing.Spec.Template.Spec.NodeSelector, deployment.Spec.Template.Spec.NodeSelector) ||
		!reflect.DeepEqual(existing.Spec.Template.Spec.Tolerations, deployment.Spec.Template.Spec.Tolerations) ||
		!reflect.DeepEqual(existing.Spec.Template.Spec.Affinity, deployment.Spec.Template.Spec.Affinity)
	if !changed {
		return nil
	}

	existing.Labels = deployment.Labels
	existing.Spec.Replicas = deployment.Spec.Replicas
	existing.Spec.Strategy = deployment.Spec.Strategy
	existing.Spec.Template.Labels = deployment.Spec.Template.Labels
	existing.Spec.Template.Spec.Containers = deployment.Spec.Template.Spec.Containers
	existing.Spec.Template.Spec.NodeSelector = deployment.Spec.Template.Spec.NodeSelector
	existing.Spec.Template.Spec.Tolerations = deployment.Spec.Template.Spec.Tolerations
	existing.Spec.Template.Spec.Affinity = deployment.Spec.Template.Spec.Affinity

	logger.Info("Updating Deployment", "name", deployment.Name)
	return r.Update(ctx, &existing)
}

// buildDeployment 根据 InferenceService 构建 Deployment 对象
func (r *InferenceServiceReconciler) buildDeployment(inferSvc *servingv1.InferenceService) *appsv1.Deployment {
	modelName := strings.TrimSpace(inferSvc.Spec.ModelName)
	modelLabel := normalizeModelLabelValue(modelName)
	labels := map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
		"serving.trin.io/model":            modelLabel,
		"llm-model":                        modelLabel,
		"app.kubernetes.io/name":           inferSvc.Name,
		"app.kubernetes.io/instance":       inferSvc.Name,
		"app.kubernetes.io/part-of":        "llm-serving-control-plane",
	}

	// 默认副本数为 1
	replicas := int32(1)
	if inferSvc.Spec.Replicas != nil {
		replicas = *inferSvc.Spec.Replicas
	}

	profile := resourceProfileFor(inferSvc.Spec.ResourceProfile)

	// 扩缩容可能重建 Recreate 策略下的 GPU Pod；GPU 调度必须精确到节点。
	strategy := appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{
			r.buildContainer(inferSvc, profile),
		},
		NodeSelector: inferSvc.Spec.NodeSelector,
		Tolerations:  inferSvc.Spec.Tolerations,
		Affinity:     inferSvc.Spec.Affinity,
	}
	if len(podSpec.Tolerations) == 0 {
		podSpec.Tolerations = nil
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inferSvc.Name,
			Namespace: inferSvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: strategy,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: podSpec,
			},
		},
	}
}

// resourceProfile 描述一个 ResourceProfile 生成的资源与调度约束。
type resourceProfile struct {
	gpu    int64
	cpu    string
	memory string
}

// resourceProfileFor 将 InferenceService.spec.resourceProfile 翻译为 Pod 所需资源和调度配置。
// 默认 gpu-small；gpu-medium/gpu-large 增加 CPU/内存与 GPU 数量；cpu-only 用于无 GPU 的 mock/本地引擎。
func resourceProfileFor(profile string) resourceProfile {
	switch strings.TrimSpace(profile) {
	case "gpu-large":
		return resourceProfile{
			gpu:    2,
			cpu:    "16",
			memory: "128Gi",
		}
	case "gpu-medium":
		return resourceProfile{
			gpu:    1,
			cpu:    "8",
			memory: "64Gi",
		}
	case "cpu-only":
		return resourceProfile{
			gpu:    0,
			cpu:    "2",
			memory: "8Gi",
		}
	default: // gpu-small or empty
		return resourceProfile{
			gpu:    1,
			cpu:    "4",
			memory: "32Gi",
		}
	}
}

// buildContainer 根据 InferenceService 构建 Container
func (r *InferenceServiceReconciler) buildContainer(inferSvc *servingv1.InferenceService, profile resourceProfile) corev1.Container {
	engine := inferSvc.Spec.Engine
	if engine == "" {
		engine = "vllm" // 默认值
	}

	// 获取引擎配置（镜像和参数）
	image, args := r.getEngineConfig(engine, inferSvc.Spec.ModelName)

	// 如果用户指定了自定义镜像，使用用户的镜像
	if inferSvc.Spec.Image != "" {
		image = inferSvc.Spec.Image
	}

	// 设置 ImagePullPolicy
	pullPolicy := corev1.PullIfNotPresent
	switch inferSvc.Spec.ImagePullPolicy {
	case "Always":
		pullPolicy = corev1.PullAlways
	case "Never":
		pullPolicy = corev1.PullNever
	case "IfNotPresent":
		pullPolicy = corev1.PullIfNotPresent
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resourceMustParse(profile.cpu),
			corev1.ResourceMemory: resourceMustParse(profile.memory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resourceMustParse(profile.cpu),
			corev1.ResourceMemory: resourceMustParse(profile.memory),
		},
	}
	if profile.gpu > 0 {
		resources.Limits[corev1.ResourceName("nvidia.com/gpu")] = resourceMustParse(fmt.Sprintf("%d", profile.gpu))
		// 请求和限制保持一致，避免 GPU 过量承诺
		resources.Requests[corev1.ResourceName("nvidia.com/gpu")] = resourceMustParse(fmt.Sprintf("%d", profile.gpu))
	}

	return corev1.Container{
		Name:            engine,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Args:            args,
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: 8000,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Resources: resources,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/health",
					Port:   intstr.FromInt(8000),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 60,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
	}
}

func resourceMustParse(value string) resource.Quantity {
	q, err := resource.ParseQuantity(value)
	if err != nil || value == "" {
		return resource.MustParse("1")
	}
	return q
}

// getEngineConfig 根据引擎类型返回默认镜像和启动参数
func (r *InferenceServiceReconciler) getEngineConfig(engine, modelName string) (string, []string) {
	switch engine {
	case "vllm":
		return "vllm/vllm-openai:latest", []string{
			"--model", modelName,
			"--host", "0.0.0.0",
			"--port", "8000",
		}
	case "triton":
		// Triton Inference Server 示例配置
		return "nvcr.io/nvidia/tritonserver:latest", []string{
			"tritonserver",
			"--model-repository=/models",
			"--http-port=8000",
		}
	case "tgi":
		// Text Generation Inference (HuggingFace) 示例配置
		return "ghcr.io/huggingface/text-generation-inference:latest", []string{
			"--model-id", modelName,
			"--port", "8000",
		}
	case "tensorrt":
		// TensorRT-LLM 示例配置
		return "nvcr.io/nvidia/tensorrt-llm:latest", []string{
			"--model", modelName,
			"--port", "8000",
		}
	case "mock":
		// Mock 引擎，用于本地测试（无需 GPU）
		return "mock-vllm:latest", nil
	default:
		// 默认使用 vLLM
		return "vllm/vllm-openai:latest", []string{
			"--model", modelName,
			"--host", "0.0.0.0",
			"--port", "8000",
		}
	}
}

// reconcileService 确保 Service 存在且与 CR 期望状态一致
func (r *InferenceServiceReconciler) reconcileService(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	logger := log.FromContext(ctx)

	// 构建期望的 Service
	service := r.buildService(inferSvc)

	// 设置 Owner Reference
	if err := ctrl.SetControllerReference(inferSvc, service, r.Scheme); err != nil {
		return err
	}

	// 检查 Service 是否已存在
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(service), &existing)

	if err != nil && errors.IsNotFound(err) {
		// 不存在，创建新的 Service
		logger.Info("Creating Service", "name", service.Name)
		return r.Create(ctx, service)
	} else if err != nil {
		return err
	}

	changed := !reflect.DeepEqual(existing.Labels, service.Labels) ||
		!reflect.DeepEqual(existing.Spec.Selector, service.Spec.Selector) ||
		!reflect.DeepEqual(existing.Spec.Ports, service.Spec.Ports)
	if !changed {
		return nil
	}

	existing.Labels = service.Labels
	existing.Spec.Selector = service.Spec.Selector
	existing.Spec.Ports = service.Spec.Ports
	logger.Info("Updating Service", "name", service.Name)
	return r.Update(ctx, &existing)
}

// buildService 根据 InferenceService 构建 Service 对象
func (r *InferenceServiceReconciler) buildService(inferSvc *servingv1.InferenceService) *corev1.Service {
	modelName := strings.TrimSpace(inferSvc.Spec.ModelName)
	modelLabel := normalizeModelLabelValue(modelName)
	labels := map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
		"serving.trin.io/model":            modelLabel,
		"llm-model":                        modelLabel,
		"app.kubernetes.io/name":           inferSvc.Name,
		"app.kubernetes.io/instance":       inferSvc.Name,
		"app.kubernetes.io/part-of":        "llm-serving-control-plane",
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inferSvc.Name,
			Namespace: inferSvc.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       8000,
					TargetPort: intstr.FromInt(8000),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// updateStatus 更新 InferenceService 的 Status
func normalizeModelLabelValue(modelName string) string {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return "default"
	}

	var builder strings.Builder
	builder.Grow(len(trimmed))
	lastSeparator := false
	for _, r := range strings.ToLower(trimmed) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastSeparator = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == ' ', r == '\\':
			if !lastSeparator {
				builder.WriteRune('-')
				lastSeparator = true
			}
		default:
			if !lastSeparator {
				builder.WriteRune('-')
				lastSeparator = true
			}
		}
	}

	result := strings.Trim(builder.String(), "-_.")
	if result == "" {
		return "default"
	}
	return result
}

func (r *InferenceServiceReconciler) updateStatus(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	// 1. 获取关联的 Deployment 状态
	var deployment appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{
		Name:      inferSvc.Name,
		Namespace: inferSvc.Namespace,
	}, &deployment)

	if err != nil {
		if errors.IsNotFound(err) {
			return r.updateLifecycleStatus(ctx, inferSvc, lifecycleState{
				available:   metav1.ConditionFalse,
				ready:       metav1.ConditionFalse,
				progressing: metav1.ConditionTrue,
				degraded:    metav1.ConditionFalse,
				unknown:     metav1.ConditionFalse,
				reason:      "DeploymentNotFound",
				message:     "Waiting for deployment to be created",
			})
		}
		_ = r.updateLifecycleStatus(ctx, inferSvc, lifecycleState{
			available:   metav1.ConditionUnknown,
			ready:       metav1.ConditionUnknown,
			progressing: metav1.ConditionUnknown,
			degraded:    metav1.ConditionUnknown,
			unknown:     metav1.ConditionTrue,
			reason:      "DeploymentInspectionFailed",
			message:     err.Error(),
		})
		return err
	}

	// 2. 更新 Service URL
	serviceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000", inferSvc.Name, inferSvc.Namespace)
	inferSvc.Status.URL = serviceURL

	// 3. 根据 Deployment 状态判断 Ready
	replicas := int32(1)
	if inferSvc.Spec.Replicas != nil {
		replicas = *inferSvc.Spec.Replicas
	}

	inferSvc.Status.Replicas = deployment.Status.ReadyReplicas

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inferSvc.Namespace), client.MatchingLabels{
		"serving.trin.io/inferenceservice": inferSvc.Name,
	}); err != nil {
		_ = r.updateLifecycleStatus(ctx, inferSvc, lifecycleState{
			available:   metav1.ConditionUnknown,
			ready:       metav1.ConditionUnknown,
			progressing: metav1.ConditionUnknown,
			degraded:    metav1.ConditionUnknown,
			unknown:     metav1.ConditionTrue,
			reason:      "PodInspectionFailed",
			message:     err.Error(),
		})
		return err
	}

	return r.updateLifecycleStatus(ctx, inferSvc, evaluateLifecycle(&deployment, pods.Items, replicas))
}

type lifecycleState struct {
	available   metav1.ConditionStatus
	ready       metav1.ConditionStatus
	progressing metav1.ConditionStatus
	degraded    metav1.ConditionStatus
	unknown     metav1.ConditionStatus
	reason      string
	message     string
}

func evaluateLifecycle(deployment *appsv1.Deployment, pods []corev1.Pod, replicas int32) lifecycleState {
	available := metav1.ConditionFalse
	if deployment.Status.AvailableReplicas > 0 {
		available = metav1.ConditionTrue
	}

	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
				return degradedLifecycle(available, "Unschedulable", condition.Message)
			}
		}
		for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			if status.State.Waiting != nil {
				switch status.State.Waiting.Reason {
				case "ImagePullBackOff", "ErrImagePull":
					return degradedLifecycle(available, "ImagePullFailed", status.State.Waiting.Message)
				case "CrashLoopBackOff":
					return degradedLifecycle(available, "CrashLoopBackOff", status.State.Waiting.Message)
				}
			}
		}
		if pod.Status.Phase == corev1.PodFailed {
			return degradedLifecycle(available, "PodFailed", pod.Status.Message)
		}
	}

	if replicas == 0 {
		return lifecycleState{
			available:   metav1.ConditionFalse,
			ready:       metav1.ConditionFalse,
			progressing: metav1.ConditionFalse,
			degraded:    metav1.ConditionFalse,
			unknown:     metav1.ConditionFalse,
			reason:      "ScaledToZero",
			message:     "Inference service is scaled to zero replicas",
		}
	}

	if deployment.Status.ReadyReplicas >= replicas {
		return lifecycleState{
			available:   available,
			ready:       metav1.ConditionTrue,
			progressing: metav1.ConditionFalse,
			degraded:    metav1.ConditionFalse,
			unknown:     metav1.ConditionFalse,
			reason:      "DeploymentReady",
			message:     "Inference service is ready",
		}
	}

	return lifecycleState{
		available:   available,
		ready:       metav1.ConditionFalse,
		progressing: metav1.ConditionTrue,
		degraded:    metav1.ConditionFalse,
		unknown:     metav1.ConditionFalse,
		reason:      "DeploymentProgressing",
		message:     fmt.Sprintf("Deployment is progressing (%d/%d ready)", deployment.Status.ReadyReplicas, replicas),
	}
}

func degradedLifecycle(available metav1.ConditionStatus, reason, message string) lifecycleState {
	if message == "" {
		message = reason
	}
	return lifecycleState{
		available:   available,
		ready:       metav1.ConditionFalse,
		progressing: metav1.ConditionFalse,
		degraded:    metav1.ConditionTrue,
		unknown:     metav1.ConditionFalse,
		reason:      reason,
		message:     message,
	}
}

func (r *InferenceServiceReconciler) updateLifecycleStatus(ctx context.Context, inferSvc *servingv1.InferenceService, state lifecycleState) error {
	previousAvailable := meta.FindStatusCondition(inferSvc.Status.Conditions, "Available")
	previousReady := meta.FindStatusCondition(inferSvc.Status.Conditions, "Ready")
	meta.SetStatusCondition(&inferSvc.Status.Conditions, lifecycleCondition(inferSvc, "Available", state.available, state.reason, state.message))
	meta.SetStatusCondition(&inferSvc.Status.Conditions, lifecycleCondition(inferSvc, "Ready", state.ready, state.reason, state.message))
	meta.SetStatusCondition(&inferSvc.Status.Conditions, lifecycleCondition(inferSvc, "Progressing", state.progressing, state.reason, state.message))
	meta.SetStatusCondition(&inferSvc.Status.Conditions, lifecycleCondition(inferSvc, "Degraded", state.degraded, state.reason, state.message))
	meta.SetStatusCondition(&inferSvc.Status.Conditions, lifecycleCondition(inferSvc, "Unknown", state.unknown, state.reason, state.message))

	if err := r.Status().Update(ctx, inferSvc); err != nil {
		return err
	}
	if r.Recorder != nil && (previousAvailable == nil || previousAvailable.Status != state.available || previousReady == nil || previousReady.Status != state.ready || previousReady.Reason != state.reason) {
		eventType := corev1.EventTypeNormal
		if state.degraded == metav1.ConditionTrue || state.unknown == metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(inferSvc, eventType, state.reason, "%s", state.message)
	}
	return nil
}

func lifecycleCondition(inferSvc *servingv1.InferenceService, conditionType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: inferSvc.Generation,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("inferenceservice-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1.InferenceService{}).
		Owns(&appsv1.Deployment{}). // 监听 Deployment 变化
		Owns(&corev1.Service{}).    // 监听 Service 变化
		Named("inferenceservice").
		Complete(r)
}
