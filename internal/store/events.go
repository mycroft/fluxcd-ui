package store

import (
	"cmp"
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// TopicEvents is published when a Kubernetes Event about a Flux object is
// recorded or updated.
const TopicEvents = "events"

// maxEvents caps the events returned for a set of objects.
const maxEvents = 25

// Event is a Kubernetes Event about a Flux object.
type Event struct {
	Object   ObjectRef // the object the event is about
	Type     string    // "Normal" or "Warning"
	Reason   string
	Message  string
	Count    int32
	LastSeen time.Time
}

const byObjectIndex = "object"

func objectKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

func indexByObject(obj any) ([]string, error) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return nil, nil
	}
	o := e.InvolvedObject
	return []string{objectKey(o.Kind, o.Namespace, o.Name)}, nil
}

// eventInformers watches the Events of Flux objects, one informer per Flux
// API version: the field selector keeps out every other event in the
// cluster, including those of same-named kinds from other groups.
type eventInformers struct {
	factories []informers.SharedInformerFactory
	indexers  []toolscache.Indexer
}

func newEventInformers(cs kubernetes.Interface, groupVersions []string, notify func()) (*eventInformers, error) {
	ei := &eventInformers{}
	for _, gv := range groupVersions {
		selector := fields.OneTermEqualSelector("involvedObject.apiVersion", gv).String()
		f := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = selector
		}))
		inf := f.Core().V1().Events().Informer()
		if err := inf.SetTransform(cache.TransformStripManagedFields()); err != nil {
			return nil, err
		}
		if err := inf.AddIndexers(toolscache.Indexers{byObjectIndex: indexByObject}); err != nil {
			return nil, err
		}
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerDetailedFuncs{
			AddFunc: func(_ any, isInInitialList bool) {
				if !isInInitialList {
					notify()
				}
			},
			UpdateFunc: func(_, _ any) { notify() },
		}); err != nil {
			return nil, err
		}
		ei.factories = append(ei.factories, f)
		ei.indexers = append(ei.indexers, inf.GetIndexer())
	}
	return ei, nil
}

func (ei *eventInformers) start(ctx context.Context) {
	for _, f := range ei.factories {
		f.Start(ctx.Done())
	}
}

func (ei *eventInformers) eventsFor(keys map[string]bool) []*corev1.Event {
	var out []*corev1.Event
	for _, idx := range ei.indexers {
		for key := range keys {
			objs, err := idx.ByIndex(byObjectIndex, key)
			if err != nil {
				continue
			}
			for _, o := range objs {
				if e, ok := o.(*corev1.Event); ok {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// Events returns the most recent events about the given objects, newest
// first.
func (s *Store) Events(ctx context.Context, refs ...ObjectRef) []Event {
	keys := map[string]bool{}
	for _, r := range refs {
		keys[objectKey(r.Kind, r.Namespace, r.Name)] = true
	}

	var raw []*corev1.Event
	if s.events != nil {
		raw = s.events.eventsFor(keys)
	} else { // built with New: read through the client
		list := &corev1.EventList{}
		if err := s.reader.List(ctx, list); err != nil {
			s.log.Warn("listing events", "err", err)
			return nil
		}
		for i := range list.Items {
			o := list.Items[i].InvolvedObject
			if keys[objectKey(o.Kind, o.Namespace, o.Name)] {
				raw = append(raw, &list.Items[i])
			}
		}
	}

	events := make([]Event, 0, len(raw))
	for _, e := range raw {
		o := e.InvolvedObject
		events = append(events, Event{
			Object:   ObjectRef{Kind: o.Kind, Namespace: o.Namespace, Name: o.Name},
			Type:     e.Type,
			Reason:   e.Reason,
			Message:  e.Message,
			Count:    max(e.Count, 1),
			LastSeen: lastSeen(e),
		})
	}
	slices.SortFunc(events, func(a, b Event) int { return b.LastSeen.Compare(a.LastSeen) })
	if len(events) > maxEvents {
		events = events[:maxEvents]
	}
	return events
}

// lastSeen returns when an event last occurred; which field holds it depends
// on the client that recorded the event.
func lastSeen(e *corev1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	return cmp.Or(e.LastTimestamp.Time, e.EventTime.Time, e.FirstTimestamp.Time, e.CreationTimestamp.Time)
}

// Related returns the object and the sources it reconciles from, deepest
// source first: the objects whose events explain the object's state.
func (s *Store) Related(ctx context.Context, k flux.Kind, namespace, name string) []ObjectRef {
	self := ObjectRef{Kind: k.GVK.Kind, Namespace: namespace, Name: name}
	obj := k.NewObject()
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return []ObjectRef{self}
	}
	refs, err := s.sources(ctx, obj, true)
	if err != nil {
		return []ObjectRef{self}
	}
	return append(refs, self)
}
