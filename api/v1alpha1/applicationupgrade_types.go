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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeploymentRef identifies a Deployment the operator manages.
type DeploymentRef struct {
	// Name of the target Deployment. Must be a valid DNS-1123 subdomain and is
	// immutable once set.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="deploymentRef.name is immutable"
	Name string `json:"name"`

	// Namespace of the target Deployment. Defaults to the namespace of the
	// ApplicationUpgrade when omitted. Setting it requires the operator to be
	// authorized in that namespace (see the design's RBAC section).
	// Must be a valid DNS-1123 label when set, and is immutable once set.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="deploymentRef.namespace is immutable"
	Namespace string `json:"namespace,omitempty"`
}

// ComponentSpec describes the desired image for one component and how to
// locate the container to update.
type ComponentSpec struct {
	// DeploymentRef points at the Deployment backing this component. Its fields
	// are immutable: retargeting an in-flight upgrade at a different Deployment
	// is not allowed. Create a new ApplicationUpgrade instead.
	DeploymentRef DeploymentRef `json:"deploymentRef"`

	// Image is the desired container image, including tag, for this component.
	// The image tag is the version source of truth the operator applies.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// ContainerName selects which container in the Deployment's pod template to
	// patch. It may be omitted only when the pod template has exactly one
	// container; a multi-container template without a name is a configuration
	// error. Must be a valid DNS-1123 label when set.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ContainerName string `json:"containerName,omitempty"`
}

// ApplicationUpgradeSpec defines the desired state of an ApplicationUpgrade.
// +kubebuilder:validation:XValidation:rule="self.worker.deploymentRef.name != self.api.deploymentRef.name",message="worker and api must reference Deployments with different names"
type ApplicationUpgradeSpec struct {
	// Worker is Component B (Job Worker). It is always upgraded first.
	Worker ComponentSpec `json:"worker"`

	// API is Component A (Job API). It is upgraded only after the worker has
	// reached its desired revision and become ready.
	API ComponentSpec `json:"api"`
}

// UpgradePhase is a high-level summary of where an upgrade is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;UpgradingWorkers;WaitingForWorkers;UpgradingAPI;WaitingForAPI;Completed;Failed
type UpgradePhase string

const (
	// PhasePending means the upgrade has been accepted but not yet started.
	PhasePending UpgradePhase = "Pending"
	// PhaseUpgradingWorkers means the worker Deployment has been patched.
	PhaseUpgradingWorkers UpgradePhase = "UpgradingWorkers"
	// PhaseWaitingForWorkers means the operator is waiting for the worker
	// rollout to complete.
	PhaseWaitingForWorkers UpgradePhase = "WaitingForWorkers"
	// PhaseUpgradingAPI means the API Deployment has been patched.
	PhaseUpgradingAPI UpgradePhase = "UpgradingAPI"
	// PhaseWaitingForAPI means the operator is waiting for the API rollout to
	// complete.
	PhaseWaitingForAPI UpgradePhase = "WaitingForAPI"
	// PhaseCompleted means both components are at their desired images and ready.
	PhaseCompleted UpgradePhase = "Completed"
	// PhaseFailed means the upgrade cannot proceed without user intervention.
	PhaseFailed UpgradePhase = "Failed"
)

// Condition types reported on an ApplicationUpgrade.
const (
	// ConditionWorkersReady is true once the worker rollout has fully reconciled.
	ConditionWorkersReady = "WorkersReady"
	// ConditionAPIReady is true once the API rollout has fully reconciled.
	ConditionAPIReady = "APIReady"
	// ConditionProgressing is true while the operator is actively driving an
	// upgrade.
	ConditionProgressing = "Progressing"
	// ConditionFailed is true when the upgrade has entered a terminal failure.
	ConditionFailed = "Failed"
)

// ApplicationUpgradeStatus defines the observed state of an ApplicationUpgrade.
type ApplicationUpgradeStatus struct {
	// Phase is a high-level summary of the upgrade's progress.
	// +optional
	Phase UpgradePhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation this status reflects. It lets
	// the operator detect a spec edited mid-upgrade and re-plan.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentWorkerImage is the worker image currently observed on the
	// Deployment.
	// +optional
	CurrentWorkerImage string `json:"currentWorkerImage,omitempty"`

	// CurrentAPIImage is the API image currently observed on the Deployment.
	// +optional
	CurrentAPIImage string `json:"currentAPIImage,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the latest available observations of the upgrade's
	// state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:resource:shortName=appup
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Worker",type=string,JSONPath=`.status.currentWorkerImage`
//+kubebuilder:printcolumn:name="API",type=string,JSONPath=`.status.currentAPIImage`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApplicationUpgrade is the Schema for the applicationupgrades API.
type ApplicationUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationUpgradeSpec   `json:"spec,omitempty"`
	Status ApplicationUpgradeStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ApplicationUpgradeList contains a list of ApplicationUpgrade.
type ApplicationUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApplicationUpgrade{}, &ApplicationUpgradeList{})
}
