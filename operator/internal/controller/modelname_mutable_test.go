package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	servingv1 "github.com/trin/llm-serving-control-plane/operator/api/v1"
)

// TestBuildDeployment_SelectorUsesOnlyStableKeys (Phase 6, T-F5) verifies the
// root-cause fix for L5: the Deployment selector must not reference the model
// label, otherwise mutating spec.modelName changes the selector and Kubernetes
// rejects the update with "field is immutable".
func TestBuildDeployment_SelectorUsesOnlyStableKeys(t *testing.T) {
	reconciler := &InferenceServiceReconciler{}
	inferSvc := &servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{
			ModelName:       "Qwen/Qwen2.5-0.5B-Instruct",
			Engine:          "vllm",
			ResourceProfile: "gpu-t4-small",
		},
	}

	deployment := reconciler.buildDeployment(inferSvc)
	selector := deployment.Spec.Selector.MatchLabels

	assert.Equal(t, map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
	}, selector, "selector must only use stable keys")

	// The model label must still exist on the object metadata and Pod template.
	assert.Equal(t, "qwen-qwen2-5-0-5b-instruct", deployment.Labels["llm-model"])
	assert.Equal(t, "qwen-qwen2-5-0-5b-instruct", deployment.Spec.Template.Labels["llm-model"])
	assert.Equal(t, "qwen-qwen2-5-0-5b-instruct", deployment.Labels["serving.trin.io/model"])
}

// TestBuildDeployment_ModelNameMutationKeepsSelectorStable (T-F5) asserts that
// hot-swapping the model produces a Deployment whose selector is byte-identical,
// while the Pod template labels and container args change.
func TestBuildDeployment_ModelNameMutationKeepsSelectorStable(t *testing.T) {
	reconciler := &InferenceServiceReconciler{}
	base := servingv1.InferenceServiceSpec{
		ModelName:       "Qwen/Qwen2.5-0.5B-Instruct",
		Engine:          "vllm",
		ResourceProfile: "gpu-t4-small",
	}

	before := reconciler.buildDeployment(&servingv1.InferenceService{Spec: base})

	mutated := base
	mutated.ModelName = "Qwen/Qwen2.5-7B-Instruct-AWQ"
	after := reconciler.buildDeployment(&servingv1.InferenceService{Spec: mutated})

	assert.Equal(t, before.Spec.Selector.MatchLabels, after.Spec.Selector.MatchLabels,
		"selector must not change when modelName changes")
	assert.NotEqual(t, before.Labels["llm-model"], after.Labels["llm-model"],
		"model label must follow modelName")
	assert.NotEqual(t, before.Spec.Template.Spec.Containers[0].Args, after.Spec.Template.Spec.Containers[0].Args,
		"container args must follow modelName")
}

// TestBuildService_SelectorUsesOnlyStableKeys (T-F5) mirrors the Deployment fix
// for the Service: selector is stable, model label stays on metadata for gateway
// discovery (DiscoverByModel reads llm-model off Service labels).
func TestBuildService_SelectorUsesOnlyStableKeys(t *testing.T) {
	reconciler := &InferenceServiceReconciler{}
	inferSvc := &servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{
			ModelName: "Qwen/Qwen2.5-7B-Instruct-AWQ",
		},
	}

	service := reconciler.buildService(inferSvc)

	assert.Equal(t, map[string]string{
		"app":                              inferSvc.Name,
		"serving.trin.io/inferenceservice": inferSvc.Name,
	}, service.Spec.Selector, "service selector must only use stable keys")

	require.NotNil(t, service.Labels)
	assert.Equal(t, "qwen-qwen2-5-7b-instruct-awq", service.Labels["llm-model"],
		"model label must remain on Service metadata for gateway discovery")
}
