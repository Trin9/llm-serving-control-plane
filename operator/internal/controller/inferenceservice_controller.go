package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.trin.io,resources=inferenceservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

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
		// 如果 Deployment 创建失败，更新 Status 为 Degraded
		_ = r.updateStatusCondition(ctx, &inferSvc, metav1.ConditionFalse, "DeploymentFailed", err.Error())
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

	// 已存在，更新 Deployment（只更新我们关心的字段）
	existing.Spec.Replicas = deployment.Spec.Replicas

	// 更新容器配置（确保容器存在）
	if len(existing.Spec.Template.Spec.Containers) > 0 {
		desiredContainer := deployment.Spec.Template.Spec.Containers[0]
		existing.Spec.Template.Spec.Containers[0].Image = desiredContainer.Image
		existing.Spec.Template.Spec.Containers[0].Args = desiredContainer.Args
		existing.Spec.Template.Spec.Containers[0].Name = desiredContainer.Name
	} else {
		// 如果容器不存在，替换整个容器列表
		existing.Spec.Template.Spec.Containers = deployment.Spec.Template.Spec.Containers
	}

	logger.Info("Updating Deployment", "name", deployment.Name)
	return r.Update(ctx, &existing)
}

// buildDeployment 根据 InferenceService 构建 Deployment 对象
func (r *InferenceServiceReconciler) buildDeployment(inferSvc *servingv1.InferenceService) *appsv1.Deployment {
	labels := map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
	}

	// 默认副本数为 1
	replicas := int32(1)
	if inferSvc.Spec.Replicas != nil {
		replicas = *inferSvc.Spec.Replicas
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      inferSvc.Name,
			Namespace: inferSvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						r.buildContainer(inferSvc),
					},
				},
			},
		},
	}
}

// buildContainer 根据 InferenceService 构建 Container
func (r *InferenceServiceReconciler) buildContainer(inferSvc *servingv1.InferenceService) corev1.Container {
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
		// 资源配置可以根据 ResourceProfile 扩展
		// 暂时使用默认配置
	}
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
		// 使用 Python 内置 HTTP 服务器模拟推理服务
		return "python:3.9-alpine", []string{
			"python", "-m", "http.server", "8000",
		}
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

	// Service 一般不需要频繁更新，这里只记录日志
	logger.Info("Service already exists", "name", service.Name)
	return nil
}

// buildService 根据 InferenceService 构建 Service 对象
func (r *InferenceServiceReconciler) buildService(inferSvc *servingv1.InferenceService) *corev1.Service {
	labels := map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
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
func (r *InferenceServiceReconciler) updateStatus(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	// 1. 获取关联的 Deployment 状态
	var deployment appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{
		Name:      inferSvc.Name,
		Namespace: inferSvc.Namespace,
	}, &deployment)

	if err != nil {
		if errors.IsNotFound(err) {
			// Deployment 还没创建出来
			return r.updateStatusCondition(ctx, inferSvc, metav1.ConditionFalse, "DeploymentNotFound", "Waiting for deployment to be created")
		}
		return err
	}

	// 2. 更新 Service URL
	serviceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000", inferSvc.Name, inferSvc.Namespace)
	inferSvc.Status.URL = serviceURL

	// 3. 根据 Deployment 状态判断 Ready
	// 期望副本数
	replicas := int32(1)
	if inferSvc.Spec.Replicas != nil {
		replicas = *inferSvc.Spec.Replicas
	}

	if deployment.Status.ReadyReplicas == replicas {
		// 全部 Ready
		return r.updateStatusCondition(ctx, inferSvc, metav1.ConditionTrue, "DeploymentReady", "Inference service is ready")
	} else {
		// 还在启动中
		msg := fmt.Sprintf("Deployment is progressing (%d/%d ready)", deployment.Status.ReadyReplicas, replicas)
		return r.updateStatusCondition(ctx, inferSvc, metav1.ConditionFalse, "DeploymentProgressing", msg)
	}
}

// 辅助函数：更新 Condition 并提交
func (r *InferenceServiceReconciler) updateStatusCondition(ctx context.Context, inferSvc *servingv1.InferenceService, status metav1.ConditionStatus, reason, message string) error {
	meta.SetStatusCondition(&inferSvc.Status.Conditions, metav1.Condition{
		Type:               "Available", // 使用标准的 Available 类型
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})

	return r.Status().Update(ctx, inferSvc)
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1.InferenceService{}).
		Owns(&appsv1.Deployment{}). // 监听 Deployment 变化
		Owns(&corev1.Service{}).    // 监听 Service 变化
		Named("inferenceservice").
		Complete(r)
}
