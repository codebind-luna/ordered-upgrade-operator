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
	"testing"
	"time"

	fuzz "github.com/google/gofuzz"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/diff"

	"github.com/codebind-luna/ordered-upgrade-operator/api/v1beta1"
)

const fuzzIterations = 2000

// newFuzzer fills every field with random data, so a field added to either
// version later is exercised without anyone remembering to write a case for it.
// The custom funcs only pin what the API server itself would normalize: phases
// to values the versions define, times to the second precision JSON carries,
// and metadata to the parts conversion copies.
func newFuzzer(seed int64) *fuzz.Fuzzer {
	return fuzz.NewWithSeed(seed).NilChance(0.2).NumElements(0, 5).Funcs(
		func(tm *metav1.TypeMeta, c fuzz.Continue) {},
		func(om *metav1.ObjectMeta, c fuzz.Continue) {
			om.Name = c.RandString()
			om.Namespace = c.RandString()
			c.Fuzz(&om.Labels)
			c.Fuzz(&om.Annotations)
		},
		func(t *metav1.Time, c fuzz.Continue) {
			*t = metav1.Unix(c.Int63n(1<<32), 0)
		},
		func(p *UpgradePhase, c fuzz.Continue) {
			phases := []UpgradePhase{"", PhasePending, PhaseUpgradingWorkers, PhaseWaitingForWorkers,
				PhaseUpgradingAPI, PhaseWaitingForAPI, PhaseCompleted, PhaseFailed}
			*p = phases[c.Intn(len(phases))]
		},
		func(p *v1beta1.UpgradePhase, c fuzz.Continue) {
			phases := []v1beta1.UpgradePhase{"", v1beta1.PhasePending, v1beta1.PhaseProgressing,
				v1beta1.PhaseCompleted, v1beta1.PhaseFailed}
			*p = phases[c.Intn(len(phases))]
		},
		func(p *v1beta1.ComponentPhase, c fuzz.Continue) {
			phases := []v1beta1.ComponentPhase{"", v1beta1.ComponentPending, v1beta1.ComponentUpgrading,
				v1beta1.ComponentRollingOut, v1beta1.ComponentReady, v1beta1.ComponentFailed}
			*p = phases[c.Intn(len(phases))]
		},
	)
}

// TestRoundTripHubSpokeHub is the property that matters most: an object written
// in v1beta1 and then read-modified-written by a v1alpha1 client without
// changes comes back identical, however little of it v1alpha1 can represent.
func TestRoundTripHubSpokeHub(t *testing.T) {
	f := newFuzzer(time.Now().UnixNano())
	for i := range fuzzIterations {
		orig := &v1beta1.ApplicationUpgrade{}
		f.Fuzz(orig)

		spoke := &ApplicationUpgrade{}
		if err := spoke.ConvertFrom(orig.DeepCopy()); err != nil {
			t.Fatalf("iteration %d: ConvertFrom: %v", i, err)
		}
		back := &v1beta1.ApplicationUpgrade{}
		if err := spoke.ConvertTo(back); err != nil {
			t.Fatalf("iteration %d: ConvertTo: %v", i, err)
		}

		if !equality.Semantic.DeepEqual(orig, back) {
			t.Fatalf("iteration %d: v1beta1 -> v1alpha1 -> v1beta1 lost data:\n%s", i, diff.ObjectReflectDiff(orig, back))
		}
	}
}

// TestRoundTripSpokeHubSpoke covers the other direction: a v1alpha1 object
// survives a trip through storage, and gains no conversion annotation, since an
// exact worker/api pair loses nothing.
func TestRoundTripSpokeHubSpoke(t *testing.T) {
	f := newFuzzer(time.Now().UnixNano())
	for i := range fuzzIterations {
		orig := &ApplicationUpgrade{}
		f.Fuzz(orig)
		delete(orig.Annotations, ConversionDataAnnotation)

		hub := &v1beta1.ApplicationUpgrade{}
		if err := orig.DeepCopy().ConvertTo(hub); err != nil {
			t.Fatalf("iteration %d: ConvertTo: %v", i, err)
		}
		back := &ApplicationUpgrade{}
		if err := back.ConvertFrom(hub); err != nil {
			t.Fatalf("iteration %d: ConvertFrom: %v", i, err)
		}

		if !equality.Semantic.DeepEqual(orig, back) {
			t.Fatalf("iteration %d: v1alpha1 -> v1beta1 -> v1alpha1 lost data:\n%s", i, diff.ObjectReflectDiff(orig, back))
		}
	}
}

// graph is the a -> {b, c}, b -> d example: a calls b and c, b calls d.
func graph() *v1beta1.ApplicationUpgrade {
	comp := func(name string, deps ...string) v1beta1.Component {
		return v1beta1.Component{
			Name:          name,
			DeploymentRef: v1beta1.DeploymentRef{Name: "app-" + name},
			Image:         "app-" + name + ":v2",
			DependsOn:     deps,
		}
	}
	return &v1beta1.ApplicationUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default"},
		Spec: v1beta1.ApplicationUpgradeSpec{Components: []v1beta1.Component{
			comp("a", "b", "c"), comp("b", "d"), comp("c"), comp("d"),
		}},
	}
}

func TestDownConversionOfGraphIsBestEffortWithStash(t *testing.T) {
	spoke := &ApplicationUpgrade{}
	if err := spoke.ConvertFrom(graph()); err != nil {
		t.Fatal(err)
	}

	// Upgrade order is c, d, b, a: worker shows the first, api the last.
	if got := spoke.Spec.Worker.DeploymentRef.Name; got != "app-c" {
		t.Errorf("worker slot = %q, want app-c (first in upgrade order)", got)
	}
	if got := spoke.Spec.API.DeploymentRef.Name; got != "app-a" {
		t.Errorf("api slot = %q, want app-a (last in upgrade order)", got)
	}
	if _, ok := spoke.Annotations[ConversionDataAnnotation]; !ok {
		t.Fatal("a four-component graph must be stashed, v1alpha1 cannot hold it")
	}
}

// TestEditThroughV1alpha1KeepsTheGraph is the read-modify-write case: a
// v1alpha1 client bumps the image it can see. The edit must land on the right
// component, and the components it cannot see must survive.
func TestEditThroughV1alpha1KeepsTheGraph(t *testing.T) {
	orig := graph()

	spoke := &ApplicationUpgrade{}
	if err := spoke.ConvertFrom(orig.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	spoke.Spec.Worker.Image = "app-c:v3"
	spoke.Status.Message = "edited by an old client"

	back := &v1beta1.ApplicationUpgrade{}
	if err := spoke.ConvertTo(back); err != nil {
		t.Fatal(err)
	}

	want := orig.DeepCopy()
	want.Spec.Components[2].Image = "app-c:v3"
	want.Status.Message = "edited by an old client"
	if !equality.Semantic.DeepEqual(want, back) {
		t.Fatalf("edit through v1alpha1 did not merge onto the graph:\n%s", diff.ObjectReflectDiff(want, back))
	}
	if _, ok := back.Annotations[ConversionDataAnnotation]; ok {
		t.Fatal("the conversion annotation must never reach storage")
	}
}

func TestUnreadableStashFallsBackInsteadOfFailing(t *testing.T) {
	spoke := &ApplicationUpgrade{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{ConversionDataAnnotation: "{not json"}},
		Spec: ApplicationUpgradeSpec{
			Worker: ComponentSpec{DeploymentRef: DeploymentRef{Name: "job-worker"}, Image: "w:v2"},
			API:    ComponentSpec{DeploymentRef: DeploymentRef{Name: "job-api"}, Image: "a:v2"},
		},
	}
	hub := &v1beta1.ApplicationUpgrade{}
	if err := spoke.ConvertTo(hub); err != nil {
		t.Fatalf("conversion must not fail on a corrupt annotation: %v", err)
	}
	if n := len(hub.Spec.Components); n != 2 {
		t.Fatalf("got %d components, want the plain worker/api mapping", n)
	}
	if hub.Annotations != nil {
		t.Fatalf("corrupt annotation leaked into the hub: %v", hub.Annotations)
	}
}
