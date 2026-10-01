package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fluxcd/cli-utils/pkg/kstatus/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// Health is the state of an object managed by a Kustomization or HelmRelease.
type Health string

const (
	HealthReady       Health = "Ready"
	HealthProgressing Health = "Progressing"
	HealthFailed      Health = "Failed"
	HealthTerminating Health = "Terminating"
	HealthSuspended   Health = "Suspended"
	HealthMissing     Health = "Missing"
	HealthUnknown     Health = "Unknown"
	HealthNoAccess    Health = "No access"
)

// ObjectHealth is the health of one managed object.
type ObjectHealth struct {
	Health  Health
	Message string
}

// Problem reports whether the health needs attention. Unknown and "no
// access" are not problems with the object itself.
func (h ObjectHealth) Problem() bool {
	switch h.Health {
	case HealthFailed, HealthMissing, HealthProgressing, HealthTerminating:
		return true
	default:
		return false
	}
}

const (
	healthConcurrency = 8
	healthCacheTTL    = 10 * time.Second
	healthTimeout     = 20 * time.Second
)

// existenceOnly lists kinds without a status: they are healthy if they
// exist, so only their metadata is read (some, like ConfigMaps, can be large).
var existenceOnly = map[schema.GroupKind]bool{
	{Kind: "ConfigMap"}:      true,
	{Kind: "Secret"}:         true,
	{Kind: "ServiceAccount"}: true,
	{Group: "rbac.authorization.k8s.io", Kind: "Role"}:               true,
	{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}:        true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:        true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}: true,
}

// HealthKey identifies an inventory entry in the result of ManagedHealth.
func HealthKey(e flux.InventoryEntry) string {
	return e.Group + "/" + e.Kind + "/" + e.Namespace + "/" + e.Name
}

type listKey struct {
	gvk       schema.GroupVersionKind
	namespace string // empty for cluster-scoped kinds
}

type healthCacheEntry struct {
	byName map[string]ObjectHealth
	err    error
	at     time.Time
}

// healthCache keeps computed health, not objects, per kind and namespace.
type healthCache struct {
	mu      sync.Mutex
	entries map[listKey]healthCacheEntry
}

// ManagedHealth returns the health of each inventory entry, keyed by
// HealthKey. Flux objects come from the cache; other objects are listed once
// per kind and namespace, and their health computed with kstatus, as Flux's
// own health checks do.
func (s *Store) ManagedHealth(ctx context.Context, entries []flux.InventoryEntry) map[string]ObjectHealth {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	out := make(map[string]ObjectHealth, len(entries))
	groups := map[listKey][]flux.InventoryEntry{}
	for _, e := range entries {
		if e.KindID != "" {
			out[HealthKey(e)] = s.fluxHealth(ctx, e)
			continue
		}
		k := listKey{gvk: schema.GroupVersionKind{Group: e.Group, Version: e.Version, Kind: e.Kind}, namespace: e.Namespace}
		groups[k] = append(groups[k], e)
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, healthConcurrency)
	)
	for k, es := range groups {
		wg.Go(func() {
			sem <- struct{}{}
			byName, err := s.healthOfList(ctx, k)
			<-sem

			mu.Lock()
			defer mu.Unlock()
			for _, e := range es {
				out[HealthKey(e)] = entryHealth(e, byName, err)
			}
		})
	}
	wg.Wait()
	return out
}

func entryHealth(e flux.InventoryEntry, byName map[string]ObjectHealth, err error) ObjectHealth {
	switch {
	case apierrors.IsForbidden(err):
		return ObjectHealth{Health: HealthNoAccess, Message: "fluxcd-ui may not list " + e.Kind + " objects: grant it with managedObjects.status.extraRules"}
	case apimeta.IsNoMatchError(err):
		return ObjectHealth{Health: HealthUnknown, Message: "the cluster does not serve " + e.Kind + " in " + e.Group + "/" + e.Version}
	case err != nil:
		return ObjectHealth{Health: HealthUnknown, Message: err.Error()}
	}
	if h, ok := byName[e.Name]; ok {
		return h
	}
	return ObjectHealth{Health: HealthMissing, Message: "not found in the cluster"}
}

// healthOfList lists one kind in one namespace and computes the health of
// every object, caching the result briefly.
func (s *Store) healthOfList(ctx context.Context, k listKey) (map[string]ObjectHealth, error) {
	s.health.mu.Lock()
	if c, ok := s.health.entries[k]; ok && time.Since(c.at) < healthCacheTTL {
		s.health.mu.Unlock()
		return c.byName, c.err
	}
	s.health.mu.Unlock()

	byName, err := s.listHealth(ctx, k)
	if ctx.Err() == nil { // don't cache answers cut short by the timeout
		s.health.mu.Lock()
		if s.health.entries == nil {
			s.health.entries = map[listKey]healthCacheEntry{}
		}
		s.health.entries[k] = healthCacheEntry{byName: byName, err: err, at: time.Now()}
		s.health.mu.Unlock()
	}
	return byName, err
}

func (s *Store) listHealth(ctx context.Context, k listKey) (map[string]ObjectHealth, error) {
	listGVK := k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List")
	byName := map[string]ObjectHealth{}

	if existenceOnly[k.gvk.GroupKind()] {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(listGVK)
		if err := s.writer.List(ctx, list, client.InNamespace(k.namespace)); err != nil {
			return nil, err
		}
		for _, o := range list.Items {
			h := ObjectHealth{Health: HealthReady}
			if o.DeletionTimestamp != nil {
				h = ObjectHealth{Health: HealthTerminating, Message: "being deleted"}
			}
			byName[o.Name] = h
		}
		return byName, nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	if err := s.writer.List(ctx, list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		byName[list.Items[i].GetName()] = kstatusHealth(&list.Items[i])
	}
	return byName, nil
}

func kstatusHealth(obj *unstructured.Unstructured) ObjectHealth {
	res, err := status.Compute(obj)
	if err != nil {
		return ObjectHealth{Health: HealthUnknown, Message: fmt.Sprintf("computing status: %v", err)}
	}
	h := ObjectHealth{Message: res.Message}
	switch res.Status {
	case status.CurrentStatus:
		h.Health = HealthReady
	case status.InProgressStatus:
		h.Health = HealthProgressing
	case status.FailedStatus:
		h.Health = HealthFailed
	case status.TerminatingStatus:
		h.Health = HealthTerminating
	case status.NotFoundStatus:
		h.Health = HealthMissing
	default:
		h.Health = HealthUnknown
	}
	return h
}

// fluxHealth reports a managed Flux object's state, as the UI shows it.
func (s *Store) fluxHealth(ctx context.Context, e flux.InventoryEntry) ObjectHealth {
	k, ok := flux.KindByID(e.KindID)
	if !ok || !s.State(k.ID).Synced {
		return ObjectHealth{Health: HealthUnknown}
	}
	d, err := s.Get(ctx, k, e.Namespace, e.Name)
	switch {
	case apierrors.IsNotFound(err):
		return ObjectHealth{Health: HealthMissing, Message: "not found in the cluster"}
	case err != nil:
		return ObjectHealth{Health: HealthUnknown, Message: err.Error()}
	}
	h := ObjectHealth{Message: d.Status.Message}
	switch d.Status.State {
	case flux.StateReady, flux.StateStatic:
		h.Health = HealthReady
	case flux.StateFailed:
		h.Health = HealthFailed
	case flux.StateProgressing:
		h.Health = HealthProgressing
	case flux.StateSuspended:
		h.Health = HealthSuspended
	default:
		h.Health = HealthUnknown
	}
	return h
}
