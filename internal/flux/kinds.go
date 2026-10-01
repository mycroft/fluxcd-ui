// Package flux maps Flux custom resources to the view models rendered by the UI.
package flux

import (
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cell is a kind-specific table cell.
type Cell struct {
	Text string
	Mono bool
}

// Field is a labeled value shown in the detail drawer.
type Field struct {
	Label string
	Value string
	Mono  bool
	// Time, when set, is rendered as a relative time instead of Value.
	Time time.Time
}

// Row is one object in a section table.
type Row struct {
	Kind      string // Kind.ID
	Namespace string
	Name      string
	Cells     []Cell // one per Kind.Columns
	Revision  string // full revision; shortened at render time
	Status    Status
	Updated   time.Time
	// Pending reports a reconciliation that was requested but not yet
	// handled by the controller.
	Pending bool
	// ReconcileRequestedAt is the last reconciliation request, when its token
	// is a timestamp (as set by the flux CLI and this UI).
	ReconcileRequestedAt time.Time
}

// Detail is one object in the detail drawer.
type Detail struct {
	Row
	Fields             []Field
	Conditions         []metav1.Condition
	Generation         int64
	ObservedGeneration int64
	// HasSource reports whether the object reconciles from another Flux
	// object, which "reconcile with source" refreshes first.
	HasSource bool
}

// Reconcilable reports whether Flux reconciles the object at all: suspended
// objects are skipped, and static objects are never reconciled.
func (d Detail) Reconcilable() bool {
	return d.Status.State != StateSuspended && d.Status.State != StateStatic
}

// Kind describes a Flux resource type handled by the UI.
type Kind struct {
	ID      string // API resource name, also used as URL segment and SSE event name
	Title   string // plural display name
	GVK     schema.GroupVersionKind
	Columns []string // headers of Row.Cells

	newObject func() client.Object
	newList   func() client.ObjectList
	describe  func(client.Object) Detail
}

// Kinds lists the supported kinds in display order.
var Kinds = []Kind{
	{
		ID:        "gitrepositories",
		Title:     "GitRepositories",
		GVK:       sourcev1.GroupVersion.WithKind(sourcev1.GitRepositoryKind),
		Columns:   []string{"URL", "Ref"},
		newObject: func() client.Object { return &sourcev1.GitRepository{} },
		newList:   func() client.ObjectList { return &sourcev1.GitRepositoryList{} },
		describe:  describeAs(describeGitRepository),
	},
	{
		ID:        "ocirepositories",
		Title:     "OCIRepositories",
		GVK:       sourcev1.GroupVersion.WithKind(sourcev1.OCIRepositoryKind),
		Columns:   []string{"URL", "Ref"},
		newObject: func() client.Object { return &sourcev1.OCIRepository{} },
		newList:   func() client.ObjectList { return &sourcev1.OCIRepositoryList{} },
		describe:  describeAs(describeOCIRepository),
	},
	{
		ID:        "buckets",
		Title:     "Buckets",
		GVK:       sourcev1.GroupVersion.WithKind(sourcev1.BucketKind),
		Columns:   []string{"Endpoint", "Bucket", "Provider"},
		newObject: func() client.Object { return &sourcev1.Bucket{} },
		newList:   func() client.ObjectList { return &sourcev1.BucketList{} },
		describe:  describeAs(describeBucket),
	},
	{
		ID:        "helmrepositories",
		Title:     "HelmRepositories",
		GVK:       sourcev1.GroupVersion.WithKind(sourcev1.HelmRepositoryKind),
		Columns:   []string{"URL", "Type"},
		newObject: func() client.Object { return &sourcev1.HelmRepository{} },
		newList:   func() client.ObjectList { return &sourcev1.HelmRepositoryList{} },
		describe:  describeAs(describeHelmRepository),
	},
	{
		ID:        "helmcharts",
		Title:     "HelmCharts",
		GVK:       sourcev1.GroupVersion.WithKind(sourcev1.HelmChartKind),
		Columns:   []string{"Chart", "Version", "Source"},
		newObject: func() client.Object { return &sourcev1.HelmChart{} },
		newList:   func() client.ObjectList { return &sourcev1.HelmChartList{} },
		describe:  describeAs(describeHelmChart),
	},
	{
		ID:        "helmreleases",
		Title:     "HelmReleases",
		GVK:       helmv2.GroupVersion.WithKind(helmv2.HelmReleaseKind),
		Columns:   []string{"Chart", "Source", "App version"},
		newObject: func() client.Object { return &helmv2.HelmRelease{} },
		newList:   func() client.ObjectList { return &helmv2.HelmReleaseList{} },
		describe:  describeAs(describeHelmRelease),
	},
	{
		ID:        "kustomizations",
		Title:     "Kustomizations",
		GVK:       kustomizev1.GroupVersion.WithKind(kustomizev1.KustomizationKind),
		Columns:   []string{"Source", "Path"},
		newObject: func() client.Object { return &kustomizev1.Kustomization{} },
		newList:   func() client.ObjectList { return &kustomizev1.KustomizationList{} },
		describe:  describeAs(describeKustomization),
	},
}

// KindByID returns the kind with the given ID.
func KindByID(id string) (Kind, bool) {
	for _, k := range Kinds {
		if k.ID == id {
			return k, true
		}
	}
	return Kind{}, false
}

// NewScheme returns a scheme with all supported Flux API groups registered.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		sourcev1.AddToScheme,
		helmv2.AddToScheme,
		kustomizev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// NewObject returns an empty object of this kind.
func (k Kind) NewObject() client.Object { return k.newObject() }

// NewList returns an empty list of this kind.
func (k Kind) NewList() client.ObjectList { return k.newList() }

// Describe maps an object of this kind to its detail view model.
func (k Kind) Describe(obj client.Object) Detail {
	d := k.describe(obj)
	d.Kind = k.ID
	return d
}

// Rows maps every item of a list returned by NewList to a Row.
func (k Kind) Rows(list client.ObjectList) ([]Row, error) {
	items, err := apimeta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(items))
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			return nil, fmt.Errorf("unexpected item type %T in %s list", item, k.ID)
		}
		rows = append(rows, k.Describe(obj).Row)
	}
	return rows, nil
}

func describeAs[T client.Object](f func(T) Detail) func(client.Object) Detail {
	return func(obj client.Object) Detail { return f(obj.(T)) }
}
