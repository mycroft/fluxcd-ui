package flux

import (
	"slices"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
)

func TestDependenciesOf(t *testing.T) {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: objectMeta("flux-system", "apps"),
		Spec: kustomizev1.KustomizationSpec{DependsOn: []meta.DependencyReference{
			{Name: "infra"},
			{Name: "db", Namespace: "data", ReadyExpr: "status.ready == true"},
		}},
	}
	want := []Dependency{
		{Kind: "kustomizations", Namespace: "flux-system", Name: "infra"},
		{Kind: "kustomizations", Namespace: "data", Name: "db", ReadyExpr: "status.ready == true"},
	}
	if got := DependenciesOf(ks); !slices.Equal(got, want) {
		t.Errorf("Kustomization: %+v", got)
	}
	d := kind(t, "kustomizations").Describe(ks)
	if !d.HasDependencies || !slices.Equal(d.DependsOn, want) {
		t.Errorf("described: has %t, %+v", d.HasDependencies, d.DependsOn)
	}
	for _, f := range d.Fields {
		if f.Label == "Depends on" {
			t.Errorf("dependencies also listed as a field: %+v", f)
		}
	}

	hr := &helmv2.HelmRelease{
		ObjectMeta: objectMeta("web", "frontend"),
		Spec:       helmv2.HelmReleaseSpec{DependsOn: []meta.DependencyReference{{Name: "backend"}}},
	}
	if got := DependenciesOf(hr); !slices.Equal(got, []Dependency{{Kind: "helmreleases", Namespace: "web", Name: "backend"}}) {
		t.Errorf("HelmRelease: %+v", got)
	}
	if d := kind(t, "helmreleases").Describe(hr); !d.HasDependencies {
		t.Error("a HelmRelease can have dependencies")
	}

	git := &sourcev1.GitRepository{ObjectMeta: objectMeta("flux-system", "fleet")}
	if DependenciesOf(git) != nil || kind(t, "gitrepositories").Describe(git).HasDependencies {
		t.Error("a GitRepository has no dependencies")
	}
}

func TestDependencyState(t *testing.T) {
	for _, tt := range []struct {
		dep   Dependency
		state State
		ok    bool
	}{
		{Dependency{Found: true, Status: Status{State: StateReady}}, StateReady, true},
		{Dependency{Found: true, Status: Status{State: StateProgressing}}, StateProgressing, false},
		{Dependency{}, StateFailed, false}, // missing
	} {
		if tt.dep.State() != tt.state || tt.dep.OK() != tt.ok {
			t.Errorf("%+v: state %s, ok %t", tt.dep, tt.dep.State(), tt.dep.OK())
		}
	}
}
