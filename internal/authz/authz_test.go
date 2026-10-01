package authz

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/mycroft/fluxcd-ui/internal/store"
)

// fakeAPIServer answers SubjectAccessReviews: only group "ui:operators" may
// reconcile HelmReleases in namespace "apps".
type fakeAPIServer struct {
	reviews []authorizationv1.SubjectAccessReviewSpec
	err     error
}

func (f *fakeAPIServer) client() client.Client {
	return fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			if f.err != nil {
				return f.err
			}
			sar := obj.(*authorizationv1.SubjectAccessReview)
			f.reviews = append(f.reviews, sar.Spec)
			ra := sar.Spec.ResourceAttributes
			sar.Status.Allowed = slices.Contains(sar.Spec.Groups, "ui:operators") &&
				ra.Verb == VerbReconcile && ra.Group == "helm.toolkit.fluxcd.io" && ra.Resource == "helmreleases" && ra.Namespace == "apps"
			return nil
		},
	}).Build()
}

var podinfo = store.ObjectRef{Kind: "HelmRelease", Namespace: "apps", Name: "podinfo"}

func TestRBAC(t *testing.T) {
	api := &fakeAPIServer{}
	a, err := NewRBAC(api.client(), "ui:", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := Identity{User: "alice", Groups: []string{"operators", "devs"}}

	tests := []struct {
		id   Identity
		verb string
		obj  store.ObjectRef
		want bool
	}{
		{alice, VerbReconcile, podinfo, true},
		{alice, VerbSuspend, podinfo, false},
		{alice, VerbReconcile, store.ObjectRef{Kind: "HelmRelease", Namespace: "other", Name: "x"}, false},
		{Identity{User: "bob", Groups: []string{"devs"}}, VerbReconcile, podinfo, false},
		{Identity{}, VerbReconcile, podinfo, false}, // no identity: never asks
	}
	for _, tt := range tests {
		got, err := a.Allowed(ctx, tt.id, tt.verb, tt.obj, false)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("%s %s %s: allowed = %t, want %t", tt.id.User, tt.verb, tt.obj, got, tt.want)
		}
	}

	// The review carries prefixed names and the object's RBAC coordinates.
	spec := api.reviews[0]
	if spec.User != "ui:alice" || !slices.Equal(spec.Groups, []string{"ui:devs", "ui:operators"}) {
		t.Errorf("subject = %q %q", spec.User, spec.Groups)
	}
	if ra := spec.ResourceAttributes; ra.Name != "podinfo" || ra.Namespace != "apps" || ra.Resource != "helmreleases" {
		t.Errorf("resource attributes = %+v", ra)
	}
	if len(api.reviews) != 4 {
		t.Errorf("%d reviews, want 4 (none without an identity)", len(api.reviews))
	}
}

func TestRBACCache(t *testing.T) {
	api := &fakeAPIServer{}
	a, err := NewRBAC(api.client(), "ui:", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.now = func() time.Time { return now }
	ctx := context.Background()
	alice := Identity{User: "alice", Groups: []string{"operators"}}

	for range 3 {
		if _, err := a.Allowed(ctx, alice, VerbReconcile, podinfo, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.reviews) != 1 {
		t.Fatalf("%d reviews for 3 cached checks", len(api.reviews))
	}
	if _, err := a.Allowed(ctx, alice, VerbReconcile, podinfo, true); err != nil {
		t.Fatal(err)
	}
	if len(api.reviews) != 2 {
		t.Fatal("a fresh check used the cache")
	}
	now = now.Add(2 * time.Minute)
	if _, err := a.Allowed(ctx, alice, VerbReconcile, podinfo, false); err != nil {
		t.Fatal(err)
	}
	if len(api.reviews) != 3 {
		t.Fatal("an expired answer was reused")
	}
}

func TestRBACErrorDenies(t *testing.T) {
	api := &fakeAPIServer{err: errors.New("connection refused")}
	a, _ := NewRBAC(api.client(), "ui:", time.Minute)
	ok, err := a.Allowed(context.Background(), Identity{User: "alice"}, VerbReconcile, podinfo, false)
	if ok || err == nil {
		t.Fatalf("allowed = %t, err = %v", ok, err)
	}
}

func TestValidatePrefix(t *testing.T) {
	for prefix, valid := range map[string]bool{"": false, "system:": false, "system:oidc:": false, "oidc:": true, "fluxcd-ui:": true} {
		if err := ValidatePrefix(prefix); (err == nil) != valid {
			t.Errorf("ValidatePrefix(%q) = %v", prefix, err)
		}
	}
	if _, err := NewRBAC(nil, "", time.Minute); err == nil {
		t.Error("NewRBAC accepted an empty prefix")
	}
}
