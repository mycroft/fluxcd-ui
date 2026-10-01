// Package diff compares what a Kustomization's source would apply with
// what the cluster holds, without changing anything.
package diff

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/kustomize"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// Action is what applying the source would do to an object.
type Action string

const (
	ActionChanged   Action = "Changed"
	ActionCreated   Action = "Created"
	ActionPruned    Action = "Pruned"
	ActionOrphaned  Action = "Not pruned" // removed from the source, but pruning is disabled
	ActionSkipped   Action = "Skipped"
	ActionUnchanged Action = "Unchanged"
)

// actionOrder sorts results: what would change first.
var actionOrder = []Action{ActionChanged, ActionCreated, ActionPruned, ActionOrphaned, ActionSkipped, ActionUnchanged}

const (
	diffTimeout     = 2 * time.Minute
	listConcurrency = 8
	// maxConcurrentDiffs bounds the builds running at once.
	maxConcurrentDiffs = 2

	reconcileAnnotation = "kustomize.toolkit.fluxcd.io/reconcile"
	ssaAnnotation       = "kustomize.toolkit.fluxcd.io/ssa"
)

// Object is the outcome for one object.
type Object struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	Action     Action
	Changes    []Change // for ActionChanged
	Note       string   // why an object was skipped
}

// Result is the diff of a Kustomization.
type Result struct {
	Revision        string // the source artifact compared
	AppliedRevision string // what the Kustomization last applied
	Objects         []Object
	Counts          map[Action]int
}

// Pending is the number of objects applying the source would change,
// create or delete.
func (r *Result) Pending() int {
	return r.Counts[ActionChanged] + r.Counts[ActionCreated] + r.Counts[ActionPruned]
}

// Differ computes Kustomization diffs.
type Differ struct {
	reader  client.Reader // cached Flux objects
	client  client.Client // live objects, and ConfigMaps for substitutions
	http    *http.Client
	rewrite func(string) (string, error) // nil to fetch artifacts directly
	tempDir string
	sem     chan struct{}
}

// New returns a Differ. rewrite, when set, maps artifact URLs to reachable
// ones (see ProxyURL); tempDir holds builds ("" for the default).
func New(reader client.Reader, c client.Client, hc *http.Client, rewrite func(string) (string, error), tempDir string) *Differ {
	return &Differ{reader: reader, client: c, http: hc, rewrite: rewrite, tempDir: tempDir, sem: make(chan struct{}, maxConcurrentDiffs)}
}

// Kustomization builds a Kustomization from the artifact its source holds,
// as kustomize-controller does, and compares the result with the cluster.
func (d *Differ) Kustomization(ctx context.Context, namespace, name string) (*Result, error) {
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	ks := &kustomizev1.Kustomization{}
	if err := d.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, ks); err != nil {
		return nil, err
	}
	artifact, err := d.sourceArtifact(ctx, ks)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(d.tempDir, "fluxcd-ui-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	artifactURL := artifact.URL
	if d.rewrite != nil {
		if artifactURL, err = d.rewrite(artifactURL); err != nil {
			return nil, err
		}
	}
	if err := fetchArtifact(ctx, d.http, artifactURL, artifact.Digest, tmp); err != nil {
		return nil, err
	}
	desired, err := build(ctx, d.client, ks, tmp+"/src")
	if err != nil {
		return nil, err
	}

	r := d.compare(ctx, ks, desired)
	r.Revision, r.AppliedRevision = artifact.Revision, ks.Status.LastAppliedRevision
	return r, nil
}

func (d *Differ) sourceArtifact(ctx context.Context, ks *kustomizev1.Kustomization) (*meta.Artifact, error) {
	ref := ks.Spec.SourceRef
	key := client.ObjectKey{Namespace: cmp.Or(ref.Namespace, ks.Namespace), Name: ref.Name}
	var (
		src      client.Object
		artifact func() *meta.Artifact
	)
	switch ref.Kind {
	case sourcev1.GitRepositoryKind:
		o := &sourcev1.GitRepository{}
		src, artifact = o, func() *meta.Artifact { return o.Status.Artifact }
	case sourcev1.OCIRepositoryKind:
		o := &sourcev1.OCIRepository{}
		src, artifact = o, func() *meta.Artifact { return o.Status.Artifact }
	case sourcev1.BucketKind:
		o := &sourcev1.Bucket{}
		src, artifact = o, func() *meta.Artifact { return o.Status.Artifact }
	default:
		return nil, fmt.Errorf("diffing Kustomizations sourced from a %s is not supported", ref.Kind)
	}
	if err := d.reader.Get(ctx, key, src); err != nil {
		return nil, fmt.Errorf("getting %s %s: %w", ref.Kind, key, err)
	}
	a := artifact()
	if a == nil {
		return nil, fmt.Errorf("%s %s has no artifact yet", ref.Kind, key)
	}
	return a, nil
}

// build runs kustomize-controller's build: generate the kustomization.yaml
// from the Kustomization's spec, build it, then apply post-build
// substitutions. Remote bases are not allowed.
func build(ctx context.Context, c client.Client, ks *kustomizev1.Kustomization, root string) ([]*unstructured.Unstructured, error) {
	dirPath, err := securejoin.SecureJoin(root, ks.Spec.Path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dirPath); err != nil {
		return nil, fmt.Errorf("path %q not found in the source artifact", ks.Spec.Path)
	}

	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ks)
	if err != nil {
		return nil, err
	}
	u := unstructured.Unstructured{Object: raw}
	u.SetAPIVersion(kustomizev1.GroupVersion.String())
	u.SetKind(kustomizev1.KustomizationKind)

	if _, err := kustomize.NewGenerator(root, u).WriteFile(dirPath); err != nil {
		return nil, fmt.Errorf("generating kustomization.yaml: %w", err)
	}
	m, err := kustomize.SecureBuild(root, dirPath, false)
	if err != nil {
		return nil, fmt.Errorf("building: %w", err)
	}

	var out []*unstructured.Unstructured
	for _, res := range m.Resources() {
		if ks.Spec.PostBuild != nil {
			subst, err := kustomize.SubstituteVariables(ctx, c, u, res)
			if err != nil {
				return nil, fmt.Errorf("post-build substitutions: %w", err)
			}
			if subst != nil {
				res = subst
			}
		}
		obj, err := res.Map()
		if err != nil {
			return nil, err
		}
		out = append(out, &unstructured.Unstructured{Object: obj})
	}
	return out, nil
}

type listKey struct {
	gvk       schema.GroupVersionKind
	namespace string
}

type liveList struct {
	byName map[string]*unstructured.Unstructured
	err    error
}

func (d *Differ) compare(ctx context.Context, ks *kustomizev1.Kustomization, desired []*unstructured.Unstructured) *Result {
	r := &Result{Counts: map[Action]int{}}
	add := func(o Object) {
		r.Objects = append(r.Objects, o)
		r.Counts[o.Action]++
	}

	// Where each object lives, then one list per kind and namespace.
	keys := make([]listKey, len(desired))
	lists := map[listKey]*liveList{}
	inSource := map[string]bool{}
	for i, o := range desired {
		gvk := o.GroupVersionKind()
		ns := o.GetNamespace()
		if namespaced, err := d.namespaced(gvk); err == nil {
			switch {
			case !namespaced:
				ns = ""
			case ns == "":
				ns = "default"
			}
		}
		keys[i] = listKey{gvk: gvk, namespace: ns}
		lists[keys[i]] = nil
		inSource[objectID(gvk.Group, gvk.Kind, ns, o.GetName())] = true
	}
	d.listLive(ctx, lists)

	for i, o := range desired {
		gvk := o.GroupVersionKind()
		obj := Object{APIVersion: o.GetAPIVersion(), Kind: o.GetKind(), Namespace: keys[i].namespace, Name: o.GetName()}
		if note := skipReason(o); note != "" {
			obj.Action, obj.Note = ActionSkipped, note
			add(obj)
			continue
		}
		list := lists[keys[i]]
		switch {
		case apierrors.IsForbidden(list.err):
			obj.Action, obj.Note = ActionSkipped, "fluxcd-ui may not read "+gvk.Kind+" objects: grant it with managedObjects.status.extraRules"
		case apimeta.IsNoMatchError(list.err):
			obj.Action, obj.Note = ActionSkipped, "the cluster does not serve "+o.GetAPIVersion()+" "+gvk.Kind+" (yet)"
		case list.err != nil:
			obj.Action, obj.Note = ActionSkipped, list.err.Error()
		default:
			live, ok := list.byName[o.GetName()]
			switch {
			case !ok:
				obj.Action = ActionCreated
			case strings.EqualFold(o.GetAnnotations()[ssaAnnotation], "IfNotPresent"):
				obj.Action, obj.Note = ActionSkipped, "applied only when absent (kustomize.toolkit.fluxcd.io/ssa: IfNotPresent)"
			default:
				obj.Changes = compareObjects(o.Object, live.Object)
				obj.Action = ActionUnchanged
				if len(obj.Changes) > 0 {
					obj.Action = ActionChanged
				}
			}
		}
		add(obj)
	}

	// What the Kustomization applied last time, and its source no longer has.
	if inv := ks.Status.Inventory; inv != nil {
		for _, e := range inv.Entries {
			ie, ok := flux.ParseInventoryID(e.ID)
			if !ok || inSource[objectID(ie.Group, ie.Kind, ie.Namespace, ie.Name)] {
				continue
			}
			apiVersion := e.Version
			if ie.Group != "" {
				apiVersion = ie.Group + "/" + e.Version
			}
			obj := Object{APIVersion: apiVersion, Kind: ie.Kind, Namespace: ie.Namespace, Name: ie.Name, Action: ActionPruned}
			if !ks.Spec.Prune {
				obj.Action, obj.Note = ActionOrphaned, "no longer in the source; pruning is disabled, so it stays"
			}
			add(obj)
		}
	}

	slices.SortFunc(r.Objects, func(a, b Object) int {
		return cmp.Or(
			cmp.Compare(slices.Index(actionOrder, a.Action), slices.Index(actionOrder, b.Action)),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name),
		)
	})
	return r
}

// skipReason says why an object is not compared, as kustomize-controller
// would not apply it either, or as its content cannot be compared.
func skipReason(o *unstructured.Unstructured) string {
	switch {
	case o.GetAPIVersion() == "v1" && o.GetKind() == "Secret":
		return "Secrets are not compared"
	case o.Object["sops"] != nil:
		return "encrypted with SOPS"
	case strings.EqualFold(o.GetAnnotations()[reconcileAnnotation], "disabled"):
		return "reconciliation disabled (kustomize.toolkit.fluxcd.io/reconcile: disabled)"
	case strings.EqualFold(o.GetAnnotations()[ssaAnnotation], "Ignore"):
		return "ignored (kustomize.toolkit.fluxcd.io/ssa: Ignore)"
	default:
		return ""
	}
}

func (d *Differ) namespaced(gvk schema.GroupVersionKind) (bool, error) {
	m, err := d.client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, err
	}
	return m.Scope.Name() == apimeta.RESTScopeNameNamespace, nil
}

// listLive fills lists with the live objects of each kind and namespace.
// Each list is fetched concurrently into its own slot; the map is only
// written once they are all done.
func (d *Differ) listLive(ctx context.Context, lists map[listKey]*liveList) {
	keys := slices.Collect(maps.Keys(lists))
	results := make([]*liveList, len(keys))
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, listConcurrency)
	)
	for i, k := range keys {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ul := &unstructured.UnstructuredList{}
			ul.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
			l := &liveList{byName: map[string]*unstructured.Unstructured{}}
			if l.err = d.client.List(ctx, ul, client.InNamespace(k.namespace)); l.err == nil {
				for j := range ul.Items {
					l.byName[ul.Items[j].GetName()] = &ul.Items[j]
				}
			}
			results[i] = l
		})
	}
	wg.Wait()
	for i, k := range keys {
		lists[k] = results[i]
	}
}

func objectID(group, kind, namespace, name string) string {
	return group + "/" + kind + "/" + namespace + "/" + name
}
