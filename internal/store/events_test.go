package store

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func event(name, kind, namespace, object, reason string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: name},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: namespace, Name: object},
		Type:           corev1.EventTypeNormal,
		Reason:         reason,
		LastTimestamp:  metav1.NewTime(at),
	}
}

func testEvents() []*corev1.Event {
	warning := event("e2", "HelmRelease", "apps", "podinfo", "InstallFailed", t0.Add(2*time.Minute))
	warning.Type, warning.Count = corev1.EventTypeWarning, 3
	// Recorded with the newer events API: only eventTime is set.
	series := event("e3", "HelmChart", "flux-system", "apps-podinfo", "ChartPullSucceeded", time.Time{})
	series.EventTime = metav1.NewMicroTime(t0.Add(time.Minute))
	return []*corev1.Event{
		event("e1", "HelmRelease", "apps", "podinfo", "Progressing", t0),
		warning,
		series,
		event("e4", "HelmRelease", "apps", "other", "Unrelated", t0.Add(time.Hour)),
	}
}

func reasons(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Reason)
	}
	return out
}

var podinfoRefs = []ObjectRef{
	{Kind: "HelmChart", Namespace: "flux-system", Name: "apps-podinfo"},
	{Kind: "HelmRelease", Namespace: "apps", Name: "podinfo"},
}

func TestEventsThroughClient(t *testing.T) {
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, e := range testEvents() {
		b = b.WithObjects(e)
	}
	s := New(b.Build(), NewBroker(time.Millisecond))

	got := s.Events(context.Background(), podinfoRefs...)
	if want := []string{"InstallFailed", "ChartPullSucceeded", "Progressing"}; !slices.Equal(reasons(got), want) {
		t.Fatalf("events = %q, want %q (newest first, unrelated left out)", reasons(got), want)
	}
	if got[0].Count != 3 || got[0].Type != corev1.EventTypeWarning || got[2].Count != 1 {
		t.Errorf("counts and types = %+v", got)
	}
	if !got[1].LastSeen.Equal(t0.Add(time.Minute)) {
		t.Errorf("eventTime fallback: last seen = %v", got[1].LastSeen)
	}
}

func TestEventsThroughIndex(t *testing.T) {
	idx := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{byObjectIndex: indexByObject})
	for _, e := range testEvents() {
		if err := idx.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	s := &Store{events: &eventInformers{indexers: []toolscache.Indexer{idx}}}

	got := s.Events(context.Background(), podinfoRefs...)
	if want := []string{"InstallFailed", "ChartPullSucceeded", "Progressing"}; !slices.Equal(reasons(got), want) {
		t.Fatalf("events = %q, want %q", reasons(got), want)
	}
	if got := s.Events(context.Background(), ObjectRef{Kind: "HelmRelease", Namespace: "apps", Name: "nope"}); len(got) != 0 {
		t.Errorf("events for an object without any: %+v", got)
	}
}

func TestEventsAreCapped(t *testing.T) {
	idx := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{byObjectIndex: indexByObject})
	for i := range maxEvents + 10 {
		if err := idx.Add(event("e"+string(rune('a'+i)), "Kustomization", "flux-system", "apps", "R", t0.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	s := &Store{events: &eventInformers{indexers: []toolscache.Indexer{idx}}}
	got := s.Events(context.Background(), ObjectRef{Kind: "Kustomization", Namespace: "flux-system", Name: "apps"})
	if len(got) != maxEvents || !got[0].LastSeen.After(got[len(got)-1].LastSeen) {
		t.Fatalf("%d events, newest %v, oldest %v", len(got), got[0].LastSeen, got[len(got)-1].LastSeen)
	}
}

func TestRelated(t *testing.T) {
	s, _ := newActionStore(t)
	ctx := context.Background()

	var got []string
	for _, r := range s.Related(ctx, kind(t, "helmreleases"), "apps", "podinfo") {
		got = append(got, r.String())
	}
	if want := []string{"HelmRepository flux-system/podinfo", "HelmChart flux-system/apps-podinfo", "HelmRelease apps/podinfo"}; !slices.Equal(got, want) {
		t.Fatalf("related = %q, want %q", got, want)
	}
	if got := s.Related(ctx, kind(t, "kustomizations"), "flux-system", "missing"); len(got) != 1 || got[0].Name != "missing" {
		t.Fatalf("related of a missing object = %+v", got)
	}
}
