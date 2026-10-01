package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

func deployment(name string, replicas, updated int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
		Status: appsv1.DeploymentStatus{
			Replicas: replicas, UpdatedReplicas: updated, ReadyReplicas: updated, AvailableReplicas: updated,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
}

func entry(group, version, kind, namespace, name, kindID string) flux.InventoryEntry {
	return flux.InventoryEntry{Group: group, Version: version, Kind: kind, Namespace: namespace, Name: name, KindID: kindID}
}

func newHealthStore(t *testing.T, lists *atomic.Int32) *Store {
	t.Helper()
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		deployment("web", 1, 1),
		deployment("rolling", 2, 1),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "cfg"}},
		&helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "podinfo"},
			Status: helmv2.HelmReleaseStatus{Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "InstallFailed", Message: "install retries exhausted"},
			}},
		},
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			if list.GetObjectKind().GroupVersionKind().Group == "traefik.io" {
				return apierrors.NewForbidden(schema.GroupResource{Group: "traefik.io", Resource: "middlewares"}, "", nil)
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	return New(c, NewBroker(time.Millisecond), "helmreleases")
}

func TestManagedHealth(t *testing.T) {
	var lists atomic.Int32
	s := newHealthStore(t, &lists)
	entries := []flux.InventoryEntry{
		entry("apps", "v1", "Deployment", "apps", "web", ""),
		entry("apps", "v1", "Deployment", "apps", "rolling", ""),
		entry("", "v1", "ConfigMap", "apps", "cfg", ""),
		entry("", "v1", "Service", "apps", "gone", ""),
		entry("traefik.io", "v1alpha1", "Middleware", "apps", "auth", ""),
		entry("helm.toolkit.fluxcd.io", "v2", "HelmRelease", "apps", "podinfo", "helmreleases"),
	}

	got := s.ManagedHealth(context.Background(), entries)
	want := map[string]Health{
		"web": HealthReady, "rolling": HealthProgressing, "cfg": HealthReady,
		"gone": HealthMissing, "auth": HealthNoAccess, "podinfo": HealthFailed,
	}
	for _, e := range entries {
		h := got[HealthKey(e)]
		if h.Health != want[e.Name] {
			t.Errorf("%s %s: %s (%s), want %s", e.Kind, e.Name, h.Health, h.Message, want[e.Name])
		}
	}
	if m := got[HealthKey(entries[5])].Message; m != "install retries exhausted" {
		t.Errorf("Flux object message = %q", m)
	}
	// One list per kind and namespace: Deployments, ConfigMaps, Services, Middlewares.
	if n := lists.Load(); n != 4 {
		t.Errorf("%d list calls, want 4", n)
	}

	// Answers are cached briefly.
	s.ManagedHealth(context.Background(), entries)
	if n := lists.Load(); n != 4 {
		t.Errorf("%d list calls after a cached check, want 4", n)
	}
}

func TestHealthProblem(t *testing.T) {
	for h, problem := range map[Health]bool{
		HealthReady: false, HealthSuspended: false, HealthUnknown: false, HealthNoAccess: false,
		HealthFailed: true, HealthMissing: true, HealthProgressing: true, HealthTerminating: true,
	} {
		if (ObjectHealth{Health: h}).Problem() != problem {
			t.Errorf("%s: problem = %t", h, !problem)
		}
	}
}
