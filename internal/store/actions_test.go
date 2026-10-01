package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

func om(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name}
}

func newActionStore(t *testing.T) (*Store, client.Client) {
	t.Helper()
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&sourcev1.GitRepository{ObjectMeta: om("flux-system", "fleet")},
		&sourcev1.HelmRepository{ObjectMeta: om("flux-system", "podinfo")},
		&sourcev1.HelmRepository{ObjectMeta: om("flux-system", "ghcr"), Spec: sourcev1.HelmRepositorySpec{Type: "oci"}},
		&sourcev1.OCIRepository{ObjectMeta: om("flux-system", "podinfo-chart")},
		&sourcev1.HelmChart{
			ObjectMeta: om("flux-system", "apps-podinfo"),
			Spec:       sourcev1.HelmChartSpec{SourceRef: sourcev1.LocalHelmChartSourceReference{Kind: "HelmRepository", Name: "podinfo"}},
		},
		&kustomizev1.Kustomization{
			ObjectMeta: om("flux-system", "apps"),
			Spec:       kustomizev1.KustomizationSpec{SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "GitRepository", Name: "fleet"}},
		},
		&kustomizev1.Kustomization{ObjectMeta: om("flux-system", "paused"), Spec: kustomizev1.KustomizationSpec{Suspend: true}},
		&helmv2.HelmRelease{
			ObjectMeta: om("apps", "podinfo"),
			Spec: helmv2.HelmReleaseSpec{Chart: &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
				Chart: "podinfo", SourceRef: helmv2.CrossNamespaceObjectReference{Kind: "HelmRepository", Name: "podinfo", Namespace: "flux-system"},
			}}},
		},
		&helmv2.HelmRelease{
			ObjectMeta: om("apps", "podinfo-oci"),
			Spec:       helmv2.HelmReleaseSpec{ChartRef: &helmv2.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "podinfo-chart", Namespace: "flux-system"}},
		},
	).Build()
	return New(c, NewBroker(time.Millisecond)), c
}

func kind(t *testing.T, id string) flux.Kind {
	t.Helper()
	k, ok := flux.KindByID(id)
	if !ok {
		t.Fatalf("unknown kind %q", id)
	}
	return k
}

func requestedAt(t *testing.T, c client.Client, obj client.Object) string {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return obj.GetAnnotations()[meta.ReconcileRequestAnnotation]
}

func TestSetSuspended(t *testing.T) {
	s, c := newActionStore(t)
	ctx := context.Background()
	k := kind(t, "kustomizations")

	if err := s.SetSuspended(ctx, k, "flux-system", "apps", true); err != nil {
		t.Fatal(err)
	}
	ks := &kustomizev1.Kustomization{ObjectMeta: om("flux-system", "apps")}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ks), ks); err != nil {
		t.Fatal(err)
	}
	if !ks.Spec.Suspend || ks.Spec.SourceRef.Name != "fleet" {
		t.Fatalf("after suspend: suspend=%t, sourceRef=%+v", ks.Spec.Suspend, ks.Spec.SourceRef)
	}

	if err := s.SetSuspended(ctx, k, "flux-system", "apps", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ks), ks); err != nil {
		t.Fatal(err)
	}
	if ks.Spec.Suspend {
		t.Fatal("still suspended after resume")
	}

	if err := s.SetSuspended(ctx, k, "flux-system", "missing", true); !apierrors.IsNotFound(err) {
		t.Fatalf("suspending a missing object: %v", err)
	}
}

func TestReconcilePlan(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		namespace  string
		object     string
		withSource bool
		want       []string
	}{
		{name: "kustomization", kind: "kustomizations", namespace: "flux-system", object: "apps",
			want: []string{"Kustomization flux-system/apps"}},
		{name: "kustomization with source", kind: "kustomizations", namespace: "flux-system", object: "apps", withSource: true,
			want: []string{"GitRepository flux-system/fleet", "Kustomization flux-system/apps"}},
		{name: "helmrelease always refreshes its chart", kind: "helmreleases", namespace: "apps", object: "podinfo",
			want: []string{"HelmChart flux-system/apps-podinfo (implied)", "HelmRelease apps/podinfo"}},
		{name: "helmrelease with source", kind: "helmreleases", namespace: "apps", object: "podinfo", withSource: true,
			want: []string{"HelmRepository flux-system/podinfo", "HelmChart flux-system/apps-podinfo (implied)", "HelmRelease apps/podinfo"}},
		{name: "helmrelease chartRef", kind: "helmreleases", namespace: "apps", object: "podinfo-oci",
			want: []string{"HelmRelease apps/podinfo-oci"}},
		{name: "helmrelease chartRef with source", kind: "helmreleases", namespace: "apps", object: "podinfo-oci", withSource: true,
			want: []string{"OCIRepository flux-system/podinfo-chart", "HelmRelease apps/podinfo-oci"}},
		{name: "helmchart with source", kind: "helmcharts", namespace: "flux-system", object: "apps-podinfo", withSource: true,
			want: []string{"HelmRepository flux-system/podinfo", "HelmChart flux-system/apps-podinfo"}},
		{name: "source", kind: "gitrepositories", namespace: "flux-system", object: "fleet", withSource: true,
			want: []string{"GitRepository flux-system/fleet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newActionStore(t)
			refs, err := s.ReconcilePlan(context.Background(), kind(t, tt.kind), tt.namespace, tt.object, tt.withSource)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range refs {
				if r.Implied {
					got = append(got, r.String()+" (implied)")
				} else {
					got = append(got, r.String())
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("reconciled %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReconcileAnnotates(t *testing.T) {
	s, c := newActionStore(t)
	before := time.Now()
	refs, err := s.ReconcilePlan(context.Background(), kind(t, "kustomizations"), "flux-system", "apps", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestReconcile(context.Background(), refs); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{
		&kustomizev1.Kustomization{ObjectMeta: om("flux-system", "apps")},
		&sourcev1.GitRepository{ObjectMeta: om("flux-system", "fleet")},
	} {
		got := requestedAt(t, c, obj)
		ts, err := time.Parse(time.RFC3339Nano, got)
		if err != nil || ts.Before(before.Truncate(time.Second)) {
			t.Errorf("%s: requestedAt = %q", obj.GetName(), got)
		}
	}
	// Unrelated objects are left alone.
	if got := requestedAt(t, c, &sourcev1.HelmRepository{ObjectMeta: om("flux-system", "podinfo")}); got != "" {
		t.Errorf("unrelated HelmRepository annotated: %q", got)
	}
}

func TestReconcileRefusals(t *testing.T) {
	s, _ := newActionStore(t)
	ctx := context.Background()
	if _, err := s.ReconcilePlan(ctx, kind(t, "kustomizations"), "flux-system", "paused", false); !errors.Is(err, ErrSuspended) {
		t.Errorf("suspended: %v", err)
	}
	if _, err := s.ReconcilePlan(ctx, kind(t, "helmrepositories"), "flux-system", "ghcr", false); !errors.Is(err, ErrNotReconciled) {
		t.Errorf("static: %v", err)
	}
	if _, err := s.ReconcilePlan(ctx, kind(t, "kustomizations"), "flux-system", "missing", false); !apierrors.IsNotFound(err) {
		t.Errorf("missing: %v", err)
	}
}

func TestGroupResource(t *testing.T) {
	for kind, want := range map[string]string{
		"HelmRelease":   "helmreleases.helm.toolkit.fluxcd.io",
		"Kustomization": "kustomizations.kustomize.toolkit.fluxcd.io",
		"OCIRepository": "ocirepositories.source.toolkit.fluxcd.io",
		"Bucket":        "buckets.source.toolkit.fluxcd.io",
	} {
		if got := (ObjectRef{Kind: kind}).GroupResource().String(); got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
	}
}
