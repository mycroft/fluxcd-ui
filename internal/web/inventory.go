package web

import (
	"github.com/mycroft/fluxcd-ui/internal/flux"
	"github.com/mycroft/fluxcd-ui/internal/store"
)

// collapseAbove is the inventory size above which groups start collapsed,
// unless they hold objects needing attention.
const collapseAbove = 30

// healthOrder is the order of the health summary: problems first.
var healthOrder = []store.Health{
	store.HealthFailed, store.HealthMissing, store.HealthProgressing, store.HealthTerminating,
	store.HealthSuspended, store.HealthReady, store.HealthUnknown, store.HealthNoAccess,
}

// inventoryView is the managed objects section of a drawer.
type inventoryView struct {
	Path    string // the object's /objects/... path
	Count   int
	Groups  []inventoryGroup
	Enabled bool // health checks are enabled
	Checked bool // health was checked
	Summary []healthCount
}

type inventoryGroup struct {
	Kind       string
	APIVersion string
	Entries    []inventoryEntry
	Problems   int
	Open       bool
}

type inventoryEntry struct {
	flux.InventoryEntry
	Health store.ObjectHealth
}

type healthCount struct {
	Health store.Health
	Count  int
}

// newInventoryView builds the section; health is nil until checked.
func newInventoryView(path string, d flux.Detail, enabled bool, health map[string]store.ObjectHealth) inventoryView {
	v := inventoryView{Path: path, Count: d.InventoryCount(), Enabled: enabled, Checked: health != nil}
	counts := map[store.Health]int{}
	for _, g := range d.Inventory {
		ig := inventoryGroup{Kind: g.Kind, APIVersion: g.APIVersion}
		for _, e := range g.Entries {
			h := health[store.HealthKey(e)]
			if v.Checked {
				counts[h.Health]++
			}
			if h.Problem() {
				ig.Problems++
			}
			ig.Entries = append(ig.Entries, inventoryEntry{InventoryEntry: e, Health: h})
		}
		ig.Open = v.Count <= collapseAbove || ig.Problems > 0
		v.Groups = append(v.Groups, ig)
	}
	for _, h := range healthOrder {
		if counts[h] > 0 {
			v.Summary = append(v.Summary, healthCount{Health: h, Count: counts[h]})
		}
	}
	return v
}
