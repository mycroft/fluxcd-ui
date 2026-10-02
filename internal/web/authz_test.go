package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mycroft/fluxcd-ui/internal/authz"
	"github.com/mycroft/fluxcd-ui/internal/store"
)

// fakeAuthorizer allows "<group> <verb> <Kind> <namespace>" combinations.
type fakeAuthorizer map[string]bool

func (f fakeAuthorizer) Allowed(_ context.Context, id authz.Identity, verb string, obj store.ObjectRef, _ bool) (bool, error) {
	for _, g := range id.Groups {
		if f[g+" "+verb+" "+obj.Kind+" "+obj.Namespace] {
			return true, nil
		}
	}
	return false, nil
}

// team-a may reconcile, but not suspend, HelmReleases in "apps".
var teamA = fakeAuthorizer{"team-a reconcile HelmRelease apps": true}

func rbacOptions() Options {
	return Options{Actions: true, UserHeader: "X-Forwarded-User", GroupsHeader: "X-Forwarded-Groups", Authorizer: teamA}
}

var alice = map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "devs, team-a"}

func getAs(t *testing.T, srv http.Handler, url string, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return string(body)
}

func TestDrawerButtonsFollowPermissions(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, rbacOptions())

	body := getAs(t, srv, "/objects/helmreleases/apps/podinfo", alice)
	assertContains(t, body, "/reconcile\"", "/reconcile-with-source")
	assertNotContains(t, body, "/suspend", "not allowed")

	body = getAs(t, srv, "/objects/gitrepositories/flux-system/flux-system", alice)
	assertContains(t, body, "You are not allowed to act on this object.")
	assertNotContains(t, body, "hx-post")

	// No identity at all: read-only.
	body = getAs(t, srv, "/objects/helmreleases/apps/podinfo", nil)
	assertContains(t, body, "You are not allowed to act on this object.")
}

func TestReleaseContentFollowsPermissions(t *testing.T) {
	opts := Options{UserHeader: "X-Forwarded-User", GroupsHeader: "X-Forwarded-Groups", ReleaseContent: true,
		Authorizer: fakeAuthorizer{"team-a inspect HelmRelease apps": true}}
	srv, _, _ := newTestServerWith(t, false, opts)
	bob := map[string]string{"X-Forwarded-User": "bob", "X-Forwarded-Groups": "devs"}
	release := func(headers map[string]string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/objects/helmreleases/apps/podinfo/release", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		return rec.Code, string(body)
	}

	// Inspecting needs no actions, only the verb.
	body := getAs(t, srv, "/objects/helmreleases/apps/podinfo", alice)
	assertContains(t, body, `hx-get="/objects/helmreleases/apps/podinfo/release"`)
	if code, body := release(alice); code != http.StatusOK || !strings.Contains(body, "replicaCount: 2") {
		t.Errorf("alice: %d %s", code, body)
	}

	body = getAs(t, srv, "/objects/helmreleases/apps/podinfo", bob)
	assertContains(t, body, "You are not allowed to inspect this release.")
	assertNotContains(t, body, "/release\"")
	code, body := release(bob)
	if code != http.StatusForbidden {
		t.Errorf("bob: status = %d", code)
	}
	assertContains(t, body, `bob is not allowed to inspect HelmRelease apps/podinfo`)
	assertNotContains(t, body, "replicaCount")

	code, body = release(nil)
	if code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d", code)
	}
	assertContains(t, body, "not authenticated: the X-Forwarded-User header is missing")
}

func TestActionsFollowPermissions(t *testing.T) {
	srv, _, c := newTestServerWith(t, false, rbacOptions())

	res, body := post(t, srv, "/objects/helmreleases/apps/podinfo/suspend", alice)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("suspend: status = %d", res.StatusCode)
	}
	assertContains(t, body, `alice is not allowed to suspend HelmRelease apps/podinfo (needs the &#34;suspend&#34; verb on helmreleases.helm.toolkit.fluxcd.io)`)
	hr := &helmv2.HelmRelease{}
	getObject(t, c, hr, "apps", "podinfo")
	if hr.Spec.Suspend {
		t.Fatal("denied suspend was applied")
	}

	if res, body := post(t, srv, "/objects/helmreleases/apps/podinfo/reconcile", alice); res.StatusCode != http.StatusOK {
		t.Fatalf("reconcile: status = %d: %s", res.StatusCode, body)
	}

	bob := map[string]string{"X-Forwarded-User": "bob", "X-Forwarded-Groups": "devs"}
	if res, _ := post(t, srv, "/objects/helmreleases/apps/podinfo/reconcile", bob); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bob reconcile: status = %d", res.StatusCode)
	}
}

func TestReconcileAuthorizesSourcesButNotImpliedCharts(t *testing.T) {
	// A HelmRelease in "apps" whose HelmChart and HelmRepository live in
	// flux-system, where team-a has no permissions.
	srv, _, c := newTestServerWith(t, false, rbacOptions(),
		&sourcev1.HelmChart{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "apps-web"},
			Spec:       sourcev1.HelmChartSpec{Chart: "web", SourceRef: sourcev1.LocalHelmChartSourceReference{Kind: "HelmRepository", Name: "charts"}},
		},
		&helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
			Spec: helmv2.HelmReleaseSpec{Chart: &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
				Chart: "web", SourceRef: helmv2.CrossNamespaceObjectReference{Kind: "HelmRepository", Name: "charts", Namespace: "flux-system"},
			}}},
			Status: helmv2.HelmReleaseStatus{HelmChart: "flux-system/apps-web"},
		},
	)

	// The HelmChart is reconciled as part of the HelmRelease: allowed.
	res, body := post(t, srv, "/objects/helmreleases/apps/web/reconcile", alice)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reconcile: status = %d: %s", res.StatusCode, body)
	}
	assertContains(t, body, "HelmChart flux-system/apps-web → HelmRelease apps/web")
	chart := &sourcev1.HelmChart{}
	getObject(t, c, chart, "flux-system", "apps-web")
	if chart.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Fatal("implied HelmChart not reconciled")
	}

	// Refreshing the HelmRepository needs its own permission.
	res, body = post(t, srv, "/objects/helmreleases/apps/web/reconcile-with-source", alice)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("reconcile with source: status = %d", res.StatusCode)
	}
	assertContains(t, body, "alice is not allowed to reconcile HelmRepository flux-system/charts")
}

func TestGroupsSeparator(t *testing.T) {
	opts := rbacOptions()
	opts.GroupsSeparator = "|"
	srv, _, _ := newTestServerWith(t, false, opts)
	headers := map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "devs|team-a"}
	if res, body := post(t, srv, "/objects/helmreleases/apps/podinfo/reconcile", headers); res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
}
