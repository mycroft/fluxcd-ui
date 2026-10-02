// Package store serves Flux objects from an informer-backed cache and
// publishes change notifications for live updates.
package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/artifact"
	"github.com/mycroft/fluxcd-ui/internal/diff"
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
	differ        *diff.Differ
	artifacts     *artifact.Fetcher
	artifactCache *artifact.Cache // extracted artifacts, for browsing
	broker        *Broker
	log           *slog.Logger

	mu     sync.RWMutex
	states map[string]*KindState
}

// New returns a Store reading from and writing to c, with the given kinds
// installed and already synced. It is meant for tests.
func New(c client.Client, broker *Broker, installed ...string) *Store {
	s := &Store{reader: c, writer: c, broker: broker, log: slog.Default(), states: map[string]*KindState{}}
	s.artifacts = artifact.NewFetcher(http.DefaultClient, nil)
	s.differ = diff.New(c, c, s.artifacts, "")
	s.artifactCache = artifact.NewCache(s.artifacts, "")
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

	// In the cluster, artifacts are fetched from source-controller like
	// kustomize-controller does; outside of it, through the API server.
	hc, rewrite := &http.Client{Timeout: time.Minute}, (func(string) (string, error))(nil)
	if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		if hc, err = rest.HTTPClientFor(cfg); err != nil {
			return nil, fmt.Errorf("creating HTTP client: %w", err)
		}
		rewrite = func(u string) (string, error) { return artifact.ProxyURL(cfg.Host, u) }
	}
	s.artifacts = artifact.NewFetcher(hc, rewrite)
	s.differ = diff.New(s.reader, s.writer, s.artifacts, "")
	s.artifactCache = artifact.NewCache(s.artifacts, "")
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
		if _, err := inf.AddEventHandler(s.eventHandler(k)); err != nil {
			return fmt.Errorf("adding event handler for %s: %w", k.ID, err)
		}
		go s.waitForSync(ctx, k, inf)
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

// List returns the rows of all objects of kind k, sorted by namespace and name,
// with the state of the sources they name.
func (s *Store) List(ctx context.Context, k flux.Kind) ([]flux.Row, error) {
	rows, err := s.rows(ctx, k)
	if err != nil {
		return nil, err
	}
	s.resolveSources(ctx, rows)
	slices.SortFunc(rows, func(a, b flux.Row) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return rows, nil
}

func (s *Store) rows(ctx context.Context, k flux.Kind) ([]flux.Row, error) {
	list := k.NewList()
	// Objects are only read, never mutated, so skip the cache's deep copy.
	if err := s.reader.List(ctx, list, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	return k.Rows(list)
}

// resolveSources fills in the state of the sources that rows name. Sources of
// a kind that is not watched, or not synced yet, are left unchecked.
func (s *Store) resolveSources(ctx context.Context, rows []flux.Row) {
	byKind := map[string]map[client.ObjectKey]flux.Status{}
	for _, row := range rows {
		for _, cell := range row.Cells {
			src := cell.Source
			if src == nil {
				continue
			}
			statuses, listed := byKind[src.Kind]
			if !listed {
				statuses = s.sourceStatuses(ctx, src.Kind)
				byKind[src.Kind] = statuses
			}
			if statuses == nil {
				continue
			}
			src.Checked = true
			src.Status, src.Found = statuses[client.ObjectKey{Namespace: src.Namespace, Name: src.Name}]
		}
	}
}

// sourceStatuses returns the status of every object of a source kind, or nil
// when that kind is not watched or not synced yet.
func (s *Store) sourceStatuses(ctx context.Context, kind string) map[client.ObjectKey]flux.Status {
	k, ok := flux.KindByGroupKind(sourcev1.GroupVersion.Group, kind)
	if !ok || !s.State(k.ID).Synced {
		return nil
	}
	rows, err := s.rows(ctx, k)
	if err != nil {
		s.log.Debug("listing sources", "kind", k.ID, "err", err)
		return nil
	}
	statuses := make(map[client.ObjectKey]flux.Status, len(rows))
	for _, r := range rows {
		statuses[client.ObjectKey{Namespace: r.Namespace, Name: r.Name}] = r.Status
	}
	return statuses
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

func (s *Store) eventHandler(k flux.Kind) toolscache.ResourceEventHandler {
	notify := func(stateChanged bool) {
		s.broker.Notify(k.ID, TopicChanged)
		if stateChanged { // rows naming the object as their source show its state
			s.broker.Notify(k.UsedBy...)
		}
	}
	return toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(_ any, isInInitialList bool) {
			if !isInInitialList { // the initial list is announced once synced
				notify(true)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(client.Object)
			n, ok2 := newObj.(client.Object)
			if !ok1 || !ok2 {
				notify(true)
				return
			}
			if o.GetResourceVersion() == n.GetResourceVersion() {
				return // periodic resync, nothing changed
			}
			notify(len(k.UsedBy) > 0 && k.Describe(o).Status != k.Describe(n).Status)
		},
		DeleteFunc: func(any) { notify(true) },
	}
}

func (s *Store) waitForSync(ctx context.Context, k flux.Kind, inf cache.Informer) {
	if !toolscache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return
	}
	s.mu.Lock()
	s.states[k.ID].Synced = true
	s.states[k.ID].Err = nil
	s.mu.Unlock()
	s.log.Info("cache synced", "kind", k.ID)
	s.broker.Notify(k.ID, TopicChanged)
	s.broker.Notify(k.UsedBy...) // their rows can now show these sources' state
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

// DiffKustomization compares what a Kustomization's source would apply with
// the cluster.
func (s *Store) DiffKustomization(ctx context.Context, namespace, name string) (*diff.Result, error) {
	return s.differ.Kustomization(ctx, namespace, name)
}

// SourceArtifact returns the artifact a source object holds.
func (s *Store) SourceArtifact(ctx context.Context, k flux.Kind, namespace, name string) (*meta.Artifact, error) {
	obj := k.NewObject()
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return nil, err
	}
	a := artifact.Of(obj)
	if a == nil {
		return nil, errors.New(k.GVK.Kind + " " + namespace + "/" + name + " has no artifact")
	}
	return a, nil
}

// ArtifactFiles lists the files of a source's artifact.
func (s *Store) ArtifactFiles(ctx context.Context, k flux.Kind, namespace, name string) (*meta.Artifact, []artifact.File, error) {
	a, err := s.SourceArtifact(ctx, k, namespace, name)
	if err != nil {
		return nil, nil, err
	}
	files, err := s.artifactCache.Files(ctx, a)
	return a, files, err
}

// ArtifactFile returns one file of a source's artifact.
func (s *Store) ArtifactFile(ctx context.Context, k flux.Kind, namespace, name, path string) (*artifact.Content, error) {
	a, err := s.SourceArtifact(ctx, k, namespace, name)
	if err != nil {
		return nil, err
	}
	return s.artifactCache.Read(ctx, a, path)
}

// Close releases the store's temporary files.
func (s *Store) Close() {
	s.artifactCache.Close()
}
