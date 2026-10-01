package store

import (
	"slices"
	"testing"
	"time"
)

func receive(t *testing.T, ch <-chan string, n int) []string {
	t.Helper()
	var got []string
	timeout := time.After(time.Second)
	for len(got) < n {
		select {
		case topic, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %q", got)
			}
			got = append(got, topic)
		case <-timeout:
			t.Fatalf("timed out after %q", got)
		}
	}
	slices.Sort(got)
	return got
}

func TestBrokerCoalescesNotifications(t *testing.T) {
	b := NewBroker(20 * time.Millisecond)
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	for range 5 {
		b.Notify("helmreleases", TopicChanged)
	}
	b.Notify("kustomizations", TopicChanged)

	got := receive(t, ch, 3)
	if want := []string{TopicChanged, "helmreleases", "kustomizations"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	select {
	case topic := <-ch:
		t.Fatalf("unexpected extra event %q", topic)
	case <-time.After(60 * time.Millisecond):
	}

	// Once published, a topic can be scheduled again.
	b.Notify("helmreleases")
	if got := receive(t, ch, 1); got[0] != "helmreleases" {
		t.Fatalf("got %q", got)
	}
}

func TestBrokerClose(t *testing.T) {
	b := NewBroker(time.Millisecond)
	ch, unsubscribe := b.Subscribe()
	b.Close()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel still open after Close")
	}
	unsubscribe() // must not panic on an already closed channel

	late, _ := b.Subscribe()
	if _, ok := <-late; ok {
		t.Fatal("subscribing to a closed broker returned an open channel")
	}
	b.Notify("helmreleases") // must not panic
}
