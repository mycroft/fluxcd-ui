package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const sampleLogs = `{"level":"info","ts":"2026-10-01T20:26:11.064Z","msg":"artifact up-to-date","controllerKind":"OCIRepository","namespace":"flux-system","name":"podinfo"}
starting manager (not JSON)
{"level":"info","ts":"2026-10-01T20:26:12.000Z","msg":"other object","controllerKind":"OCIRepository","namespace":"flux-system","name":"other"}
{"level":"info","ts":"2026-10-01T20:26:13.000Z","msg":"same name, other kind","controllerKind":"HelmChart","namespace":"flux-system","name":"podinfo"}
{"level":"error","ts":1790800000.5,"msg":"Reconciler error","error":"failed to pull artifact: unauthorized","controllerKind":"OCIRepository","namespace":"flux-system","name":"podinfo"}
`

func TestFilterLogs(t *testing.T) {
	lines, err := filterLogs(strings.NewReader(sampleLogs), "OCIRepository", "flux-system", "podinfo")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %+v", len(lines), lines)
	}
	if lines[0].Message != "artifact up-to-date" || !lines[0].Time.Equal(time.Date(2026, 10, 1, 20, 26, 11, 64e6, time.UTC)) {
		t.Errorf("first line = %+v", lines[0])
	}
	// Epoch-seconds timestamps, as zap writes by default, parse too.
	if l := lines[1]; l.Level != "error" || l.Error != "failed to pull artifact: unauthorized" || l.Time.Unix() != 1790800000 {
		t.Errorf("error line = %+v", l)
	}
}

func TestControllerLogs(t *testing.T) {
	ctx := context.Background()
	k := kind(t, "helmreleases")

	if _, err := (&Store{}).ControllerLogs(ctx, k, "apps", "podinfo"); !errors.Is(err, ErrLogsUnavailable) {
		t.Errorf("without a clientset: %v", err)
	}

	s := &Store{clientset: fake.NewClientset(), fluxNamespace: "flux-system"}
	if _, err := s.ControllerLogs(ctx, k, "apps", "podinfo"); err == nil || !strings.Contains(err.Error(), "no helm-controller pod found in namespace flux-system") {
		t.Errorf("without controller pods: %v", err)
	}

	// The fake clientset serves "fake logs" for every pod: not JSON, so no lines.
	s.clientset = fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "flux-system", Name: "helm-controller-abc", Labels: map[string]string{"app": "helm-controller"},
	}})
	lines, err := s.ControllerLogs(ctx, k, "apps", "podinfo")
	if err != nil || len(lines) != 0 {
		t.Errorf("lines = %+v, err = %v", lines, err)
	}
}
