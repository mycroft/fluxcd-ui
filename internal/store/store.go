// Package store serves Flux objects from an informer-backed cache and
// publishes change notifications for live updates.
package store

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// TopicChanged is published, in addition to the kind's ID, whenever any
// object changes.
const TopicChanged = "changed"

// KindState reports whether objects of a kind can be listed.
type KindState struct {
	Installed bool  // the CRD version is served by the cluster
	Synced    bool  // the informer has completed its initial list
	Err       error // last watch error while not synced
}

// Store reads Flux objects from a client.Reader, normally an informer cache,
// and writes them (actions) through a direct client.
type Store struct {
	reader client.Reader
	writer client.Client
	cache  cache.Cache     // nil when built with New
	events *eventInformers // nil when built with New
	// clientset reads controller pod logs in fluxNamespace; nil when built
	// with New.
	clientset     kubernetes.Interface
	fluxNamespace string
	health        healthCache // computed health of managed objects
	broker        *Broker
	log           *slog.Logger

	mu     sync.RWMutex
	states map[string]*KindState
}

// New returns a Store reading from and writing to c, with the given kinds
// installed and already synced. It is meant for tests.
func New(c client.Client, broker *Broker, installed ...string) *Store {
	s := &Store{reader: c, writer: c, broker: broker, log: slog.Default(), states: map[string]*KindState{}}
	for _, k := range flux.Kinds {
		s.states[k.ID] = &KindState{}
	}
	for _, id := range installed {
		s.states[id] = &KindState{Installed: true, Synced: true}
	}
	return s
}

// NewForCluster discovers which Flux kinds the cluster serves and prepares an
// informer cache for them. Call Start to run it. fluxNamespace is where the
// Flux controllers run, for their logs.
func NewForCluster(cfg *rest.Config, broker *Broker, fluxNamespace string, log *slog.Logger) (*Store, error) {
	scheme, err := flux.NewScheme()
	if err != nil {
		return nil, err
	}
	installed, err := discoverInstalled(cfg)
	if err != nil {
		return nil, err
	}

	s := &Store{broker: broker, log: log, fluxNamespace: fluxNamespace, states: map[string]*KindState{}}
	for _, k := range flux.Kinds {
		s.states[k.ID] = &KindState{Installed: installed[k.ID]}
		if !installed[k.ID] {
			log.Warn("kind not served by the cluster, skipping", "kind", k.GVK.String())
		}
	}

	c, err := cache.New(cfg, cache.Options{
		Scheme:                   scheme,
		DefaultTransform:         cache.TransformStripManagedFields(),
		DefaultWatchErrorHandler: s.onWatchError,
	})
	if err != nil {
		return nil, fmt.Errorf("creating cache: %w", err)
	}
	s.cache, s.reader = c, c

	// Writes bypass the cache, so that they are never based on stale objects.
	if s.writer, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	s.clientset = cs
	var groupVersions []string
	for _, k := range flux.Kinds {
		if gv := k.GVK.GroupVersion().String(); installed[k.ID] && !slices.Contains(groupVersions, gv) {
			groupVersions = append(groupVersions, gv)
		}
	}
	if s.events, err = newEventInformers(cs, groupVersions, func() { broker.Notify(TopicEvents) }); err != nil {
		return nil, fmt.Errorf("creating event informers: %w", err)
	}
	return s, nil
}

// Start registers informers for the installed kinds and runs the cache until
// ctx is cancelled.
func (s *Store) Start(ctx context.Context) error {
	for _, k := range flux.Kinds {
		if !s.State(k.ID).Installed {
			continue
		}
		inf, err := s.cache.GetInformer(ctx, k.NewObject(), cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("getting informer for %s: %w", k.ID, err)
		}
		if _, err := inf.AddEventHandler(s.eventHandler(k.ID)); err != nil {
			return fmt.Errorf("adding event handler for %s: %w", k.ID, err)
		}
		go s.waitForSync(ctx, k.ID, inf)
	}
	s.events.start(ctx)
	return s.cache.Start(ctx)
}

// State returns the state of the kind with the given ID.
func (s *Store) State(id string) KindState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.states[id]; ok {
		return *st
	}
	return KindState{}
}

// Ready reports whether every installed kind has either synced or reported a
// watch error, so that the UI can show either its objects or what went wrong.
func (s *Store) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.states {
		if st.Installed && !st.Synced && st.Err == nil {
			return false
		}
	}
	return true
}

// List returns the rows of all objects of kind k, sorted by namespace and name.
func (s *Store) List(ctx context.Context, k flux.Kind) ([]flux.Row, error) {
	list := k.NewList()
	// Objects are only read, never mutated, so skip the cache's deep copy.
	if err := s.reader.List(ctx, list, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	rows, err := k.Rows(list)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b flux.Row) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return rows, nil
}

// Get returns the detail of one object. A missing object yields an error
// satisfying apierrors.IsNotFound.
func (s *Store) Get(ctx context.Context, k flux.Kind, namespace, name string) (flux.Detail, error) {
	obj := k.NewObject()
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return flux.Detail{}, err
	}
	return k.Describe(obj), nil
}

func (s *Store) eventHandler(id string) toolscache.ResourceEventHandler {
	notify := func() { s.broker.Notify(id, TopicChanged) }
	return toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(_ any, isInInitialList bool) {
			if !isInInitialList { // the initial list is announced once synced
				notify()
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(client.Object)
			n, ok2 := newObj.(client.Object)
			if ok1 && ok2 && o.GetResourceVersion() == n.GetResourceVersion() {
				return // periodic resync, nothing changed
			}
			notify()
		},
		DeleteFunc: func(any) { notify() },
	}
}

func (s *Store) waitForSync(ctx context.Context, id string, inf cache.Informer) {
	if !toolscache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return
	}
	s.mu.Lock()
	s.states[id].Synced = true
	s.states[id].Err = nil
	s.mu.Unlock()
	s.log.Info("cache synced", "kind", id)
	s.broker.Notify(id, TopicChanged)
}

// onWatchError records list/watch failures (typically missing RBAC) against
// the kind they belong to, so the UI can show them instead of syncing forever.
func (s *Store) onWatchError(ctx context.Context, r *toolscache.Reflector, err error) {
	toolscache.DefaultWatchErrorHandler(ctx, r, err)

	// Typed informers describe their type as e.g. "*v1.GitRepository".
	desc := r.TypeDescription()
	for _, k := range flux.Kinds {
		if !strings.HasSuffix(desc, "."+k.GVK.Kind) {
			continue
		}
		s.mu.Lock()
		st := s.states[k.ID]
		notify := !st.Synced && st.Err == nil
		if !st.Synced {
			st.Err = err
		}
		s.mu.Unlock()
		if notify {
			s.broker.Notify(k.ID, TopicChanged)
		}
		return
	}
}

// discoverInstalled reports, per kind ID, whether the cluster serves its
// group/version/kind.
func discoverInstalled(cfg *rest.Config) (map[string]bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}
	kindsByGV := map[schema.GroupVersion]map[string]bool{}
	installed := map[string]bool{}
	for _, k := range flux.Kinds {
		gv := k.GVK.GroupVersion()
		kinds, ok := kindsByGV[gv]
		if !ok {
			kinds = map[string]bool{}
			res, err := dc.ServerResourcesForGroupVersion(gv.String())
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return nil, fmt.Errorf("discovering %s: %w", gv, err)
			default:
				for _, r := range res.APIResources {
					kinds[r.Kind] = true
				}
			}
			kindsByGV[gv] = kinds
		}
		installed[k.ID] = kinds[k.GVK.Kind]
	}
	return installed, nil
}
