package store

import (
	"cmp"
	"context"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// ResolveDependencies fills in the state of the objects d depends on, and
// lists the objects of its kind that depend on it.
func (s *Store) ResolveDependencies(ctx context.Context, k flux.Kind, d *flux.Detail) error {
	if !d.HasDependencies {
		return nil
	}
	for i := range d.DependsOn {
		dep := &d.DependsOn[i]
		obj := k.NewObject()
		switch err := s.reader.Get(ctx, client.ObjectKey{Namespace: dep.Namespace, Name: dep.Name}, obj); {
		case err == nil:
			dep.Found, dep.Status = true, k.Describe(obj).Status
		case !apierrors.IsNotFound(err):
			return err
		}
	}

	list := k.NewList()
	if err := s.reader.List(ctx, list, client.UnsafeDisableDeepCopy); err != nil {
		return err
	}
	items, err := apimeta.ExtractList(list)
	if err != nil {
		return err
	}
	d.RequiredBy = nil
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}
		if slices.ContainsFunc(flux.DependenciesOf(obj), func(dep flux.Dependency) bool {
			return dep.Namespace == d.Namespace && dep.Name == d.Name
		}) {
			d.RequiredBy = append(d.RequiredBy, flux.Dependency{
				Kind: k.ID, Namespace: obj.GetNamespace(), Name: obj.GetName(), Found: true, Status: k.Describe(obj).Status,
			})
		}
	}
	slices.SortFunc(d.RequiredBy, func(a, b flux.Dependency) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return nil
}
