package flux

import (
	"testing"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func objectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 1}
}

func readyConditions(at time.Time) []metav1.Condition {
	return []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded",
		Message: "ok", LastTransitionTime: metav1.NewTime(at),
	}}
}

func kind(t *testing.T, id string) Kind {
	t.Helper()
	k, ok := KindByID(id)
	if !ok {
		t.Fatalf("unknown kind %q", id)
	}
	return k
}

func cellTexts(r Row) []string {
	out := make([]string, len(r.Cells))
	for i, c := range r.Cells {
		out[i] = c.Text
	}
	return out
}

func assertCells(t *testing.T, r Row, want ...string) {
	t.Helper()
	got := cellTexts(r)
	if len(got) != len(want) {
		t.Fatalf("cells = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cells = %q, want %q", got, want)
		}
	}
}

func TestKindsHaveOneCellPerColumn(t *testing.T) {
	for _, k := range Kinds {
		d := k.Describe(k.NewObject())
		if len(d.Cells) != len(k.Columns) {
			t.Errorf("%s: %d cells for %d columns", k.ID, len(d.Cells), len(k.Columns))
		}
		if d.Kind != k.ID {
			t.Errorf("%s: row kind = %q", k.ID, d.Kind)
		}
	}
}

func TestGitRepositoryRow(t *testing.T) {
	at := time.Date(2026, 9, 30, 19, 50, 37, 0, time.UTC)
	o := &sourcev1.GitRepository{
		ObjectMeta: objectMeta("flux-system", "flux-system"),
		Spec: sourcev1.GitRepositorySpec{
			URL:       "ssh://git@example.com/repo.git",
			Reference: &sourcev1.GitRepositoryRef{Branch: "main"},
			Interval:  metav1.Duration{Duration: time.Minute},
		},
		Status: sourcev1.GitRepositoryStatus{
			ObservedGeneration: 1,
			Conditions:         readyConditions(at),
			Artifact:           &meta.Artifact{Revision: "main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc"},
		},
	}
	d := kind(t, "gitrepositories").Describe(o)
	assertCells(t, d.Row, "ssh://git@example.com/repo.git", "main")
	if d.Revision != "main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc" {
		t.Errorf("revision = %q", d.Revision)
	}
	if d.Status.State != StateReady || !d.Updated.Equal(at) {
		t.Errorf("status = %+v, updated = %v", d.Status, d.Updated)
	}
}

func TestGitAndOCIRefs(t *testing.T) {
	gitTests := map[string]*sourcev1.GitRepositoryRef{
		"":               nil,
		"main":           {Branch: "main"},
		"tag v1.0.0":     {Branch: "main", Tag: "v1.0.0"},
		"semver >=1.0":   {Tag: "v1.0.0", SemVer: ">=1.0"},
		"refs/pull/1":    {SemVer: ">=1.0", Name: "refs/pull/1"},
		"commit cb89c25": {Name: "refs/pull/1", Commit: "cb89c25f8d709a17d3800304fad747848f3a42cc"},
	}
	for want, ref := range gitTests {
		if got := gitRef(ref); got != want {
			t.Errorf("gitRef(%+v) = %q, want %q", ref, got, want)
		}
	}

	ociTests := map[string]*sourcev1.OCIRepositoryRef{
		"latest":         nil,
		"6.5.0":          {Tag: "6.5.0"},
		"semver 6.x":     {Tag: "6.5.0", SemVer: "6.x"},
		"sha256:7109707": {SemVer: "6.x", Digest: "sha256:7109707984ef623368e15635d2b88c38d74a9e977e48d70bafa2dd101b4a2e12"},
	}
	for want, ref := range ociTests {
		if got := ociRef(ref); got != want {
			t.Errorf("ociRef(%+v) = %q, want %q", ref, got, want)
		}
	}
}

func TestHelmRepositoryOCIIsStatic(t *testing.T) {
	k := kind(t, "helmrepositories")

	oci := &sourcev1.HelmRepository{
		ObjectMeta: objectMeta("flux-system", "charts"),
		Spec:       sourcev1.HelmRepositorySpec{URL: "oci://ghcr.io/charts", Type: "oci"},
	}
	d := k.Describe(oci)
	assertCells(t, d.Row, "oci://ghcr.io/charts", "oci")
	if d.Status.State != StateStatic {
		t.Errorf("oci state = %s, want Static", d.Status.State)
	}

	oci.Spec.Suspend = true
	if got := k.Describe(oci).Status.State; got != StateSuspended {
		t.Errorf("suspended oci state = %s, want Suspended", got)
	}

	def := &sourcev1.HelmRepository{
		ObjectMeta: objectMeta("flux-system", "bitnami"),
		Spec:       sourcev1.HelmRepositorySpec{URL: "https://charts.bitnami.com/bitnami"},
	}
	d = k.Describe(def)
	assertCells(t, d.Row, "https://charts.bitnami.com/bitnami", "default")
	if d.Status.State != StateUnknown {
		t.Errorf("default state without conditions = %s, want Unknown", d.Status.State)
	}
}

func TestHelmChartRow(t *testing.T) {
	o := &sourcev1.HelmChart{
		ObjectMeta: objectMeta("flux-system", "alloy-alloy"),
		Spec: sourcev1.HelmChartSpec{
			Chart:     "alloy",
			SourceRef: sourcev1.LocalHelmChartSourceReference{Kind: "HelmRepository", Name: "grafana"},
		},
		Status: sourcev1.HelmChartStatus{Artifact: &meta.Artifact{Revision: "1.13.0"}},
	}
	d := kind(t, "helmcharts").Describe(o)
	assertCells(t, d.Row, "alloy", "*", "HelmRepository/grafana")
	if d.Revision != "1.13.0" {
		t.Errorf("revision = %q", d.Revision)
	}
}

func TestHelmReleaseRow(t *testing.T) {
	k := kind(t, "helmreleases")

	t.Run("chart template", func(t *testing.T) {
		o := &helmv2.HelmRelease{
			ObjectMeta: objectMeta("alloy", "alloy"),
			Spec: helmv2.HelmReleaseSpec{
				Chart: &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
					Chart: "alloy", Version: "1.x",
					SourceRef: helmv2.CrossNamespaceObjectReference{Kind: "HelmRepository", Name: "grafana", Namespace: "flux-system"},
				}},
			},
			Status: helmv2.HelmReleaseStatus{
				LastAttemptedRevision: "1.13.0",
				History: helmv2.Snapshots{
					{Version: 60, ChartName: "alloy", ChartVersion: "1.12.0", AppVersion: "v1.19.0"},
					{Version: 61, ChartName: "alloy", ChartVersion: "1.13.0", AppVersion: "v1.20.0"},
				},
			},
		}
		d := k.Describe(o)
		assertCells(t, d.Row, "alloy", "HelmRepository/flux-system/grafana", "v1.20.0")
		if d.Revision != "1.13.0" {
			t.Errorf("revision = %q, want deployed chart version", d.Revision)
		}
		if o.Status.History[0].Version != 60 {
			t.Error("describing reordered the release history")
		}
	})

	t.Run("chart ref, never deployed", func(t *testing.T) {
		o := &helmv2.HelmRelease{
			ObjectMeta: objectMeta("apps", "podinfo"),
			Spec: helmv2.HelmReleaseSpec{
				ChartRef: &helmv2.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "podinfo"},
			},
			Status: helmv2.HelmReleaseStatus{LastAttemptedRevision: "6.5.0"},
		}
		d := k.Describe(o)
		assertCells(t, d.Row, "podinfo", "OCIRepository/podinfo", "")
		if d.Revision != "6.5.0" {
			t.Errorf("revision = %q, want last attempted revision", d.Revision)
		}
	})
}

func TestKustomizationRow(t *testing.T) {
	o := &kustomizev1.Kustomization{
		ObjectMeta: objectMeta("flux-system", "apps"),
		Spec: kustomizev1.KustomizationSpec{
			SourceRef: kustomizev1.CrossNamespaceSourceReference{Kind: "GitRepository", Name: "flux-system"},
			Suspend:   true,
		},
		Status: kustomizev1.KustomizationStatus{LastAppliedRevision: "main@sha1:abc"},
	}
	d := kind(t, "kustomizations").Describe(o)
	assertCells(t, d.Row, "GitRepository/flux-system", "./")
	if d.Status.State != StateSuspended || d.Revision != "main@sha1:abc" {
		t.Errorf("status = %s, revision = %q", d.Status.State, d.Revision)
	}
	// The source cell names the source to resolve, in the Kustomization's
	// namespace unless the reference says otherwise.
	if src := d.Cells[0].Source; src == nil || *src != (SourceStatus{Kind: "GitRepository", Namespace: "flux-system", Name: "flux-system"}) {
		t.Errorf("source = %+v", src)
	}
	if d.Cells[1].Source != nil {
		t.Errorf("path cell has a source: %+v", d.Cells[1].Source)
	}
	o.Spec.SourceRef = kustomizev1.CrossNamespaceSourceReference{Kind: "OCIRepository", Name: "fleet", Namespace: "sources"}
	if src := kind(t, "kustomizations").Describe(o).Cells[0].Source; src == nil || src.Kind != "OCIRepository" || src.Namespace != "sources" {
		t.Errorf("cross-namespace source = %+v", src)
	}
}

func TestSourceStatus(t *testing.T) {
	tests := []struct {
		src     SourceStatus
		state   State
		ok      bool
		label   string
		summary string
	}{
		{SourceStatus{Found: true, Status: Status{State: StateReady, Message: "stored artifact"}}, StateReady, true, "Source ready", "GitRepository flux-system/fleet: Ready. stored artifact"},
		{SourceStatus{Found: true, Status: Status{State: StateFailed, Message: "auth failed"}}, StateFailed, false, "Source failed", "GitRepository flux-system/fleet: Failed. auth failed"},
		{SourceStatus{Found: true, Status: Status{State: StateSuspended}}, StateSuspended, false, "Source suspended", "GitRepository flux-system/fleet: Suspended"},
		{SourceStatus{}, StateFailed, false, "Source not found", "GitRepository flux-system/fleet not found"},
	}
	for _, tt := range tests {
		src := tt.src
		src.Kind, src.Namespace, src.Name = "GitRepository", "flux-system", "fleet"
		if src.State() != tt.state || src.OK() != tt.ok || src.Label() != tt.label || src.Summary() != tt.summary {
			t.Errorf("%+v: state %s, ok %t, label %q, summary %q", tt.src, src.State(), src.OK(), src.Label(), src.Summary())
		}
	}
}

func TestKindByGroupKind(t *testing.T) {
	if k, ok := KindByGroupKind("source.toolkit.fluxcd.io", "Bucket"); !ok || k.ID != "buckets" {
		t.Errorf("Bucket = %q, %t", k.ID, ok)
	}
	if _, ok := KindByGroupKind("source.toolkit.fluxcd.io", "ExternalArtifact"); ok {
		t.Error("ExternalArtifact is not shown, yet found")
	}
}

func TestRowsFromList(t *testing.T) {
	list := &sourcev1.GitRepositoryList{Items: []sourcev1.GitRepository{
		{ObjectMeta: objectMeta("a", "one")},
		{ObjectMeta: objectMeta("b", "two")},
	}}
	rows, err := kind(t, "gitrepositories").Rows(list)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "one" || rows[1].Namespace != "b" || rows[0].Kind != "gitrepositories" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestBucketRow(t *testing.T) {
	k := kind(t, "buckets")
	o := &sourcev1.Bucket{
		ObjectMeta: objectMeta("flux-system", "manifests"),
		Spec: sourcev1.BucketSpec{
			Endpoint:   "minio.example.com",
			BucketName: "fleet",
			Prefix:     "/clusters/home",
			Interval:   metav1.Duration{Duration: 5 * time.Minute},
		},
		Status: sourcev1.BucketStatus{
			ObservedGeneration: 1,
			Conditions:         readyConditions(time.Now()),
			Artifact:           &meta.Artifact{Revision: "sha256:7109707984ef623368e15635d2b88c38d74a9e977e48d70bafa2dd101b4a2e12"},
		},
	}
	d := k.Describe(o)
	assertCells(t, d.Row, "minio.example.com", "fleet/clusters/home", "generic")
	if d.Status.State != StateReady || ShortRevision(d.Revision) != "sha256:7109707" {
		t.Errorf("status = %s, revision = %q", d.Status.State, d.Revision)
	}

	o.Spec.Provider, o.Spec.Prefix = "aws", ""
	assertCells(t, k.Describe(o).Row, "minio.example.com", "fleet", "aws")
}

func TestHelmReleaseHistory(t *testing.T) {
	deployed := time.Date(2026, 9, 26, 11, 25, 49, 0, time.UTC)
	o := &helmv2.HelmRelease{
		ObjectMeta: objectMeta("alloy", "alloy"),
		Status: helmv2.HelmReleaseStatus{History: helmv2.Snapshots{
			{Version: 60, ChartName: "alloy", ChartVersion: "1.12.0", Status: "superseded", Action: "upgrade"},
			{Version: 62, ChartName: "alloy", ChartVersion: "1.13.1", Status: "failed", Action: "upgrade"},
			{Version: 61, ChartName: "alloy", ChartVersion: "1.13.0", AppVersion: "v1.20.0", Status: "deployed", Action: "upgrade", LastDeployed: metav1.NewTime(deployed)},
		}},
	}
	h := kind(t, "helmreleases").Describe(o).History
	if len(h) != 3 || h[0].Revision != 62 || h[1].Revision != 61 || h[2].Revision != 60 {
		t.Fatalf("history = %+v, want newest first", h)
	}
	if r := h[1]; r.Chart != "alloy@1.13.0" || r.AppVersion != "v1.20.0" || r.Status != "deployed" || r.Action != "upgrade" || !r.Deployed.Equal(deployed) {
		t.Errorf("revision 61 = %+v", r)
	}
	if o.Status.History[0].Version != 60 {
		t.Error("the cached history was reordered")
	}
	if h := kind(t, "kustomizations").Describe(kind(t, "kustomizations").NewObject()).History; h != nil {
		t.Errorf("a Kustomization has no release history: %+v", h)
	}
}
