package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

var (
	// ErrSuspended is returned when reconciling a suspended object, which
	// its controller would ignore.
	ErrSuspended = errors.New("object is suspended: resume it first")
	// ErrNotReconciled is returned when reconciling an object that Flux never
	// reconciles, such as an OCI HelmRepository.
	ErrNotReconciled = errors.New("object is not reconciled by Flux")
)

// ObjectRef identifies a Flux object.
type ObjectRef struct {
	Kind      string
	Namespace string
	Name      string
	// Implied marks a HelmRelease's HelmChart in a reconcile plan: it is
	// reconciled as part of the HelmRelease, and authorized through it.
	Implied bool
}

func (r ObjectRef) String() string {
	return r.Kind + " " + r.Namespace + "/" + r.Name
}

// GroupResource returns the API group and resource of the object's kind, as
// used in RBAC rules.
func (r ObjectRef) GroupResource() schema.GroupResource {
	return kindResources[r.Kind]
}

// sourceKinds are the kinds source-controller reconciles on request.
var sourceKinds = map[string]bool{
	sourcev1.GitRepositoryKind:  true,
	sourcev1.OCIRepositoryKind:  true,
	sourcev1.BucketKind:         true,
	sourcev1.HelmRepositoryKind: true,
	sourcev1.HelmChartKind:      true,
}

// SetSuspended suspends or resumes an object, like `flux suspend` and
// `flux resume`. Resuming changes the spec, which makes the controller
// reconcile right away.
func (s *Store) SetSuspended(ctx context.Context, k flux.Kind, namespace, name string, suspend bool) error {
	patch := fmt.Appendf(nil, `{"spec":{"suspend":%t}}`, suspend)
	return s.patch(ctx, ObjectRef{Kind: k.GVK.Kind, Namespace: namespace, Name: name}, patch)
}

// ReconcilePlan returns the objects a reconcile request annotates: sources
// first, the object itself last. As the flux CLI does, a HelmRelease's
// HelmChart is always included. With withSource, the object's sources are
// included too, recursively.
func (s *Store) ReconcilePlan(ctx context.Context, k flux.Kind, namespace, name string, withSource bool) ([]ObjectRef, error) {
	obj := k.NewObject()
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return nil, err
	}
	switch k.Describe(obj).Status.State {
	case flux.StateSuspended:
		return nil, ErrSuspended
	case flux.StateStatic:
		return nil, ErrNotReconciled
	default: // every other state can be reconciled
	}

	refs, err := s.sources(ctx, obj, withSource)
	if err != nil {
		return nil, err
	}
	return append(refs, ObjectRef{Kind: k.GVK.Kind, Namespace: namespace, Name: name}), nil
}

// RequestReconcile requests a reconciliation of each object in order, like
// `flux reconcile`: it sets the reconcile.fluxcd.io/requestedAt annotation.
func (s *Store) RequestReconcile(ctx context.Context, refs []ObjectRef) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{meta.ReconcileRequestAnnotation: time.Now().Format(time.RFC3339Nano)},
		},
	})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.patch(ctx, ref, patch); err != nil {
			return fmt.Errorf("requesting reconciliation of %s: %w", ref, err)
		}
	}
	return nil
}

// sources returns the objects to reconcile before obj, deepest source first.
func (s *Store) sources(ctx context.Context, obj client.Object, withSource bool) ([]ObjectRef, error) {
	switch o := obj.(type) {
	case *kustomizev1.Kustomization:
		if !withSource {
			return nil, nil
		}
		ref := o.Spec.SourceRef
		return sourceRefs(ObjectRef{Kind: ref.Kind, Namespace: cmp.Or(ref.Namespace, o.Namespace), Name: ref.Name}), nil

	case *sourcev1.HelmChart:
		if !withSource {
			return nil, nil
		}
		ref := o.Spec.SourceRef
		return sourceRefs(ObjectRef{Kind: ref.Kind, Namespace: o.Namespace, Name: ref.Name}), nil

	case *helmv2.HelmRelease:
		chart, ok := helmReleaseChart(o)
		chart.Implied = chart.Kind == sourcev1.HelmChartKind
		switch {
		case !ok:
			return nil, nil
		case chart.Kind != sourcev1.HelmChartKind: // an OCIRepository chartRef
			if !withSource {
				return nil, nil
			}
			return sourceRefs(chart), nil
		case !withSource:
			return []ObjectRef{chart}, nil
		}
		hc := &sourcev1.HelmChart{}
		if err := s.reader.Get(ctx, client.ObjectKey{Namespace: chart.Namespace, Name: chart.Name}, hc); err != nil {
			return nil, fmt.Errorf("getting %s: %w", chart, err)
		}
		return append(sourceRefs(ObjectRef{Kind: hc.Spec.SourceRef.Kind, Namespace: hc.Namespace, Name: hc.Spec.SourceRef.Name}), chart), nil
	}
	return nil, nil
}

// helmReleaseChart returns the HelmChart or OCIRepository a HelmRelease
// installs its chart from.
func helmReleaseChart(hr *helmv2.HelmRelease) (ObjectRef, bool) {
	if r := hr.Spec.ChartRef; r != nil {
		return ObjectRef{Kind: r.Kind, Namespace: cmp.Or(r.Namespace, hr.Namespace), Name: r.Name}, true
	}
	if hr.Spec.Chart == nil {
		return ObjectRef{}, false
	}
	// helm-controller records the HelmChart it manages as "namespace/name".
	if ns, name, ok := strings.Cut(hr.Status.HelmChart, "/"); ok {
		return ObjectRef{Kind: sourcev1.HelmChartKind, Namespace: ns, Name: name}, true
	}
	ns := cmp.Or(hr.Spec.Chart.Spec.SourceRef.Namespace, hr.Namespace)
	return ObjectRef{Kind: sourcev1.HelmChartKind, Namespace: ns, Name: hr.Namespace + "-" + hr.Name}, true
}

// sourceRefs keeps ref only if it is a kind source-controller reconciles.
func sourceRefs(ref ObjectRef) []ObjectRef {
	if sourceKinds[ref.Kind] {
		return []ObjectRef{ref}
	}
	return nil
}

// patch applies a JSON merge patch to a Flux object.
func (s *Store) patch(ctx context.Context, ref ObjectRef, patch []byte) error {
	gvk, ok := kindGVKs[ref.Kind]
	if !ok {
		return fmt.Errorf("unsupported kind %q", ref.Kind)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ref.Namespace)
	obj.SetName(ref.Name)
	return s.writer.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

var kindGVKs = func() map[string]schema.GroupVersionKind {
	m := map[string]schema.GroupVersionKind{}
	for _, k := range flux.Kinds {
		m[k.GVK.Kind] = k.GVK
	}
	m[sourcev1.BucketKind] = sourcev1.GroupVersion.WithKind(sourcev1.BucketKind)
	return m
}()

var kindResources = func() map[string]schema.GroupResource {
	m := map[string]schema.GroupResource{}
	for _, k := range flux.Kinds {
		m[k.GVK.Kind] = schema.GroupResource{Group: k.GVK.Group, Resource: k.ID}
	}
	m[sourcev1.BucketKind] = schema.GroupResource{Group: sourcev1.GroupVersion.Group, Resource: "buckets"}
	return m
}()
