package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	servingv1 "github.com/trin/llm-serving-control-plane/operator/api/v1"
)

func TestResourceProfileFor(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		gpu     int64
		cpu     string
		memory  string
		hasGPU  bool
	}{
		{name: "defaults to gpu-small", profile: "", gpu: 1, cpu: "4", memory: "32Gi", hasGPU: true},
		{name: "gpu-small", profile: "gpu-small", gpu: 1, cpu: "4", memory: "32Gi", hasGPU: true},
		{name: "gpu-medium", profile: "gpu-medium", gpu: 1, cpu: "8", memory: "64Gi", hasGPU: true},
		{name: "gpu-large", profile: "gpu-large", gpu: 2, cpu: "16", memory: "128Gi", hasGPU: true},
		{name: "cpu-only", profile: "cpu-only", gpu: 0, cpu: "2", memory: "8Gi", hasGPU: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := resourceProfileFor(test.profile)
			assert.Equal(t, test.gpu, profile.gpu)
			assert.Equal(t, test.cpu, profile.cpu)
			assert.Equal(t, test.memory, profile.memory)
			assert.Equal(t, test.hasGPU, profile.gpu > 0)
		})
	}
}

func TestBuildDeployment_AppliesGPUProfile(t *testing.T) {
	reconciler := &InferenceServiceReconciler{}
	inferSvc := &servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{
			ModelName:       "Qwen/Qwen2.5-7B-Instruct",
			Engine:          "vllm",
			ResourceProfile: "gpu-large",
			NodeSelector:    map[string]string{"accelerator.example.com/model": "A100"},
			Tolerations: []corev1.Toleration{{
				Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}},
		},
	}

	deployment := reconciler.buildDeployment(inferSvc)
	container := deployment.Spec.Template.Spec.Containers[0]

	gpuQuantity, ok := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	assert.True(t, ok)
	assert.Equal(t, "2", gpuQuantity.String())

	assert.Equal(t, "A100", deployment.Spec.Template.Spec.NodeSelector["accelerator.example.com/model"])
	assert.Len(t, deployment.Spec.Template.Spec.Tolerations, 1)
	assert.NotNil(t, deployment.Spec.Template.Spec.Affinity)
	assert.NotNil(t, container.ReadinessProbe)
	assert.Equal(t, "/health", container.ReadinessProbe.HTTPGet.Path)

	// cpu-only must not request GPU
	cpuOnly := reconciler.buildDeployment(&servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{
			ModelName:       "mock",
			Engine:          "mock",
			ResourceProfile: "cpu-only",
		},
	})
	_, hasGPU := cpuOnly.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	assert.False(t, hasGPU)
}
