package store

import (
	"context"
	"strings"
	"testing"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

func TestObjectYAML(t *testing.T) {
	scheme, err := flux.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "flux-system", Name: "fleet",
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "source.toolkit.fluxcd.io/v1",
				FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:url":{}}}`),
			}},
		},
		Spec:   sourcev1.GitRepositorySpec{URL: "https://example.com/fleet.git", Interval: metav1.Duration{Duration: time.Minute}},
		Status: sourcev1.GitRepositoryStatus{Conditions: readyCondition(metav1.ConditionTrue, "stored artifact")},
	}).Build()
	s := New(c, NewBroker(time.Millisecond), "gitrepositories")
	k, _ := flux.KindByID("gitrepositories")

	y, err := s.ObjectYAML(context.Background(), k, "flux-system", "fleet")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(y, "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n") {
		t.Errorf("YAML does not start with the object's type:\n%s", y)
	}
	for _, want := range []string{"  name: fleet\n", "  url: https://example.com/fleet.git\n", "  interval: 1m0s\n", "    message: stored artifact\n"} {
		if !strings.Contains(y, want) {
			t.Errorf("YAML lacks %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "managedFields") {
		t.Errorf("YAML has managed fields:\n%s", y)
	}

	if _, err := s.ObjectYAML(context.Background(), k, "flux-system", "gone"); !apierrors.IsNotFound(err) {
		t.Errorf("missing object: %v", err)
	}
}
