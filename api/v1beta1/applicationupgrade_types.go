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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeploymentRef identifies a Deployment the operator manages.
// The whole ref is immutable, not just its fields: a field-level transition rule
// only runs when the field exists on both sides, so it cannot catch an optional
// namespace being removed.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="deploymentRef is immutable"
type DeploymentRef struct {
	// Name of the target Deployment. Must be a valid DNS-1123 subdomain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Namespace of the target Deployment. Defaults to the namespace of the
	// ApplicationUpgrade when omitted. Setting it requires the operator to be
	// authorized in that namespace (see the design's RBAC section).
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// Component is one Deployment in the upgrade, and the components it calls.
type Component struct {
	// Name identifies the component within this upgrade and is what other
	// components list in dependsOn. Must be a valid DNS-1123 label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// DeploymentRef points at the Deployment backing this component. It is
	// immutable: retargeting an in-flight upgrade at a different Deployment is
	// not allowed. Create a new ApplicationUpgrade instead.
	DeploymentRef DeploymentRef `json:"deploymentRef"`

	// Image is the desired container image, including tag, for this component.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// ContainerName selects which container in the Deployment's pod template to
	// patch. It may be omitted only when the pod template has exactly one
	// container. Must be a valid DNS-1123 label when set.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ContainerName string `json:"containerName,omitempty"`

	// DependsOn names the components this one calls. Every component listed here
	// is fully upgraded - all of its old pods gone - before this one starts, so
	// a new caller never reaches an old callee. Application developers guarantee
	// the other direction: a new callee accepts requests from the old caller.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=15
	// +kubebuilder:validation:items:MaxLength=63
	DependsOn []string `json:"dependsOn,omitempty"`
}

// ApplicationUpgradeSpec defines the desired state of an ApplicationUpgrade.
//
// Cycle detection is deliberately not a CEL rule: it needs a graph traversal,
// which CEL cannot express. The controller rejects a cyclic graph as a
// configuration error before patching anything.
// +kubebuilder:validation:XValidation:rule="self.components.all(c, !has(c.dependsOn) || c.dependsOn.all(d, d != c.name && self.components.exists(x, x.name == d)))",message="dependsOn must name other components in this upgrade"
// +kubebuilder:validation:XValidation:rule="self.components.all(c, self.components.exists_one(x, x.deploymentRef.name == c.deploymentRef.name && has(x.deploymentRef.__namespace__) == has(c.deploymentRef.__namespace__) && (!has(x.deploymentRef.__namespace__) || x.deploymentRef.__namespace__ == c.deploymentRef.__namespace__)))",message="each component must reference a different Deployment"
type ApplicationUpgradeSpec struct {
	// Components to upgrade. A component is upgraded only after every component
	// it depends on has completed its rollout.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Components []Component `json:"components"`
}

// UpgradePhase is a high-level summary of where an upgrade is in its lifecycle.
// It is reported, not validated: clients must tolerate values added in later
// releases, and should prefer conditions for anything they act on.
type UpgradePhase string

const (
	// PhasePending means the upgrade has been accepted but not yet started.
	PhasePending UpgradePhase = "Pending"
	// PhaseProgressing means at least one component is being upgraded.
	PhaseProgressing UpgradePhase = "Progressing"
	// PhaseCompleted means every component is at its desired image and ready.
	PhaseCompleted UpgradePhase = "Completed"
	// PhaseFailed means the upgrade cannot proceed without user intervention.
	PhaseFailed UpgradePhase = "Failed"
)

// ComponentPhase summarizes where one component is in its upgrade.
type ComponentPhase string

const (
	// ComponentPending means the component is waiting for its dependencies.
	ComponentPending ComponentPhase = "Pending"
	// ComponentUpgrading means the image was patched in the latest pass and the
	// Deployment controller has not yet reported on the new revision.
	ComponentUpgrading ComponentPhase = "Upgrading"
	// ComponentRollingOut means the new revision is rolling out.
	ComponentRollingOut ComponentPhase = "RollingOut"
	// ComponentReady means the component is at its desired image and no
	// old-revision pod remains.
	ComponentReady ComponentPhase = "Ready"
	// ComponentFailed means the component's rollout exceeded its progress
	// deadline.
	ComponentFailed ComponentPhase = "Failed"
)

// Condition types reported on an ApplicationUpgrade.
const (
	// ConditionReady is true once every component has fully rolled out.
	ConditionReady = "Ready"
	// ConditionProgressing is true while the operator is actively driving an
	// upgrade.
	ConditionProgressing = "Progressing"
	// ConditionFailed is true when the upgrade is blocked on a failure.
	ConditionFailed = "Failed"
)

// ComponentStatus is the observed state of one component.
type ComponentStatus struct {
	// Name of the component this status describes.
	Name string `json:"name"`

	// Wave is the component's 1-based position in the upgrade order: every
	// component in wave n depends only on components in earlier waves.
	// +optional
	Wave int32 `json:"wave,omitempty"`

	// Phase summarizes where this component is in its upgrade.
	// +optional
	Phase ComponentPhase `json:"phase,omitempty"`

	// CurrentImage is the image currently observed on the Deployment.
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`
}

// ApplicationUpgradeStatus defines the observed state of an ApplicationUpgrade.
type ApplicationUpgradeStatus struct {
	// Phase is a high-level summary of the upgrade's progress.
	// +optional
	Phase UpgradePhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// Components reports each component's progress.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ComponentStatus `json:"components,omitempty"`

	// Conditions represent the latest available observations of the upgrade's
	// state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:resource:shortName=appup
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApplicationUpgrade is the Schema for the applicationupgrades API.
type ApplicationUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationUpgradeSpec   `json:"spec,omitempty"`
	Status ApplicationUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationUpgradeList contains a list of ApplicationUpgrade.
type ApplicationUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApplicationUpgrade{}, &ApplicationUpgradeList{})
}
