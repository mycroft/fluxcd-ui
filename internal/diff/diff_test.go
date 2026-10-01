package diff

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/artifact"
	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// The source: no kustomization.yaml, as in many Flux repositories.
var sourceFiles = map[string]string{
	"apps/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
spec:
  replicas: 2
  selector: {matchLabels: {app: podinfo}}
  template:
    metadata: {labels: {app: podinfo}}
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/stefanprodan/podinfo:6.7.1
`,
	"apps/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: podinfo
spec:
  ports: [{port: 9898}]
`,
	"apps/configmap.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: greeting
data:
  greeting: ${GREETING}
`,
	"apps/secret.yaml": `apiVersion: v1
kind: Secret
metadata:
  name: token
stringData: {token: s3cr3t}
`,
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newDiffer(t *testing.T, tarGz []byte, digestOverride string) *Differ {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tarGz) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(tarGz)
	digest := cmpOr(digestOverride, "sha256:"+hex.EncodeToString(sum[:]))

	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{
		&sourcev1.GitRepository{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "fleet"},
			Status: sourcev1.GitRepositoryStatus{Artifact: &meta.Artifact{
				URL: srv.URL + "/gitrepository/flux-system/fleet/abc.tar.gz", Path: "gitrepository/flux-system/fleet/abc.tar.gz", Digest: digest, Revision: "main@sha1:abc",
			}},
		},
		&kustomizev1.Kustomization{
			ObjectMeta: metav1.ObjectMeta{Namespace: "flux-system", Name: "apps"},
			Spec: kustomizev1.KustomizationSpec{
				SourceRef:       kustomizev1.CrossNamespaceSourceReference{Kind: "GitRepository", Name: "fleet"},
				Path:            "./apps",
				TargetNamespace: "apps",
				Prune:           true,
				PostBuild:       &kustomizev1.PostBuild{Substitute: map[string]string{"GREETING": "hello"}},
			},
			Status: kustomizev1.KustomizationStatus{
				LastAppliedRevision: "main@sha1:old",
				Inventory: &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{
					{ID: "apps_podinfo_apps_Deployment", Version: "v1"},
					{ID: "apps_greeting__ConfigMap", Version: "v1"},
					{ID: "apps_old-config__ConfigMap", Version: "v1"},
				}},
			},
		},
		// Live: drifted replicas and image, an unchanged ConfigMap, and an
		// old ConfigMap the source no longer has. No Service yet.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "podinfo"},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(3)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "podinfo"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "podinfo"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "podinfo", Image: "ghcr.io/stefanprodan/podinfo:6.7.0"}}},
				},
			},
		},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "greeting"}, Data: map[string]string{"greeting": "hello"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "old-config"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return New(c, c, artifact.NewFetcher(srv.Client(), nil), t.TempDir())
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func find(t *testing.T, r *Result, kind, name string) Object {
	t.Helper()
	for _, o := range r.Objects {
		if o.Kind == kind && o.Name == name {
			return o
		}
	}
	t.Fatalf("no %s %s in %+v", kind, name, r.Objects)
	return Object{}
}

func TestKustomizationDiff(t *testing.T) {
	d := newDiffer(t, tarball(t, sourceFiles), "")
	r, err := d.Kustomization(context.Background(), "flux-system", "apps")
	if err != nil {
		t.Fatal(err)
	}
	if r.Revision != "main@sha1:abc" || r.AppliedRevision != "main@sha1:old" {
		t.Errorf("revisions = %q, %q", r.Revision, r.AppliedRevision)
	}

	dep := find(t, r, "Deployment", "podinfo")
	if dep.Action != ActionChanged || dep.Namespace != "apps" || len(dep.Changes) != 2 {
		t.Fatalf("deployment = %+v", dep)
	}
	var got []string
	for _, c := range dep.Changes {
		got = append(got, c.Path+": "+c.Live+" → "+c.Desired)
	}
	want := "spec.replicas: 3 → 2|spec.template.spec.containers[name=podinfo].image: ghcr.io/stefanprodan/podinfo:6.7.0 → ghcr.io/stefanprodan/podinfo:6.7.1"
	if strings.Join(got, "|") != want {
		t.Errorf("changes = %q", got)
	}

	// The substituted ConfigMap matches; the rest as described above.
	for kind, wantAction := range map[[2]string]Action{
		{"ConfigMap", "greeting"}:   ActionUnchanged,
		{"Service", "podinfo"}:      ActionCreated,
		{"Secret", "token"}:         ActionSkipped,
		{"ConfigMap", "old-config"}: ActionPruned,
	} {
		if o := find(t, r, kind[0], kind[1]); o.Action != wantAction {
			t.Errorf("%s %s: %s (%s), want %s", kind[0], kind[1], o.Action, o.Note, wantAction)
		}
	}
	if r.Objects[0].Action != ActionChanged || r.Objects[len(r.Objects)-1].Action != ActionUnchanged {
		t.Errorf("results are not sorted by action: %+v", r.Objects)
	}
	if r.Counts[ActionChanged] != 1 || r.Counts[ActionCreated] != 1 || r.Counts[ActionPruned] != 1 {
		t.Errorf("counts = %v", r.Counts)
	}
}

func TestKustomizationDiffPruneDisabled(t *testing.T) {
	d := newDiffer(t, tarball(t, sourceFiles), "")
	ks := &kustomizev1.Kustomization{}
	ctx := context.Background()
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: "flux-system", Name: "apps"}, ks); err != nil {
		t.Fatal(err)
	}
	ks.Spec.Prune = false
	if err := d.client.Update(ctx, ks); err != nil {
		t.Fatal(err)
	}
	r, err := d.Kustomization(ctx, "flux-system", "apps")
	if err != nil {
		t.Fatal(err)
	}
	if o := find(t, r, "ConfigMap", "old-config"); o.Action != ActionOrphaned {
		t.Errorf("old-config = %+v", o)
	}
}

func TestKustomizationDiffRejectsTamperedArtifact(t *testing.T) {
	d := newDiffer(t, tarball(t, sourceFiles), "sha256:"+strings.Repeat("0", 64))
	_, err := d.Kustomization(context.Background(), "flux-system", "apps")
	if err == nil || !strings.Contains(err.Error(), "does not match its digest") {
		t.Fatalf("err = %v", err)
	}
}

func TestKustomizationDiffMissingPath(t *testing.T) {
	d := newDiffer(t, tarball(t, map[string]string{"other/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n"}), "")
	_, err := d.Kustomization(context.Background(), "flux-system", "apps")
	if err == nil || !strings.Contains(err.Error(), `path "./apps" not found`) {
		t.Fatalf("err = %v", err)
	}
}
