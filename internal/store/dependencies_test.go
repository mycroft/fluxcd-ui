package store

import (
	"context"
	"slices"
	"testing"
	"time"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

func TestResolveDependencies(t *testing.T) {
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	ks := func(namespace, name string, ready metav1.ConditionStatus, deps ...meta.DependencyReference) *kustomizev1.Kustomization {
		return &kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec:       kustomizev1.KustomizationSpec{DependsOn: deps},
			Status:     kustomizev1.KustomizationStatus{Conditions: readyCondition(ready, "msg "+name)},
		}
	}
	apps := ks("flux-system", "apps", metav1.ConditionFalse,
		meta.DependencyReference{Name: "infra"},
		meta.DependencyReference{Name: "gone"},
		meta.DependencyReference{Name: "db", Namespace: "data"},
	)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		apps,
		ks("flux-system", "infra", metav1.ConditionTrue),
		ks("data", "db", metav1.ConditionFalse),
		ks("flux-system", "monitoring", metav1.ConditionTrue, meta.DependencyReference{Name: "apps"}),
		ks("team-a", "web", metav1.ConditionFalse, meta.DependencyReference{Name: "apps", Namespace: "flux-system"}),
		ks("team-a", "apps", metav1.ConditionTrue), // same name, another namespace
		ks("team-a", "api", metav1.ConditionTrue, meta.DependencyReference{Name: "apps"}),
	).Build()
	s := New(c, NewBroker(time.Millisecond), "kustomizations", "gitrepositories")
	k, _ := flux.KindByID("kustomizations")

	d, err := s.Get(context.Background(), k, "flux-system", "apps")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveDependencies(context.Background(), k, &d); err != nil {
		t.Fatal(err)
	}
	type dep struct {
		ref   string
		found bool
		state flux.State
	}
	summarize := func(deps []flux.Dependency) []dep {
		out := make([]dep, len(deps))
		for i, d := range deps {
			out[i] = dep{d.Namespace + "/" + d.Name, d.Found, d.Status.State}
		}
		return out
	}
	if got, want := summarize(d.DependsOn), []dep{
		{"flux-system/infra", true, flux.StateReady},
		{"flux-system/gone", false, ""},
		{"data/db", true, flux.StateFailed},
	}; !slices.Equal(got, want) {
		t.Errorf("depends on %+v\nwant       %+v", got, want)
	}
	// team-a/api depends on team-a/apps, not on this one.
	if got, want := summarize(d.RequiredBy), []dep{
		{"flux-system/monitoring", true, flux.StateReady},
		{"team-a/web", true, flux.StateFailed},
	}; !slices.Equal(got, want) {
		t.Errorf("required by %+v\nwant        %+v", got, want)
	}
	if d.DependsOn[2].Status.Message != "msg db" || d.RequiredBy[0].Kind != "kustomizations" {
		t.Errorf("resolved dependencies lack their details: %+v, %+v", d.DependsOn[2], d.RequiredBy[0])
	}

	// Kinds without dependencies are left alone.
	git, _ := flux.KindByID("gitrepositories")
	empty := flux.Detail{}
	if err := s.ResolveDependencies(context.Background(), git, &empty); err != nil || empty.RequiredBy != nil {
		t.Errorf("GitRepository: %+v, %v", empty.RequiredBy, err)
	}
}
