package flux

import (
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
)

func TestParseInventoryID(t *testing.T) {
	tests := []struct {
		id                           string
		namespace, name, group, kind string
	}{
		{"alloy_alloy__ServiceAccount", "alloy", "alloy", "", "ServiceAccount"},
		{"_alloy_rbac.authorization.k8s.io_ClusterRole", "", "alloy", "rbac.authorization.k8s.io", "ClusterRole"},
		{"_system__aggregate-to-view_rbac.authorization.k8s.io_ClusterRole", "", "system:aggregate-to-view", "rbac.authorization.k8s.io", "ClusterRole"},
		{"apps_my_app_apps_Deployment", "apps", "my_app", "apps", "Deployment"},
		{"_gitrepositories.source.toolkit.fluxcd.io_apiextensions.k8s.io_CustomResourceDefinition", "", "gitrepositories.source.toolkit.fluxcd.io", "apiextensions.k8s.io", "CustomResourceDefinition"},
	}
	for _, tt := range tests {
		e, ok := parseInventoryID(tt.id)
		if !ok || e.Namespace != tt.namespace || e.Name != tt.name || e.Group != tt.group || e.Kind != tt.kind {
			t.Errorf("parseInventoryID(%q) = %+v %t", tt.id, e, ok)
		}
	}
	for _, id := range []string{"", "nounderscore", "ns_name", "ns__Kind"} {
		if _, ok := parseInventoryID(id); ok {
			t.Errorf("parseInventoryID(%q) accepted a malformed ID", id)
		}
	}
}

func TestGroupInventory(t *testing.T) {
	groups := groupInventory([]inventoryRef{
		{"apps_web_apps_Deployment", "v1"},
		{"apps_api_apps_Deployment", "v1"},
		{"apps_web__Service", "v1"},
		{"apps_podinfo_helm.toolkit.fluxcd.io_HelmRelease", "v2"},
		{"garbage", "v1"},
	})
	if len(groups) != 3 {
		t.Fatalf("%d groups: %+v", len(groups), groups)
	}
	dep, hr, svc := groups[0], groups[1], groups[2] // sorted by kind
	if dep.Kind != "Deployment" || dep.APIVersion != "apps/v1" || dep.Entries[0].Name != "api" || dep.Entries[1].Name != "web" {
		t.Errorf("deployments = %+v", dep)
	}
	if svc.Kind != "Service" || svc.APIVersion != "v1" {
		t.Errorf("services = %+v", svc)
	}
	// Flux objects link to their own drawer.
	if hr.Kind != "HelmRelease" || hr.Entries[0].KindID != "helmreleases" || dep.Entries[0].KindID != "" {
		t.Errorf("kind IDs: helmrelease %q, deployment %q", hr.Entries[0].KindID, dep.Entries[0].KindID)
	}
}

func TestKustomizationInventory(t *testing.T) {
	k := kind(t, "kustomizations")
	o := &kustomizev1.Kustomization{ObjectMeta: objectMeta("flux-system", "apps")}
	d := k.Describe(o)
	if !d.HasInventory || d.Inventory != nil || d.InventoryCount() != 0 {
		t.Fatalf("without inventory: %+v", d.Inventory)
	}
	o.Status.Inventory = &kustomizev1.ResourceInventory{Entries: []kustomizev1.ResourceRef{
		{ID: "apps_web_apps_Deployment", Version: "v1"},
		{ID: "apps_web__Service", Version: "v1"},
	}}
	if d := k.Describe(o); d.InventoryCount() != 2 || len(d.Inventory) != 2 {
		t.Errorf("inventory = %+v", d.Inventory)
	}
	if kind(t, "gitrepositories").Describe(kind(t, "gitrepositories").NewObject()).HasInventory {
		t.Error("GitRepositories have no inventory")
	}
}
