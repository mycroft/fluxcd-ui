package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// releaseSecret encodes rel like Helm's Secret driver: JSON, gzipped, base64.
func releaseSecret(t *testing.T, namespace, name string, rel map[string]any) *corev1.Secret {
	t.Helper()
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       helmReleaseType,
		Data:       map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

func newReleaseStore(t *testing.T, objects []client.Object, secrets ...runtime.Object) *Store {
	t.Helper()
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &Store{reader: c, clientset: k8sfake.NewClientset(secrets...)}
}

func TestHelmReleaseContent(t *testing.T) {
	ctx := context.Background()
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "podinfo"},
		// Edited after the release was stored, to read another team's
		// release of the same name: helm-controller's record wins.
		Spec: helmv2.HelmReleaseSpec{TargetNamespace: "web", StorageNamespace: "team-b", Suspend: true},
		Status: helmv2.HelmReleaseStatus{StorageNamespace: "helm-storage", History: helmv2.Snapshots{
			{Name: "web-podinfo", Namespace: "web", Version: 2},
			{Name: "web-podinfo", Namespace: "web", Version: 3},
		}},
	}
	rel := map[string]any{
		"name":      "web-podinfo",
		"namespace": "web",
		"version":   3,
		"info":      map[string]any{"status": "deployed"},
		"chart":     map[string]any{"metadata": map[string]any{"name": "podinfo", "version": "6.5.0"}},
		// Above 2^53, where float64 would round it.
		"config":   map[string]any{"replicaCount": 2, "ingress": map[string]any{"enabled": true}, "channelID": int64(1234567890123456789)},
		"manifest": "---\nkind: Deployment\n",
	}
	teamB := map[string]any{"name": "web-podinfo", "namespace": "web", "version": 3, "config": map[string]any{"password": "team-b"}}
	// The latest revision, in the storage namespace, under the release name.
	s := newReleaseStore(t, []client.Object{hr},
		releaseSecret(t, "helm-storage", "sh.helm.release.v1.web-podinfo.v3", rel),
		releaseSecret(t, "helm-storage", "sh.helm.release.v1.web-podinfo.v2", map[string]any{"version": 2}),
		releaseSecret(t, "team-b", "sh.helm.release.v1.web-podinfo.v3", teamB),
	)
	c, err := s.HelmReleaseContent(ctx, "apps", "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web-podinfo" || c.Namespace != "helm-storage" || c.Revision != 3 || c.Chart != "podinfo@6.5.0" || c.Status != "deployed" {
		t.Errorf("content = %+v", c)
	}
	if want := "channelID: 1234567890123456789\ningress:\n  enabled: true\nreplicaCount: 2\n"; c.Values != want {
		t.Errorf("values = %q, want %q", c.Values, want)
	}
	if c.Manifest != "---\nkind: Deployment\n" {
		t.Errorf("manifest = %q", c.Manifest)
	}

	// Without helm-controller's record, the spec says where the release is.
	hr.Name, hr.Status.StorageNamespace, hr.Spec.StorageNamespace = "legacy", "", "helm-storage"
	s = newReleaseStore(t, []client.Object{hr}, releaseSecret(t, "helm-storage", "sh.helm.release.v1.web-podinfo.v3", rel))
	if c, err := s.HelmReleaseContent(ctx, "apps", "legacy"); err != nil || c.Namespace != "helm-storage" {
		t.Errorf("from the spec: %+v, %v", c, err)
	}
}

func TestHelmReleaseContentWithoutValues(t *testing.T) {
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "podinfo"},
		Status:     helmv2.HelmReleaseStatus{History: helmv2.Snapshots{{Name: "podinfo", Namespace: "apps", Version: 1}}},
	}
	for _, config := range []any{nil, map[string]any{}} {
		rel := map[string]any{"name": "podinfo", "namespace": "apps", "version": 1, "config": config}
		s := newReleaseStore(t, []client.Object{hr}, releaseSecret(t, "apps", "sh.helm.release.v1.podinfo.v1", rel))
		if c, err := s.HelmReleaseContent(context.Background(), "apps", "podinfo"); err != nil || c.Values != "" {
			t.Errorf("config %v: values = %q, %v", config, c.Values, err)
		}
	}
}

func TestHelmReleaseContentErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Store{}).HelmReleaseContent(ctx, "apps", "podinfo"); !errors.Is(err, ErrReleaseContentUnavailable) {
		t.Errorf("without a clientset: %v", err)
	}

	hr := func(name string, history ...*helmv2.Snapshot) *helmv2.HelmRelease {
		return &helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name}, Status: helmv2.HelmReleaseStatus{History: history}}
	}
	notHelm := releaseSecret(t, "apps", "sh.helm.release.v1.forged.v1", map[string]any{"name": "forged", "namespace": "apps", "version": 1})
	notHelm.Type = corev1.SecretTypeOpaque
	// A Helm release, but not the one its name says.
	renamed := releaseSecret(t, "apps", "sh.helm.release.v1.renamed.v1", map[string]any{"name": "other", "namespace": "apps", "version": 1})
	s := newReleaseStore(t,
		[]client.Object{
			hr("new"),
			hr("gone", &helmv2.Snapshot{Name: "gone", Version: 1}),
			hr("forged", &helmv2.Snapshot{Name: "forged", Namespace: "apps", Version: 1}),
			hr("renamed", &helmv2.Snapshot{Name: "renamed", Namespace: "apps", Version: 1}),
		},
		notHelm, renamed,
	)

	if _, err := s.HelmReleaseContent(ctx, "apps", "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("missing HelmRelease: %v", err)
	}
	if _, err := s.HelmReleaseContent(ctx, "apps", "new"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("no history: %v", err)
	}
	if _, err := s.HelmReleaseContent(ctx, "apps", "gone"); !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "apps/sh.helm.release.v1.gone.v1") {
		t.Errorf("missing Secret: %v", err)
	}
	if _, err := s.HelmReleaseContent(ctx, "apps", "forged"); err == nil || !strings.Contains(err.Error(), `of type "Opaque", not a Helm release`) {
		t.Errorf("non-Helm Secret: %v", err)
	}
	if _, err := s.HelmReleaseContent(ctx, "apps", "renamed"); err == nil || !strings.Contains(err.Error(), "holds release apps/other v1, not apps/renamed v1") {
		t.Errorf("another release: %v", err)
	}
}

func TestDecodeRelease(t *testing.T) {
	// Helm also reads releases stored without compression.
	plain := base64.StdEncoding.EncodeToString([]byte(`{"version":1,"manifest":"kind: Service"}`))
	rel, err := decodeRelease([]byte(plain))
	if err != nil || rel.Version != 1 || rel.Manifest != "kind: Service" {
		t.Errorf("uncompressed: %+v, %v", rel, err)
	}
	if _, err := decodeRelease([]byte("not base64!")); err == nil {
		t.Error("invalid base64 decoded")
	}
}
