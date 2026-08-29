package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEvaluateLifecycle(t *testing.T) {
	readyDeployment := &appsv1.Deployment{Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1}}

	tests := []struct {
		name       string
		deployment *appsv1.Deployment
		pods       []corev1.Pod
		replicas   int32
		reason     string
		ready      metav1.ConditionStatus
		available  metav1.ConditionStatus
		degraded   metav1.ConditionStatus
	}{
		{
			name:       "ready deployment",
			deployment: readyDeployment,
			replicas:   1,
			reason:     "DeploymentReady",
			ready:      metav1.ConditionTrue,
			available:  metav1.ConditionTrue,
			degraded:   metav1.ConditionFalse,
		},
		{
			name: "partially available deployment",
			deployment: &appsv1.Deployment{Status: appsv1.DeploymentStatus{
				ReadyReplicas: 1, AvailableReplicas: 1,
			}},
			replicas:   3,
			reason:     "DeploymentProgressing",
			ready:      metav1.ConditionFalse,
			available:  metav1.ConditionTrue,
			degraded:   metav1.ConditionFalse,
		},
		{
			name:       "unschedulable pod",
			deployment: readyDeployment,
			replicas:   1,
			pods: []corev1.Pod{{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "Insufficient nvidia.com/gpu",
			}}}}},
			reason:   "Unschedulable",
			ready:    metav1.ConditionFalse,
			available: metav1.ConditionTrue,
			degraded: metav1.ConditionTrue,
		},
		{
			name:       "image pull failure",
			deployment: readyDeployment,
			replicas:   1,
			pods: []corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "image unavailable"}},
			}}}}},
			reason:   "ImagePullFailed",
			ready:    metav1.ConditionFalse,
			available: metav1.ConditionTrue,
			degraded: metav1.ConditionTrue,
		},
		{
			name:       "crash loop",
			deployment: readyDeployment,
			replicas:   1,
			pods: []corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "container restart"}},
			}}}}},
			reason:   "CrashLoopBackOff",
			ready:    metav1.ConditionFalse,
			available: metav1.ConditionTrue,
			degraded: metav1.ConditionTrue,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := evaluateLifecycle(test.deployment, test.pods, test.replicas)
			assert.Equal(t, test.reason, state.reason)
			assert.Equal(t, test.ready, state.ready)
			assert.Equal(t, test.available, state.available)
			assert.Equal(t, test.degraded, state.degraded)
		})
	}
}