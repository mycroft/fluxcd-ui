package web

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/artifact"
	"github.com/mycroft/fluxcd-ui/internal/diff"
	"github.com/mycroft/fluxcd-ui/internal/flux"
	"github.com/mycroft/fluxcd-ui/internal/store"
)

func ready() []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded", Message: "all good"}}
}

func failed(msg string) []metav1.Condition {
	return []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Failed", Message: msg}}
}

func fixtures() []client.Object {
	return []client.Object{
		&sourcev1.GitRepository{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "flux-system"},
			Spec:       sourcev1.GitRepositorySpec{URL: "ssh://git@example.com/fleet.git"},
			Status: sourcev1.GitRepositoryStatus{
				Conditions: ready(),
				Artifact:   &meta.Artifact{Revision: "main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc"},
			},
		},
		&helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "podinfo"},
			Spec:       helmv2.HelmReleaseSpec{ChartRef: &helmv2.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "podinfo"}},
			Status:     helmv2.HelmReleaseStatus{Conditions: failed("install retries exhausted")},
		},
		&helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "grafana"},
			Status:     helmv2.HelmReleaseStatus{Conditions: ready()},
		},
		&kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "apps"},
			Spec:       kustomizev1.KustomizationSpec{Suspend: true},
		},
		&sourcev1.HelmRepository{ // static: never reconciled
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "ghcr"},
			Spec:       sourcev1.HelmRepositorySpec{URL: "oci://ghcr.io/charts", Type: "oci"},
		},
	}
}

type testBackend struct {
	*store.Store
	notReady bool
}

func (b testBackend) Ready() bool { return !b.notReady && b.Store.Ready() }

// testLogs is what testBackend serves as controller logs.
var testLogs = []store.LogLine{
	{Time: time.Date(2026, 10, 1, 20, 0, 2, 0, time.UTC), Level: "error", Message: "Reconciler error", Error: "install retries exhausted"},
	{Time: time.Date(2026, 10, 1, 20, 0, 1, 0, time.UTC), Level: "info", Message: "running install action"},
}

// testDiff is what testBackend serves as the diff of flux-system/apps.
var testDiff = &diff.Result{
	Revision:        "main@sha1:abcdef1234567890abcdef1234567890abcdef12",
	AppliedRevision: "main@sha1:0000000111111122222223333333444444455555",
	Counts:          map[diff.Action]int{diff.ActionChanged: 1, diff.ActionCreated: 1, diff.ActionUnchanged: 3},
	Objects: []diff.Object{
		{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "apps", Name: "podinfo", Action: diff.ActionChanged,
			Changes: []diff.Change{{Path: "spec.replicas", Live: "3", Desired: "2"}}},
		{APIVersion: "v1", Kind: "Service", Namespace: "apps", Name: "podinfo", Action: diff.ActionCreated},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "apps", Name: "untouched", Action: diff.ActionUnchanged},
	},
}

func (b testBackend) DiffKustomization(_ context.Context, namespace, name string) (*diff.Result, error) {
	if namespace == "flux-system" && name == "apps" {
		return testDiff, nil
	}
	return nil, errors.New("downloading the source artifact: connection refused")
}

// testArtifactFiles is what testBackend serves as an artifact's files.
var testArtifactFiles = []artifact.File{
	{Path: "README.md", Size: 120},
	{Path: "apps/deployment.yaml", Size: 2048},
	{Path: "apps/base/kustomization.yaml", Size: 64},
}

func (b testBackend) ArtifactFiles(_ context.Context, k flux.Kind, _, _ string) (*meta.Artifact, []artifact.File, error) {
	if k.ID != "gitrepositories" {
		return nil, nil, errors.New("HelmRelease apps/podinfo has no artifact")
	}
	return &meta.Artifact{Revision: "main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc"}, testArtifactFiles, nil
}

func (b testBackend) ArtifactFile(_ context.Context, _ flux.Kind, _, _, path string) (*artifact.Content, error) {
	switch path {
	case "apps/deployment.yaml":
		return &artifact.Content{File: artifact.File{Path: path, Size: 2048}, Text: "kind: Deployment\n# <script>alert(1)</script>\n"}, nil
	case "logo.png":
		return &artifact.Content{File: artifact.File{Path: path, Size: 10}, Binary: true}, nil
	default:
		return nil, artifact.ErrNotFound
	}
}

func (b testBackend) ControllerLogs(_ context.Context, k flux.Kind, namespace, name string) ([]store.LogLine, error) {
	if k.ID == "helmreleases" && namespace == "apps" && name == "podinfo" {
		return testLogs, nil
	}
	return nil, errors.New(`pods is forbidden: cannot list resource "pods"`)
}

// testRelease is what testBackend serves as apps/podinfo's Helm release.
var testRelease = &store.ReleaseContent{
	Name: "podinfo", Namespace: "apps", Revision: 3, Chart: "podinfo@6.5.0", Status: "deployed",
	Values:   "replicaCount: 2\n# <script>alert(1)</script>\n",
	Manifest: "---\nkind: Deployment\n",
}

func (b testBackend) HelmReleaseContent(_ context.Context, namespace, name string) (*store.ReleaseContent, error) {
	if namespace == "apps" && name == "podinfo" {
		return testRelease, nil
	}
	return nil, store.ErrNoRelease
}

func newTestServer(t *testing.T, notReady bool) (*Server, *store.Broker) {
	t.Helper()
	srv, broker, _ := newTestServerWith(t, notReady, Options{Version: "test"})
	return srv, broker
}

func newTestServerWith(t *testing.T, notReady bool, opts Options, extra ...client.Object) (*Server, *store.Broker, client.Client) {
	t.Helper()
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(fixtures(), extra...)...).Build()
	broker := store.NewBroker(time.Millisecond)
	// OCIRepositories are left out to exercise the "not installed" path.
	st := store.New(c, broker, "gitrepositories", "helmrepositories", "helmcharts", "helmreleases", "kustomizations")
	srv, err := New(testBackend{Store: st, notReady: notReady}, broker, opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return srv, broker, c
}

func get(t *testing.T, h http.Handler, url string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func assertContains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body does not contain %q", w)
		}
	}
}

func assertNotContains(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("body unexpectedly contains %q", u)
		}
	}
}

func TestIndex(t *testing.T) {
	srv, _ := newTestServer(t, false)
	res, body := get(t, srv, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	assertContains(t, body,
		`sse-connect="/events"`,
		`hx-trigger="sse:helmreleases"`,
		`<span class="font-medium">flux-system</span>`,
		`title="main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc">main@cb89c25<`,
		"install retries exhausted", // failed rows surface their message
		"1 failing",
		"Not installed: the cluster does not serve",
		`<option value="monitoring">monitoring</option>`,
		`/static/css/app.css?v=test`,
	)
}

func TestFilters(t *testing.T) {
	srv, _ := newTestServer(t, false)

	_, body := get(t, srv, "/?status=failing")
	assertContains(t, body, `value="failing" class="peer sr-only" checked`, ">podinfo<", "No matching objects.")
	assertNotContains(t, body, ">grafana<")

	_, body = get(t, srv, "/?ns=monitoring")
	assertContains(t, body, ">grafana<", `<option value="monitoring" selected>`)
	assertNotContains(t, body, ">podinfo<")

	_, body = get(t, srv, "/?q=FLEET")
	assertContains(t, body, "ssh://git@example.com/fleet.git", `id="section-gitrepositories"`)
	assertNotContains(t, body, ">podinfo<", ">grafana<")
}

func TestSectionsCollapseWhenFilteredEmpty(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/?q=podinfo")
	assertContains(t, body, `id="section-helmreleases" class="group/section overflow-hidden rounded-xl border border-slate-200 bg-white shadow-xs dark:border-slate-800 dark:bg-slate-900" open>`)
	assertNotContains(t, body, `id="section-gitrepositories" class="group/section overflow-hidden rounded-xl border border-slate-200 bg-white shadow-xs dark:border-slate-800 dark:bg-slate-900" open>`)
}

func TestContentFragment(t *testing.T) {
	srv, _ := newTestServer(t, false)
	res, body := get(t, srv, "/fragments/content?q=podinfo&ns=&status=")
	if got := res.Header.Get("Hx-Push-Url"); got != "/?q=podinfo" {
		t.Errorf("HX-Push-Url = %q", got)
	}
	assertContains(t, body, `id="summary"`, `hx-swap-oob="true"`, ">podinfo<")
	assertNotContains(t, body, "<html")
}

func TestRowsFragment(t *testing.T) {
	srv, _ := newTestServer(t, false)

	res, body := get(t, srv, "/fragments/rows/helmreleases?status=failing")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.HasPrefix(body, `<tbody id="rows-helmreleases"`) {
		t.Errorf("fragment does not start with the tbody: %.60q", body)
	}
	assertContains(t, body, ">podinfo<", `id="count-helmreleases" class="flex items-center gap-2" hx-swap-oob="true"`, "1 / 2")
	assertNotContains(t, body, ">grafana<")

	if res, _ := get(t, srv, "/fragments/rows/nope"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown kind status = %d", res.StatusCode)
	}
}

func TestSummaryFragment(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/fragments/summary?ns=apps")
	// One object in "apps": the failed HelmRelease.
	assertContains(t, body, `All
      <span class="font-semibold tabular-nums">1</span>`, `Failing
      <span class="font-semibold tabular-nums">1</span>`)
	assertNotContains(t, body, "hx-swap-oob")
}

func TestObjectDrawer(t *testing.T) {
	srv, _ := newTestServer(t, false)

	res, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, "HelmRelease", "install retries exhausted", "OCIRepository/podinfo", `hx-trigger="sse:helmreleases, sse:events"`)

	res, body = get(t, srv, "/objects/helmreleases/apps/missing")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing object status = %d", res.StatusCode)
	}
	assertContains(t, body, "does not exist")

	if res, _ := get(t, srv, "/objects/ocirepositories/apps/podinfo"); res.StatusCode != http.StatusNotFound {
		t.Errorf("not installed kind status = %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/objects/nope/apps/podinfo"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown kind status = %d", res.StatusCode)
	}
}

func TestProbes(t *testing.T) {
	srv, _ := newTestServer(t, false)
	if res, _ := get(t, srv, "/healthz"); res.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/readyz"); res.StatusCode != http.StatusOK {
		t.Errorf("readyz = %d", res.StatusCode)
	}

	notReady, _ := newTestServer(t, true)
	if res, _ := get(t, notReady, "/readyz"); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz before sync = %d", res.StatusCode)
	}
}

func TestStatic(t *testing.T) {
	srv, _ := newTestServer(t, false)
	res, body := get(t, srv, "/static/js/app.js?v=test")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "htmx:sseOpen") {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if res, _ := get(t, srv, "/static/js/app.js"); res.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("unversioned Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
}

func TestEvents(t *testing.T) {
	srv, broker := newTestServer(t, false)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	// Response headers arrive after the handler subscribed, so this is delivered.
	broker.Notify("helmreleases")

	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if sc.Text() == "event: helmreleases" {
			if !sc.Scan() || sc.Text() != "data: helmreleases" {
				t.Fatalf("unexpected data line %q", sc.Text())
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	broker.Close()
	for sc.Scan() { // the stream ends once the broker closes
	}
}

func TestThemeCookie(t *testing.T) {
	srv, _ := newTestServer(t, false)
	tests := map[string]string{
		"":           `<html lang="en" class="h-full">`, // follows the OS
		"dark":       `<html lang="en" class="h-full" data-theme="dark">`,
		"light":      `<html lang="en" class="h-full" data-theme="light">`,
		`"><script>`: `<html lang="en" class="h-full">`,
	}
	for cookie, want := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "theme", Value: cookie})
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		if !strings.Contains(string(body), want) {
			t.Errorf("cookie %q: page does not contain %q", cookie, want)
		}
	}
	_, body := get(t, srv, "/")
	assertContains(t, body, `data-theme-toggle`)
}

func TestStaticCountsAsReady(t *testing.T) {
	srv, _ := newTestServer(t, false)

	// 5 objects: 2 ready + 1 static, 1 failed, 1 suspended. The chips add up.
	_, body := get(t, srv, "/fragments/summary")
	for label, count := range map[string]string{"All": "5", "Ready": "3", "Failing": "1", "Progressing": "0", "Suspended": "1"} {
		assertContains(t, body, label+"\n      <span class=\"font-semibold tabular-nums\">"+count+"</span>")
	}

	// The Ready filter lists it, still with its own Static pill.
	_, body = get(t, srv, "/?status=ready")
	assertContains(t, body, ">ghcr<", "</span>Static</span>", ">grafana<")
	assertNotContains(t, body, ">podinfo<")
}

func TestDrawerRefreshKeepsFrame(t *testing.T) {
	srv, _ := newTestServer(t, false)

	_, full := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertContains(t, full, `id="drawer-panel"`, `id="drawer-body"`, `?part=body`)

	// The live refresh returns only the body, with the header out of band:
	// the frame (and its scroll position) is never replaced.
	res, body := get(t, srv, "/objects/helmreleases/apps/podinfo?part=body")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, "install retries exhausted", `id="drawer-header"`, `hx-swap-oob="true"`)
	assertNotContains(t, body, `id="drawer-panel"`, `id="drawer-body"`)

	res, body = get(t, srv, "/objects/helmreleases/apps/gone?part=body")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing object: status = %d", res.StatusCode)
	}
	assertContains(t, body, "does not exist")
}

func TestDrawerEvents(t *testing.T) {
	ev := func(name, kind, object, typ, reason, msg string, ago time.Duration) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "apps", Name: name},
			InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: "apps", Name: object},
			Type:           typ, Reason: reason, Message: msg,
			LastTimestamp: metav1.NewTime(time.Now().Add(-ago)),
		}
	}
	srv, _, _ := newTestServerWith(t, false, Options{},
		ev("a", "HelmRelease", "podinfo", corev1.EventTypeWarning, "InstallFailed", "Helm install failed: timed out", time.Minute),
		ev("b", "OCIRepository", "podinfo", corev1.EventTypeNormal, "NewArtifact", "stored artifact 6.5.0", 2*time.Minute),
		ev("c", "HelmRelease", "other", corev1.EventTypeNormal, "Unrelated", "not about podinfo", time.Second),
	)

	_, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertContains(t, body,
		`hx-trigger="sse:helmreleases, sse:events"`,
		"InstallFailed", "Helm install failed: timed out", "bg-red-50", // warnings stand out
		"NewArtifact", "OCIRepository apps/podinfo", // the source's events, labeled
	)
	assertNotContains(t, body, "Unrelated")
	if strings.Index(body, "InstallFailed") > strings.Index(body, "NewArtifact") {
		t.Error("events are not sorted newest first")
	}

	_, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system")
	assertContains(t, body, "No recent events.")
}

func TestControllerLogs(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{Logs: true})

	_, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertContains(t, body, "Controller logs", "helm-controller", `hx-get="/objects/helmreleases/apps/podinfo/logs"`)

	res, body := get(t, srv, "/objects/helmreleases/apps/podinfo/logs")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, "10-01 20:00:02", "Reconciler error", "install retries exhausted", "font-semibold text-red-600", "2 lines, newest first")
	if strings.Index(body, "Reconciler error") > strings.Index(body, "running install action") {
		t.Error("lines are not newest first")
	}

	_, body = get(t, srv, "/objects/kustomizations/flux-system/apps/logs")
	assertContains(t, body, "pods is forbidden")
}

func TestControllerLogsDisabled(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertNotContains(t, body, "Controller logs")
	if res, _ := get(t, srv, "/objects/helmreleases/apps/podinfo/logs"); res.StatusCode != http.StatusNotFound {
		t.Errorf("logs status = %d, want 404", res.StatusCode)
	}
}

func TestDrawerInventory(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{},
		&kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "infra"},
			Status: kustomizev1.KustomizationStatus{Inventory: &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{
				{ID: "apps_podinfo_helm.toolkit.fluxcd.io_HelmRelease", Version: "v2"},
				{ID: "_apps__Namespace", Version: "v1"},
			}}},
		},
	)

	_, body := get(t, srv, "/objects/kustomizations/flux-system/infra")
	assertContains(t, body, "Managed objects", "HelmRelease", "helm.toolkit.fluxcd.io/v2", "Namespace",
		`hx-get="/objects/helmreleases/apps/podinfo"`, // Flux objects open their own drawer
		`class="group/inv" open`)                      // small inventories start expanded

	// The inventory is part of the drawer frame, not of the live refresh.
	_, body = get(t, srv, "/objects/kustomizations/flux-system/infra?part=body")
	assertNotContains(t, body, "Managed objects")

	_, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system")
	assertNotContains(t, body, "Managed objects")
}

func TestDrawerReleaseHistory(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{},
		&helmv2.HelmRelease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
			Status: helmv2.HelmReleaseStatus{History: helmv2.Snapshots{
				{Version: 2, ChartName: "web", ChartVersion: "1.1.0", Status: "failed", Action: "upgrade"},
				{Version: 1, ChartName: "web", ChartVersion: "1.0.0", Status: "deployed", Action: "install"},
			}},
		},
	)
	_, body := get(t, srv, "/objects/helmreleases/apps/web?part=body") // part of the live refresh
	assertContains(t, body, "Release history", "web@1.1.0", "web@1.0.0", "font-medium text-red-700", "install")
	if strings.Index(body, "web@1.1.0") > strings.Index(body, "web@1.0.0") {
		t.Error("revisions are not newest first")
	}

	_, body = get(t, srv, "/objects/kustomizations/flux-system/apps")
	assertNotContains(t, body, "Release history")
}

func TestDisplayVersion(t *testing.T) {
	for in, want := range map[string]string{"0.2.0": "v0.2.0", "1.10.3-rc.1": "v1.10.3-rc.1", "64146c5": "64146c5", "dev": "dev"} {
		if got := displayVersion(in); got != want {
			t.Errorf("displayVersion(%q) = %q, want %q", in, got, want)
		}
	}
	srv, _, _ := newTestServerWith(t, false, Options{Version: "0.2.0"})
	_, body := get(t, srv, "/")
	assertContains(t, body, `title="fluxcd-ui version">v0.2.0</span>`)
}

func TestDrawerInventoryHealth(t *testing.T) {
	infra := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "infra"},
		Status: kustomizev1.KustomizationStatus{Inventory: &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{
			{ID: "apps_podinfo_helm.toolkit.fluxcd.io_HelmRelease", Version: "v2"}, // failed, in the fixtures
			{ID: "_apps__Namespace", Version: "v1"},                                // not in the cluster
			{ID: "apps_cfg__ConfigMap", Version: "v1"},
		}}},
	}
	srv, _, _ := newTestServerWith(t, false, Options{ManagedStatus: true}, infra,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "cfg"}})

	// The drawer renders the list at once and loads health in the background.
	_, body := get(t, srv, "/objects/kustomizations/flux-system/infra")
	assertContains(t, body, `hx-get="/objects/kustomizations/flux-system/infra/inventory" hx-trigger="load"`, "checking status…")

	res, body := get(t, srv, "/objects/kustomizations/flux-system/infra/inventory")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body,
		"1 Failed", "1 Missing", "1 Ready", // summary, problems first
		"install retries exhausted", // the HelmRelease's own message
		"not found in the cluster",
		"need attention", "Refresh",
	)
	assertNotContains(t, body, `hx-trigger="load"`, "checking status…")
	if strings.Index(body, "1 Failed") > strings.Index(body, "1 Ready") {
		t.Error("summary does not list problems first")
	}
}

func TestDrawerInventoryHealthDisabled(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{},
		&kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "infra"},
			Status: kustomizev1.KustomizationStatus{Inventory: &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{
				{ID: "_apps__Namespace", Version: "v1"},
			}}},
		})
	_, body := get(t, srv, "/objects/kustomizations/flux-system/infra")
	assertContains(t, body, "Managed objects")
	assertNotContains(t, body, `/inventory"`, "checking status…")
	if res, _ := get(t, srv, "/objects/kustomizations/flux-system/infra/inventory"); res.StatusCode != http.StatusNotFound {
		t.Errorf("inventory status = %d, want 404", res.StatusCode)
	}
}

func TestKustomizationDiff(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{Diff: true})

	_, body := get(t, srv, "/objects/kustomizations/flux-system/apps")
	assertContains(t, body, "Diff with source", `hx-get="/objects/kustomizations/flux-system/apps/diff"`)
	_, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system")
	assertNotContains(t, body, "Diff with source") // Kustomizations only

	res, body := get(t, srv, "/objects/kustomizations/flux-system/apps/diff")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body,
		"main@abcdef1", "newer than the last applied", // the source is ahead of the cluster
		"1 Changed", "1 Created", "3 Unchanged",
		"spec.replicas", ">3</td>", ">2</td>",
		"Service",
	)
	assertNotContains(t, body, "untouched", "The cluster matches the source.") // unchanged objects are only counted
	if strings.Index(body, "1 Changed") > strings.Index(body, "3 Unchanged") {
		t.Error("summary is not in action order")
	}

	_, body = get(t, srv, "/objects/kustomizations/flux-system/other/diff")
	assertContains(t, body, "connection refused")
}

func TestKustomizationDiffDisabled(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/objects/kustomizations/flux-system/apps")
	assertNotContains(t, body, "Diff with source")
	if res, _ := get(t, srv, "/objects/kustomizations/flux-system/apps/diff"); res.StatusCode != http.StatusNotFound {
		t.Errorf("diff status = %d, want 404", res.StatusCode)
	}
}

func TestArtifactBrowser(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{ArtifactBrowser: true})

	_, body := get(t, srv, "/objects/gitrepositories/flux-system/flux-system")
	assertContains(t, body, "Source artifact", `hx-get="/objects/gitrepositories/flux-system/flux-system/artifact"`)
	_, body = get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertNotContains(t, body, "Source artifact") // not a source

	res, body := get(t, srv, "/objects/gitrepositories/flux-system/flux-system/artifact")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	assertContains(t, body, "3 files, 2.2 KiB", "main@cb89c25",
		`hx-get="/objects/gitrepositories/flux-system/flux-system/artifact/file?path=apps%2Fdeployment.yaml"`,
		`class="group/dir" open`) // small trees start expanded
	// Directories first: apps/ (and its base/) before README.md.
	if strings.Index(body, "apps/") > strings.Index(body, "README.md") || strings.Index(body, "base/") > strings.Index(body, "deployment.yaml") {
		t.Error("directories are not listed before files")
	}

	res, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system/artifact/file?path=apps/deployment.yaml")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("file status = %d", res.StatusCode)
	}
	assertContains(t, body, "kind: Deployment", "&lt;script&gt;alert(1)&lt;/script&gt;")
	assertNotContains(t, body, "<script>alert")

	_, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system/artifact/file?path=logo.png")
	assertContains(t, body, "Binary file, not shown.")
	res, body = get(t, srv, "/objects/gitrepositories/flux-system/flux-system/artifact/file?path=../../etc/passwd")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing file status = %d", res.StatusCode)
	}
	assertContains(t, body, "no such file in the artifact")
}

func TestArtifactBrowserDisabled(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/objects/gitrepositories/flux-system/flux-system")
	assertNotContains(t, body, "Source artifact")
	if res, _ := get(t, srv, "/objects/gitrepositories/flux-system/flux-system/artifact"); res.StatusCode != http.StatusNotFound {
		t.Errorf("artifact status = %d, want 404", res.StatusCode)
	}
}

func TestReleaseContent(t *testing.T) {
	srv, _, _ := newTestServerWith(t, false, Options{ReleaseContent: true})

	_, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertContains(t, body, "Values and manifest", `hx-get="/objects/helmreleases/apps/podinfo/release"`)
	assertNotContains(t, body, "not allowed to inspect")
	_, body = get(t, srv, "/objects/kustomizations/flux-system/apps")
	assertNotContains(t, body, "Values and manifest") // not a HelmRelease

	res, body := get(t, srv, "/objects/helmreleases/apps/podinfo/release")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	assertContains(t, body, "Revision 3 of", "podinfo@6.5.0", "apps/sh.helm.release.v1.podinfo.v3",
		"replicaCount: 2", "&lt;script&gt;alert(1)&lt;/script&gt;", "kind: Deployment", "21 B, without hooks")
	assertNotContains(t, body, "<script>alert", "first 4 MiB shown")

	_, body = get(t, srv, "/objects/helmreleases/monitoring/grafana/release")
	assertContains(t, body, "this HelmRelease has no Helm release yet")
}

func TestReleaseContentDisabled(t *testing.T) {
	srv, _ := newTestServer(t, false)
	_, body := get(t, srv, "/objects/helmreleases/apps/podinfo")
	assertNotContains(t, body, "Values and manifest")
	if res, _ := get(t, srv, "/objects/helmreleases/apps/podinfo/release"); res.StatusCode != http.StatusNotFound {
		t.Errorf("release status = %d, want 404", res.StatusCode)
	}
}

func TestTruncate(t *testing.T) {
	if got, cut := truncate("héllo", 10); got != "héllo" || cut {
		t.Errorf("short: %q, %t", got, cut)
	}
	// "é" is two bytes: cutting inside it backs off to the rune start.
	if got, cut := truncate("héllo", 2); got != "h" || !cut {
		t.Errorf("mid-rune: %q, %t", got, cut)
	}
}

func TestKustomizationRowsShowSourceState(t *testing.T) {
	ks := func(name, source string) *kustomizev1.Kustomization {
		return &kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name},
			Spec: kustomizev1.KustomizationSpec{SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Kind: "GitRepository", Namespace: "flux-system", Name: source,
			}},
		}
	}
	srv, _, _ := newTestServerWith(t, false, Options{}, ks("web", "flux-system"), ks("broken", "gone"))
	_, body := get(t, srv, "/fragments/rows/kustomizations")

	// A ready source: a green dot, its message on hover, no label.
	assertContains(t, body, `title="GitRepository flux-system/flux-system: Ready. all good"`, "bg-emerald-500")
	assertNotContains(t, body, "Source ready")
	// A missing one is called out.
	assertContains(t, body, `title="GitRepository flux-system/gone not found"`, "Source not found")
	// Without a source reference, the cell stays plain.
	assertContains(t, body, `title="">—</td>`)
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 2252: "2.2 KiB", 5714780: "5.5 MiB", 3 << 30: "3.0 GiB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestBuildTreeCollapsesLargeArtifacts(t *testing.T) {
	var files []artifact.File
	for i := range expandAbove + 1 {
		files = append(files, artifact.File{Path: fmt.Sprintf("apps/f%02d.yaml", i), Size: 1})
	}
	root, count, total := buildTree("/f", files)
	if count != expandAbove+1 || total != int64(expandAbove+1) || len(root.Children) != 1 {
		t.Fatalf("count = %d, total = %d, children = %d", count, total, len(root.Children))
	}
	// apps/ is the only top-level entry: it opens even in a large artifact.
	if apps := root.Children[0]; !apps.Open || apps.Files != expandAbove+1 {
		t.Errorf("apps/ = open %t, %d files", apps.Open, apps.Files)
	}

	files = append(files, artifact.File{Path: "README.md", Size: 1})
	if root, _, _ := buildTree("/f", files); root.Children[0].Open {
		t.Error("a directory with siblings in a large artifact starts expanded")
	}
}
