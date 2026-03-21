/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InferenceServiceSpec defines the desired state of InferenceService
type InferenceServiceSpec struct {
	// ModelName is the name of the model to serve (e.g., "Qwen/Qwen2.5-7B-Instruct")
	// +kubebuilder:validation:Required
	ModelName string `json:"modelName"`

	// Replicas is the number of desired pods running the model
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// ResourceProfile defines the compute resource template (e.g., "gpu-small", "gpu-large", "cpu-only")
	// +kubebuilder:default="gpu-small"
	// +optional
	ResourceProfile string `json:"resourceProfile,omitempty"`

	// Engine specifies the inference engine type (e.g., "vllm", "triton", "tgi", "tensorrt", "mock")
	// Use "mock" for local testing without GPU
	// +kubebuilder:default="vllm"
	// +kubebuilder:validation:Enum=vllm;triton;tgi;tensorrt;mock
	// +optional
	Engine string `json:"engine,omitempty"`

	// Image allows custom container image override (optional)
	// If not specified, a default image will be used based on the Engine type
	// +optional
	Image string `json:"image,omitempty"`

	// ImagePullPolicy describes a policy for if/when to pull a container image
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`
}

// InferenceServiceStatus defines the observed state of InferenceService.
type InferenceServiceStatus struct {
	// URL is the endpoint to access the inference service
	// This is populated by the controller after the Service is created
	// +optional
	URL string `json:"url,omitempty"`

	// Conditions represent the current state of the InferenceService
	// Standard condition types: Available, Progressing, Degraded
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelName",description="Model name"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas",description="Desired replicas"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url",description="Service endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// InferenceService is the Schema for the inferenceservices API
type InferenceService struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of InferenceService
	// +required
	Spec InferenceServiceSpec `json:"spec"`

	// status defines the observed state of InferenceService
	// +optional
	Status InferenceServiceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// InferenceServiceList contains a list of InferenceService
type InferenceServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []InferenceService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceService{}, &InferenceServiceList{})
}
