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

// Finalizer name
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

	// 1. Fetch the InferenceService CR
	var inferSvc servingv1.InferenceService
	if err := r.Get(ctx, req.NamespacedName, &inferSvc); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("InferenceService not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get InferenceService")
		return ctrl.Result{}, err
	}

	// 2. Check whether the object is being deleted (DeletionTimestamp is not zero)
	if !inferSvc.ObjectMeta.DeletionTimestamp.IsZero() {
		// If it has our Finalizer, cleanup needs to be performed
		if controllerutil.ContainsFinalizer(&inferSvc, finalizerName) {
			logger.Info("Executing finalizer cleanup...")

			// TODO: perform external resource cleanup here (e.g. AWS resources, DNS, etc.)
			// Intentionally left empty for now, since K8s resources are deleted cascadingly.

			// Remove the Finalizer so K8s can delete the object
			controllerutil.RemoveFinalizer(&inferSvc, finalizerName)
			if err := r.Update(ctx, &inferSvc); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("Finalizer removed, object can be deleted now")
		}
		// Stop reconciling
		return ctrl.Result{}, nil
	}

	// If the object is not being deleted, make sure the Finalizer exists
	if !controllerutil.ContainsFinalizer(&inferSvc, finalizerName) {
		controllerutil.AddFinalizer(&inferSvc, finalizerName)
		if err := r.Update(ctx, &inferSvc); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("Reconciling InferenceService", "name", inferSvc.Name, "namespace", inferSvc.Namespace)

	// 3. Reconcile the Deployment
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

	// 4. Reconcile the Service
	if err := r.reconcileService(ctx, &inferSvc); err != nil {
		logger.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	// 5. Update the Status (based on the real state)
	if err := r.updateStatus(ctx, &inferSvc); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled InferenceService")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil // requeue periodically to check for state changes
}

// reconcileDeployment ensures the Deployment exists and matches the desired state from the CR.
func (r *InferenceServiceReconciler) reconcileDeployment(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	logger := log.FromContext(ctx)

	// Build the desired Deployment
	deployment := r.buildDeployment(inferSvc)

	// Set the Owner Reference to enable cascading deletion
	if err := ctrl.SetControllerReference(inferSvc, deployment, r.Scheme); err != nil {
		return err
	}

	// Check whether the Deployment already exists
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(deployment), &existing)

	if err != nil && errors.IsNotFound(err) {
		// Not found: create a new Deployment
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

// buildDeployment builds a Deployment object from the InferenceService.
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

	// Default to 1 replica
	replicas := int32(1)
	if inferSvc.Spec.Replicas != nil {
		replicas = *inferSvc.Spec.Replicas
	}

	profile := resourceProfileFor(inferSvc.Spec.ResourceProfile)

	// Scaling may rebuild GPU Pods under the Recreate strategy; GPU scheduling must be node-precise.
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

// resourceProfile describes the resources and scheduling constraints generated by a ResourceProfile.
type resourceProfile struct {
	gpu    int64
	cpu    string
	memory string
}

// resourceProfileFor translates InferenceService.spec.resourceProfile into the Pod's resource and
// scheduling configuration. Defaults to gpu-small; gpu-medium/gpu-large increase CPU/memory and the
// GPU count; cpu-only is used for GPU-free mock/local engines.
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
	case "gpu-t4-small":
		// Fits an Azure Standard_NC4as_T4_v3 node (1x T4, 4 vCPU / ~28Gi raw,
		// less after AKS/system reservations).
		return resourceProfile{
			gpu:    1,
			cpu:    "2",
			memory: "12Gi",
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

// buildContainer builds a Container from the InferenceService.
func (r *InferenceServiceReconciler) buildContainer(inferSvc *servingv1.InferenceService, profile resourceProfile) corev1.Container {
	engine := inferSvc.Spec.Engine
	if engine == "" {
		engine = "vllm" // default value
	}

	// Get the engine configuration (image and arguments)
	image, args := r.getEngineConfig(engine, inferSvc.Spec.ModelName)

	// If the user specified a custom image, use theirs
	if inferSvc.Spec.Image != "" {
		image = inferSvc.Spec.Image
	}

	// Set ImagePullPolicy
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
		// Keep requests and limits equal to avoid GPU overcommitment
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

// getEngineConfig returns the default image and launch arguments for the given engine type.
func (r *InferenceServiceReconciler) getEngineConfig(engine, modelName string) (string, []string) {
	switch engine {
	case "vllm":
		return "vllm/vllm-openai:latest", []string{
			"--model", modelName,
			"--host", "0.0.0.0",
			"--port", "8000",
		}
	case "triton":
		// Example Triton Inference Server configuration
		return "nvcr.io/nvidia/tritonserver:latest", []string{
			"tritonserver",
			"--model-repository=/models",
			"--http-port=8000",
		}
	case "tgi":
		// Example Text Generation Inference (HuggingFace) configuration
		return "ghcr.io/huggingface/text-generation-inference:latest", []string{
			"--model-id", modelName,
			"--port", "8000",
		}
	case "tensorrt":
		// Example TensorRT-LLM configuration
		return "nvcr.io/nvidia/tensorrt-llm:latest", []string{
			"--model", modelName,
			"--port", "8000",
		}
	case "mock":
		// Mock engine for local testing (no GPU required)
		return "mock-vllm:latest", nil
	default:
		// Default to vLLM
		return "vllm/vllm-openai:latest", []string{
			"--model", modelName,
			"--host", "0.0.0.0",
			"--port", "8000",
		}
	}
}

// reconcileService ensures the Service exists and matches the desired state from the CR.
func (r *InferenceServiceReconciler) reconcileService(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	logger := log.FromContext(ctx)

	// Build the desired Service
	service := r.buildService(inferSvc)

	// Set the Owner Reference
	if err := ctrl.SetControllerReference(inferSvc, service, r.Scheme); err != nil {
		return err
	}

	// Check whether the Service already exists
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(service), &existing)

	if err != nil && errors.IsNotFound(err) {
		// Not found: create a new Service
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

// buildService builds a Service object from the InferenceService.
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

// normalizeModelLabelValue converts a model name into a valid Kubernetes label value.
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

// updateStatus updates the Status of the InferenceService based on the actual Deployment/Pod state.
func (r *InferenceServiceReconciler) updateStatus(ctx context.Context, inferSvc *servingv1.InferenceService) error {
	// 1. Fetch the state of the associated Deployment
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

	// 2. Update the Service URL
	serviceURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000", inferSvc.Name, inferSvc.Namespace)
	inferSvc.Status.URL = serviceURL

	// 3. Determine readiness based on the Deployment state
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
		Owns(&appsv1.Deployment{}). // watch Deployment changes
		Owns(&corev1.Service{}).    // watch Service changes
		Named("inferenceservice").
		Complete(r)
}
