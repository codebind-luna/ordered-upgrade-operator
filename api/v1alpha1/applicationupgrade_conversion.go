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
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"github.com/codebind-luna/ordered-upgrade-operator/api/v1beta1"
	"github.com/codebind-luna/ordered-upgrade-operator/internal/dag"
)

// ConversionDataAnnotation carries the v1beta1 spec and status of an object that
// v1alpha1 cannot represent - more than two components, a different dependency
// shape, per-component status. v1alpha1 has exactly two fixed slots and no
// edges, so a v1beta1 graph converted down is lossy; without this annotation a
// v1alpha1 client doing an ordinary read-modify-write would silently truncate
// the graph to two components on the way back up.
//
// The annotation exists only in the v1alpha1 view. It is written on
// down-conversion when, and only when, the plain mapping would lose something,
// and it is always removed on up-conversion, so stored objects never carry it.
const ConversionDataAnnotation = "upgrades.lunadas.dev/conversion-data"

// The component names a v1alpha1 object becomes in v1beta1: worker is the
// callee, so api depends on it.
const (
	LegacyWorkerName = "worker"
	LegacyAPIName    = "api"
)

type conversionData struct {
	Spec   v1beta1.ApplicationUpgradeSpec   `json:"spec"`
	Status v1beta1.ApplicationUpgradeStatus `json:"status"`
}

var _ conversion.Convertible = &ApplicationUpgrade{}

// ConvertTo converts this v1alpha1 object to the v1beta1 hub.
//
// Conversion runs on every read and write that crosses versions, LISTs and
// WATCHes included, so it must never fail: one object that cannot convert would
// break listing the whole resource. Unreadable conversion data is ignored rather
// than reported.
func (src *ApplicationUpgrade) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta1.ApplicationUpgrade)
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()

	data, ok := popConversionData(&dst.ObjectMeta)
	if !ok {
		dst.Spec = upSpec(src.Spec)
		dst.Status = upStatus(src.Status)
		return nil
	}

	// Restore what v1alpha1 could not hold, then apply on top whatever the
	// v1alpha1 client changed. A field that still matches what down-conversion
	// produced from the stash is untouched; one that differs is a real edit and
	// wins over the stash.
	dst.Spec = data.Spec
	dst.Status = data.Status

	workerName, apiName := legacyMapping(data.Spec)
	var expected ApplicationUpgrade
	expected.Spec = downSpec(data.Spec, workerName, apiName)
	expected.Status = downStatus(data.Status, workerName, apiName)

	if src.Spec.Worker != expected.Spec.Worker {
		applyComponentSpec(&dst.Spec, workerName, src.Spec.Worker)
	}
	if src.Spec.API != expected.Spec.API {
		applyComponentSpec(&dst.Spec, apiName, src.Spec.API)
	}

	if src.Status.Phase != expected.Status.Phase {
		dst.Status.Phase = upPhase(src.Status.Phase)
	}
	if src.Status.ObservedGeneration != expected.Status.ObservedGeneration {
		dst.Status.ObservedGeneration = src.Status.ObservedGeneration
	}
	if src.Status.Message != expected.Status.Message {
		dst.Status.Message = src.Status.Message
	}
	if !equality.Semantic.DeepEqual(src.Status.Conditions, expected.Status.Conditions) {
		dst.Status.Conditions = copyConditions(src.Status.Conditions)
	}
	if src.Status.CurrentWorkerImage != expected.Status.CurrentWorkerImage {
		setComponentImage(&dst.Status, workerName, src.Status.CurrentWorkerImage)
	}
	if src.Status.CurrentAPIImage != expected.Status.CurrentAPIImage {
		setComponentImage(&dst.Status, apiName, src.Status.CurrentAPIImage)
	}
	return nil
}

// ConvertFrom converts the v1beta1 hub to this version.
//
// A two-component graph named worker/api, with api depending on worker, maps
// onto v1alpha1 exactly. Anything else is mapped best-effort - worker is the
// first component in upgrade order, api the last - and the full v1beta1 spec and
// status are stashed in ConversionDataAnnotation so the round trip back to
// v1beta1 is lossless.
func (dst *ApplicationUpgrade) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.ApplicationUpgrade)
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	delete(dst.Annotations, ConversionDataAnnotation)

	workerName, apiName := legacyMapping(src.Spec)
	dst.Spec = downSpec(src.Spec, workerName, apiName)
	dst.Status = downStatus(src.Status, workerName, apiName)

	// Stash only when the plain mapping loses something. Deciding by an actual
	// round trip, rather than by a hand-written "is this representable" check,
	// keeps the two from drifting as fields are added to either version.
	if equality.Semantic.DeepEqual(upSpec(dst.Spec), src.Spec) &&
		equality.Semantic.DeepEqual(upStatus(dst.Status), src.Status) {
		if len(dst.Annotations) == 0 {
			dst.Annotations = nil
		}
		return nil
	}

	raw, err := json.Marshal(conversionData{Spec: src.Spec, Status: src.Status})
	if err != nil {
		// Unreachable for these types; degrade to the lossy view rather than
		// failing every read of the object.
		return nil
	}
	if dst.Annotations == nil {
		dst.Annotations = map[string]string{}
	}
	dst.Annotations[ConversionDataAnnotation] = string(raw)
	return nil
}

// popConversionData removes and decodes the stash, if there is one.
func popConversionData(meta *metav1.ObjectMeta) (conversionData, bool) {
	raw, ok := meta.Annotations[ConversionDataAnnotation]
	if !ok {
		return conversionData{}, false
	}
	delete(meta.Annotations, ConversionDataAnnotation)
	if len(meta.Annotations) == 0 {
		meta.Annotations = nil
	}
	var data conversionData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return conversionData{}, false
	}
	return data, true
}

// legacyMapping picks which v1beta1 components the v1alpha1 worker and api
// slots show. An exact worker/api pair maps by role; otherwise worker is the
// first component in upgrade order and api the last, which keeps the slots
// meaningful ("upgraded first", "upgraded last") for a v1alpha1 reader. A graph
// the controller would reject (a cycle) falls back to list order.
func legacyMapping(spec v1beta1.ApplicationUpgradeSpec) (worker, api string) {
	names := make([]string, 0, len(spec.Components))
	deps := make(map[string][]string, len(spec.Components))
	for _, c := range spec.Components {
		names = append(names, c.Name)
		deps[c.Name] = c.DependsOn
	}

	order := names
	if waves, err := dag.Waves(names, deps); err == nil {
		order = nil
		for _, w := range waves {
			order = append(order, w...)
		}
	}

	switch len(order) {
	case 0:
		return "", ""
	case 1:
		return order[0], ""
	default:
		return order[0], order[len(order)-1]
	}
}

func upSpec(in ApplicationUpgradeSpec) v1beta1.ApplicationUpgradeSpec {
	worker := upComponent(LegacyWorkerName, in.Worker)
	api := upComponent(LegacyAPIName, in.API)
	api.DependsOn = []string{LegacyWorkerName}
	return v1beta1.ApplicationUpgradeSpec{Components: []v1beta1.Component{worker, api}}
}

func upComponent(name string, in ComponentSpec) v1beta1.Component {
	return v1beta1.Component{
		Name: name,
		DeploymentRef: v1beta1.DeploymentRef{
			Name:      in.DeploymentRef.Name,
			Namespace: in.DeploymentRef.Namespace,
		},
		Image:         in.Image,
		ContainerName: in.ContainerName,
	}
}

func downSpec(in v1beta1.ApplicationUpgradeSpec, workerName, apiName string) ApplicationUpgradeSpec {
	var out ApplicationUpgradeSpec
	if c := findComponent(in, workerName); c != nil {
		out.Worker = downComponent(*c)
	}
	if c := findComponent(in, apiName); c != nil {
		out.API = downComponent(*c)
	}
	return out
}

func downComponent(in v1beta1.Component) ComponentSpec {
	return ComponentSpec{
		DeploymentRef: DeploymentRef{
			Name:      in.DeploymentRef.Name,
			Namespace: in.DeploymentRef.Namespace,
		},
		Image:         in.Image,
		ContainerName: in.ContainerName,
	}
}

func applyComponentSpec(spec *v1beta1.ApplicationUpgradeSpec, name string, in ComponentSpec) {
	for i := range spec.Components {
		if spec.Components[i].Name == name {
			c := &spec.Components[i]
			c.DeploymentRef = v1beta1.DeploymentRef{Name: in.DeploymentRef.Name, Namespace: in.DeploymentRef.Namespace}
			c.Image = in.Image
			c.ContainerName = in.ContainerName
			return
		}
	}
}

func findComponent(spec v1beta1.ApplicationUpgradeSpec, name string) *v1beta1.Component {
	if name == "" {
		return nil
	}
	for i := range spec.Components {
		if spec.Components[i].Name == name {
			return &spec.Components[i]
		}
	}
	return nil
}

// upStatus maps a v1alpha1 status onto the worker/api components. The v1alpha1
// phase encodes both components' progress in one value, so it is split back out
// here; downPhase is its exact inverse for every phase v1alpha1 defines.
func upStatus(in ApplicationUpgradeStatus) v1beta1.ApplicationUpgradeStatus {
	out := v1beta1.ApplicationUpgradeStatus{
		Phase:              upPhase(in.Phase),
		ObservedGeneration: in.ObservedGeneration,
		Message:            in.Message,
		Conditions:         copyConditions(in.Conditions),
	}
	if in.Phase == "" && in.CurrentWorkerImage == "" && in.CurrentAPIImage == "" {
		return out
	}
	workerPhase, apiPhase := componentPhases(in.Phase)
	out.Components = []v1beta1.ComponentStatus{
		{Name: LegacyWorkerName, Wave: 1, Phase: workerPhase, CurrentImage: in.CurrentWorkerImage},
		{Name: LegacyAPIName, Wave: 2, Phase: apiPhase, CurrentImage: in.CurrentAPIImage},
	}
	return out
}

func downStatus(in v1beta1.ApplicationUpgradeStatus, workerName, apiName string) ApplicationUpgradeStatus {
	worker := findComponentStatus(in, workerName)
	api := findComponentStatus(in, apiName)
	return ApplicationUpgradeStatus{
		Phase:              downPhase(in.Phase, worker.Phase, api.Phase),
		ObservedGeneration: in.ObservedGeneration,
		Message:            in.Message,
		Conditions:         copyConditions(in.Conditions),
		CurrentWorkerImage: worker.CurrentImage,
		CurrentAPIImage:    api.CurrentImage,
	}
}

func findComponentStatus(in v1beta1.ApplicationUpgradeStatus, name string) v1beta1.ComponentStatus {
	if name == "" {
		return v1beta1.ComponentStatus{}
	}
	for _, c := range in.Components {
		if c.Name == name {
			return c
		}
	}
	return v1beta1.ComponentStatus{}
}

func setComponentImage(status *v1beta1.ApplicationUpgradeStatus, name, image string) {
	if name == "" {
		return
	}
	for i := range status.Components {
		if status.Components[i].Name == name {
			status.Components[i].CurrentImage = image
			return
		}
	}
	status.Components = append(status.Components, v1beta1.ComponentStatus{Name: name, CurrentImage: image})
}

// upPhase collapses the four in-flight v1alpha1 phases into Progressing; which
// component is moving is carried by the per-component phases instead. Unknown
// values pass through unchanged.
func upPhase(p UpgradePhase) v1beta1.UpgradePhase {
	switch p {
	case PhaseUpgradingWorkers, PhaseWaitingForWorkers, PhaseUpgradingAPI, PhaseWaitingForAPI:
		return v1beta1.PhaseProgressing
	default:
		return v1beta1.UpgradePhase(p)
	}
}

func componentPhases(p UpgradePhase) (worker, api v1beta1.ComponentPhase) {
	switch p {
	case PhasePending:
		return v1beta1.ComponentPending, v1beta1.ComponentPending
	case PhaseUpgradingWorkers:
		return v1beta1.ComponentUpgrading, v1beta1.ComponentPending
	case PhaseWaitingForWorkers:
		return v1beta1.ComponentRollingOut, v1beta1.ComponentPending
	case PhaseUpgradingAPI:
		return v1beta1.ComponentReady, v1beta1.ComponentUpgrading
	case PhaseWaitingForAPI:
		return v1beta1.ComponentReady, v1beta1.ComponentRollingOut
	case PhaseCompleted:
		return v1beta1.ComponentReady, v1beta1.ComponentReady
	default:
		// Failed does not say which component failed; leave both unknown.
		return "", ""
	}
}

func downPhase(p v1beta1.UpgradePhase, worker, api v1beta1.ComponentPhase) UpgradePhase {
	if p != v1beta1.PhaseProgressing {
		return UpgradePhase(p)
	}
	switch {
	case api == v1beta1.ComponentUpgrading:
		return PhaseUpgradingAPI
	case api == v1beta1.ComponentRollingOut:
		return PhaseWaitingForAPI
	case worker == v1beta1.ComponentUpgrading:
		return PhaseUpgradingWorkers
	default:
		return PhaseWaitingForWorkers
	}
}

func copyConditions(in []metav1.Condition) []metav1.Condition {
	if in == nil {
		return nil
	}
	out := make([]metav1.Condition, len(in))
	copy(out, in)
	return out
}
