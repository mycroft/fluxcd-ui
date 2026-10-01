package flux

import (
	"cmp"
	"slices"
	"strings"
)

// InventoryEntry is an object applied by a Kustomization or a HelmRelease.
type InventoryEntry struct {
	Group     string
	Version   string
	Kind      string
	Namespace string // empty for cluster-scoped objects
	Name      string
	// KindID is set when the object is itself a Flux kind shown by the UI,
	// so the drawer can link to it.
	KindID string
}

// InventoryGroup holds the inventory entries of one kind.
type InventoryGroup struct {
	Kind       string
	APIVersion string
	Entries    []InventoryEntry
}

// kindIDs maps API group and kind to the ID of the kinds the UI shows. It is
// filled in init: Kinds' describe functions use it, so initializing it from
// Kinds in a variable declaration would be an initialization cycle.
var kindIDs = map[[2]string]string{}

func init() {
	for _, k := range Kinds {
		kindIDs[[2]string{k.GVK.Group, k.GVK.Kind}] = k.ID
	}
}

// inventoryRef is an inventory entry as stored in status.inventory.
type inventoryRef struct {
	id      string
	version string
}

// parseInventoryID parses an inventory ID, "<namespace>_<name>_<group>_<kind>"
// in the cli-utils format: names may contain underscores, and encode colons
// as "__". It sets the entry's Namespace, Name, Group and Kind.
func parseInventoryID(id string) (InventoryEntry, bool) {
	var e InventoryEntry
	namespace, rest, ok := strings.Cut(id, "_")
	if !ok {
		return e, false
	}
	i := strings.LastIndex(rest, "_")
	if i < 0 {
		return e, false
	}
	rest, e.Kind = rest[:i], rest[i+1:]
	i = strings.LastIndex(rest, "_")
	if i < 0 {
		return e, false
	}
	e.Namespace, e.Name, e.Group = namespace, strings.ReplaceAll(rest[:i], "__", ":"), rest[i+1:]
	return e, e.Name != "" && e.Kind != ""
}

// groupInventory parses inventory entries and groups them by kind, sorted by
// kind, then namespace and name. Unparseable entries are skipped.
func groupInventory(refs []inventoryRef) []InventoryGroup {
	byKind := map[[2]string]*InventoryGroup{}
	for _, r := range refs {
		e, ok := parseInventoryID(r.id)
		if !ok {
			continue
		}
		e.Version = r.version
		key := [2]string{e.Group, e.Kind}
		e.KindID = kindIDs[key]
		g := byKind[key]
		if g == nil {
			apiVersion := e.Version
			if e.Group != "" {
				apiVersion = e.Group + "/" + e.Version
			}
			g = &InventoryGroup{Kind: e.Kind, APIVersion: apiVersion}
			byKind[key] = g
		}
		g.Entries = append(g.Entries, e)
	}

	groups := make([]InventoryGroup, 0, len(byKind))
	for _, g := range byKind {
		slices.SortFunc(g.Entries, func(a, b InventoryEntry) int {
			return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
		})
		groups = append(groups, *g)
	}
	slices.SortFunc(groups, func(a, b InventoryGroup) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.APIVersion, b.APIVersion))
	})
	return groups
}
