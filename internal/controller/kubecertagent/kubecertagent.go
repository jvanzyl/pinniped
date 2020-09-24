// Copyright 2020 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kubecertagent provides controllers that ensure a set of pods (the kube-cert-agent), is
// colocated with the Kubernetes controller manager so that Pinniped can access its signing keys.
//
// Note: the controllers use a filter that accepts all pods that look like the controller manager or
// an agent pod, across any add/update/delete event. Each of the controllers only care about a
// subset of these events in reality, but the liberal filter implementation serves as an MVP.
package kubecertagent

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"

	"k8s.io/apimachinery/pkg/api/resource"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/klog/v2"
)

const (
	// ControllerManagerNamespace is the assumed namespace of the kube-controller-manager pod(s).
	ControllerManagerNamespace = "kube-system"

	controllerManagerNameAnnotationKey = "kube-cert-agent.pinniped.dev/controller-manager-name"
	controllerManagerUIDAnnotationKey  = "kube-cert-agent.pinniped.dev/controller-manager-uid"

	// agentPodLabelKey is used to identify which pods are created by the kube-cert-agent
	// controllers.
	agentPodLabelKey   = "kube-cert-agent.pinniped.dev"
	agentPodLabelValue = "true"

	// agentPodCertPathAnnotationKey is the annotation that the kube-cert-agent pod will use
	// to communicate the in-pod path to the kube API's certificate.
	agentPodCertPathAnnotationKey = "kube-cert-agent.pinniped.dev/cert-path"

	// agentPodKeyPathAnnotationKey is the annotation that the kube-cert-agent pod will use
	// to communicate the in-pod path to the kube API's key.
	agentPodKeyPathAnnotationKey = "kube-cert-agent.pinniped.dev/key-path"
)

type AgentPodConfig struct {
	// The namespace in which agent pods will be created.
	Namespace string

	// The container image used for the agent pods.
	ContainerImage string

	//  The name prefix for each of the agent pods.
	PodNamePrefix string
}

type CredentialIssuerConfigLocationConfig struct {
	// The namespace in which the CredentialIssuerConfig should be created/updated.
	Namespace string

	// The resource name for the CredentialIssuerConfig to be created/updated.
	Name string
}

func (c *AgentPodConfig) Labels() map[string]string {
	return map[string]string{
		agentPodLabelKey: agentPodLabelValue,
	}
}

func (c *AgentPodConfig) PodTemplate() *corev1.Pod {
	terminateImmediately := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.PodNamePrefix,
			Namespace: c.Namespace,
			Labels:    c.Labels(),
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &terminateImmediately,
			Containers: []corev1.Container{
				{
					Name:            "sleeper",
					Image:           c.ContainerImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/bin/sleep", "infinity"},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("16Mi"),
							corev1.ResourceCPU:    resource.MustParse("10m"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("16Mi"),
							corev1.ResourceCPU:    resource.MustParse("10m"),
						},
					},
				},
			},
		},
	}
	return pod
}

func newAgentPod(
	controllerManagerPod *corev1.Pod,
	template *corev1.Pod,
) *corev1.Pod {
	agentPod := template.DeepCopy()

	agentPod.Name = fmt.Sprintf("%s%s", agentPod.Name, hash(controllerManagerPod))

	// It would be nice to use the OwnerReferences field here, but the agent pod is most likely in a
	// different namespace than the kube-controller-manager pod, and therefore that breaks the
	// OwnerReferences contract (see metav1.OwnerReference doc).
	if agentPod.Annotations == nil {
		agentPod.Annotations = make(map[string]string)
	}
	agentPod.Annotations[controllerManagerNameAnnotationKey] = controllerManagerPod.Name
	agentPod.Annotations[controllerManagerUIDAnnotationKey] = string(controllerManagerPod.UID)

	agentPod.Spec.Containers[0].VolumeMounts = controllerManagerPod.Spec.Containers[0].VolumeMounts
	agentPod.Spec.Volumes = controllerManagerPod.Spec.Volumes
	agentPod.Spec.RestartPolicy = corev1.RestartPolicyNever
	agentPod.Spec.NodeSelector = controllerManagerPod.Spec.NodeSelector
	agentPod.Spec.AutomountServiceAccountToken = boolPtr(false)
	agentPod.Spec.NodeName = controllerManagerPod.Spec.NodeName
	agentPod.Spec.Tolerations = controllerManagerPod.Spec.Tolerations

	return agentPod
}

func isAgentPodUpToDate(actualAgentPod, expectedAgentPod *corev1.Pod) bool {
	return equality.Semantic.DeepEqual(
		actualAgentPod.Spec.Containers[0].VolumeMounts,
		expectedAgentPod.Spec.Containers[0].VolumeMounts,
	) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.Containers[0].Name,
			expectedAgentPod.Spec.Containers[0].Name,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.Containers[0].Image,
			expectedAgentPod.Spec.Containers[0].Image,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.Containers[0].Command,
			expectedAgentPod.Spec.Containers[0].Command,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.Volumes,
			expectedAgentPod.Spec.Volumes,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.RestartPolicy,
			expectedAgentPod.Spec.RestartPolicy,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.NodeSelector,
			expectedAgentPod.Spec.NodeSelector,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.AutomountServiceAccountToken,
			expectedAgentPod.Spec.AutomountServiceAccountToken,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.NodeName,
			expectedAgentPod.Spec.NodeName,
		) &&
		equality.Semantic.DeepEqual(
			actualAgentPod.Spec.Tolerations,
			expectedAgentPod.Spec.Tolerations,
		)
}

func isControllerManagerPod(obj metav1.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}

	if pod.Labels == nil {
		return false
	}

	component, ok := pod.Labels["component"]
	if !ok || component != "kube-controller-manager" {
		return false
	}

	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	return true
}

func isAgentPod(obj metav1.Object) bool {
	value, foundLabel := obj.GetLabels()[agentPodLabelKey]
	return foundLabel && value == agentPodLabelValue
}

func findControllerManagerPodForSpecificAgentPod(
	agentPod *corev1.Pod,
	kubeSystemPodInformer corev1informers.PodInformer,
) (*corev1.Pod, error) {
	name, ok := agentPod.Annotations[controllerManagerNameAnnotationKey]
	if !ok {
		klog.InfoS("agent pod missing parent name annotation", "pod", agentPod.Name)
		return nil, nil
	}

	uid, ok := agentPod.Annotations[controllerManagerUIDAnnotationKey]
	if !ok {
		klog.InfoS("agent pod missing parent uid annotation", "pod", agentPod.Name)
		return nil, nil
	}

	maybeControllerManagerPod, err := kubeSystemPodInformer.
		Lister().
		Pods(ControllerManagerNamespace).
		Get(name)
	notFound := k8serrors.IsNotFound(err)
	if err != nil && !notFound {
		return nil, fmt.Errorf("cannot get controller pod: %w", err)
	} else if notFound ||
		maybeControllerManagerPod == nil ||
		string(maybeControllerManagerPod.UID) != uid {
		return nil, nil
	}

	return maybeControllerManagerPod, nil
}

func boolPtr(b bool) *bool { return &b }

func hash(controllerManagerPod *corev1.Pod) string {
	// FNV should be faster than SHA, and we don't care about hash-reversibility here, and Kubernetes
	// uses FNV for their pod templates, so should be good enough for us?
	h := fnv.New32a()
	_, _ = h.Write([]byte(controllerManagerPod.UID)) // Never returns an error, per godoc.
	return hex.EncodeToString(h.Sum([]byte{}))
}
