package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func post(t *testing.T, h http.Handler, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func getObject(t *testing.T, c client.Client, obj client.Object, namespace, name string) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
}

const gitRepoURL = "/objects/gitrepositories/flux-system/flux-system"

func TestActionsDisabled(t *testing.T) {
	srv, _ := newTestServer(t, false)

	_, body := get(t, srv, gitRepoURL)
	assertNotContains(t, body, "hx-post")

	res, body := post(t, srv, gitRepoURL+"/reconcile", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, "Actions are disabled", `hx-swap-oob="beforeend"`)
}

func TestActionButtons(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{Actions: true})

	_, body := get(t, srv, gitRepoURL)
	assertContains(t, body, `hx-post="/objects/gitrepositories/flux-system/flux-system/reconcile"`, "/suspend")
	assertNotContains(t, body, "/reconcile-with-source", "/resume") // a GitRepository has no source

	_, body = get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertContains(t, body, "/reconcile-with-source", "/suspend")

	_, body = get(t, srv, "/objects/kustomizations/flux-system/apps") // suspended
	assertContains(t, body, "/resume")
	assertNotContains(t, body, "/reconcile", "/suspend")
}

func TestReconcileAction(t *testing.T) {
	srv, _, c := newTestServerWith(t, false, Options{Actions: true})

	res, body := post(t, srv, gitRepoURL+"/reconcile", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	assertContains(t, body, "Reconciliation requested: GitRepository flux-system/flux-system.", `role="status"`)

	repo := &sourcev1.GitRepository{}
	getObject(t, c, repo, "flux-system", "flux-system")
	if repo.Annotations[meta.ReconcileRequestAnnotation] == "" {
		t.Fatal("reconcile annotation not set")
	}

	// Until the controller handles it, the request shows as pending.
	_, body = get(t, srv, "/fragments/rows/gitrepositories")
	assertContains(t, body, "reconcile requested")
}

func TestSuspendResumeActions(t *testing.T) {
	srv, _, c := newTestServerWith(t, false, Options{Actions: true})
	ks := &kustomizev1.Kustomization{}

	if res, body := post(t, srv, "/objects/kustomizations/flux-system/apps/resume", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d: %s", res.StatusCode, body)
	}
	getObject(t, c, ks, "flux-system", "apps")
	if ks.Spec.Suspend {
		t.Fatal("still suspended after resume")
	}

	res, body := post(t, srv, "/objects/kustomizations/flux-system/apps/suspend", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("suspend status = %d: %s", res.StatusCode, body)
	}
	assertContains(t, body, "Suspended Kustomization flux-system/apps.")
	getObject(t, c, ks, "flux-system", "apps")
	if !ks.Spec.Suspend {
		t.Fatal("not suspended")
	}
}

func TestActionErrors(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{Actions: true})
	tests := []struct {
		url    string
		status int
		body   string
	}{
		{"/objects/kustomizations/flux-system/apps/reconcile", http.StatusConflict, "object is suspended"},
		{"/objects/gitrepositories/flux-system/missing/suspend", http.StatusNotFound, `role="alert"`},
		{"/objects/gitrepositories/flux-system/flux-system/delete", http.StatusNotFound, ""},
		{"/objects/nope/flux-system/flux-system/reconcile", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		res, body := post(t, srv, tt.url, nil)
		if res.StatusCode != tt.status {
			t.Errorf("%s: status = %d, want %d", tt.url, res.StatusCode, tt.status)
		}
		assertContains(t, body, tt.body)
	}
}

func TestActionsRejectCrossOrigin(t *testing.T) {
	srv, _, c := newTestServerWith(t, false, Options{Actions: true})
	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example.com"},
	} {
		if res, _ := post(t, srv, gitRepoURL+"/suspend", headers); res.StatusCode != http.StatusForbidden {
			t.Errorf("%v: status = %d, want 403", headers, res.StatusCode)
		}
	}
	repo := &sourcev1.GitRepository{}
	getObject(t, c, repo, "flux-system", "flux-system")
	if repo.Spec.Suspend {
		t.Fatal("cross-origin request suspended the object")
	}
}

func TestUserHeader(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{Actions: true, UserHeader: "X-Auth-Request-User"})

	res, body := post(t, srv, gitRepoURL+"/reconcile", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without identity: status = %d", res.StatusCode)
	}
	assertContains(t, body, "X-Auth-Request-User header is missing")

	if res, _ := post(t, srv, gitRepoURL+"/reconcile", map[string]string{"X-Auth-Request-User": "alice"}); res.StatusCode != http.StatusOK {
		t.Fatalf("with identity: status = %d", res.StatusCode)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-User", "alice")
	srv.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	assertContains(t, string(b), `title="Signed in"`, "alice")
}
