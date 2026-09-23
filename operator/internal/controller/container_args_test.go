package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	servingv1 "github.com/trin/llm-serving-control-plane/operator/api/v1"
)

// TestBuildContainer_ExtraArgsAppendedAfterEngineDefaults (Phase 6, T-A1)
// verifies that spec.extraArgs are appended AFTER the controller-generated engine
// flags, so experiments can toggle engine features (prefix caching, AWQ, ...)
// without changing the controller defaults.
func TestBuildContainer_ExtraArgsAppendedAfterEngineDefaults(t *testing.T) {
	reconciler := &InferenceServiceReconciler{}

	inferSvc := &servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{
			ModelName: "Qwen/Qwen2.5-0.5B-Instruct",
			Engine:    "vllm",
			ExtraArgs: []string{"--enable-prefix-caching", "--max-model-len", "4096"},
		},
	}
	container := reconciler.buildContainer(inferSvc, resourceProfileFor("gpu-t4-small"))
	assert.Equal(t, []string{
		"--model", "Qwen/Qwen2.5-0.5B-Instruct",
		"--host", "0.0.0.0",
		"--port", "8000",
		"--enable-prefix-caching",
		"--max-model-len", "4096",
	}, container.Args, "extraArgs must be appended after the engine defaults, in order")

	// With no extraArgs the container must carry exactly the engine defaults
	// (guards against nil/empty slice regressions).
	plain := reconciler.buildContainer(&servingv1.InferenceService{
		Spec: servingv1.InferenceServiceSpec{ModelName: "m", Engine: "vllm"},
	}, resourceProfileFor("gpu-t4-small"))
	assert.Equal(t, []string{"--model", "m", "--host", "0.0.0.0", "--port", "8000"}, plain.Args)
}
