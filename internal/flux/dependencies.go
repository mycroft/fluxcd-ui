package flux

import (
	"cmp"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Dependency is an object of the same kind that another one waits for
// (spec.dependsOn), or that waits for it. Describe functions set the
// reference; the store resolves its state.
type Dependency struct {
	Kind      string // Kind.ID
	Namespace string
	Name      string
	// ReadyExpr is a custom readiness check, a CEL expression Flux evaluates
	// instead of the dependency's Ready condition.
	ReadyExpr string
	Found     bool
	Status    Status
}

// State is the dependency's state; a missing dependency counts as failed, as
// the object waiting for it cannot proceed.
func (d Dependency) State() State {
	if !d.Found {
		return StateFailed
	}
	return d.Status.State
}

// OK reports whether the dependency needs no attention.
func (d Dependency) OK() bool {
	st := d.State()
	return st == StateReady || st == StateStatic
}

// DependenciesOf returns the objects obj waits for, with their namespace
// defaulted to obj's. Only Kustomizations and HelmReleases have dependencies.
func DependenciesOf(obj client.Object) []Dependency {
	switch o := obj.(type) {
	case *kustomizev1.Kustomization:
		return dependencies("kustomizations", o.Spec.DependsOn, o.Namespace)
	case *helmv2.HelmRelease:
		return dependencies("helmreleases", o.Spec.DependsOn, o.Namespace)
	default:
		return nil
	}
}

func dependencies(kind string, refs []meta.DependencyReference, namespace string) []Dependency {
	if len(refs) == 0 {
		return nil
	}
	deps := make([]Dependency, len(refs))
	for i, r := range refs {
		deps[i] = Dependency{Kind: kind, Namespace: cmp.Or(r.Namespace, namespace), Name: r.Name, ReadyExpr: r.ReadyExpr}
	}
	return deps
}
