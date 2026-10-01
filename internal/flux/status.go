package flux

import (
	"github.com/fluxcd/pkg/apis/meta"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// State is the summarized reconciliation state of a Flux object.
type State string

const (
	StateReady       State = "Ready"
	StateFailed      State = "Failed"
	StateProgressing State = "Progressing"
	StateSuspended   State = "Suspended"
	// StateStatic marks objects that are not reconciled by design, such as
	// HelmRepositories of type "oci".
	StateStatic  State = "Static"
	StateUnknown State = "Unknown"
)

// Status is the summarized state of an object along with the reason and
// message of the condition it was derived from.
type Status struct {
	State   State
	Reason  string
	Message string
}

// computeStatus derives a Status from an object's suspend flag, generations
// and conditions. Order matters: suspension wins over everything, a failure
// wins over an in-progress reconciliation.
func computeStatus(suspended bool, generation, observedGeneration int64, conds []metav1.Condition) Status {
	ready := apimeta.FindStatusCondition(conds, meta.ReadyCondition)
	stalled := apimeta.FindStatusCondition(conds, meta.StalledCondition)
	reconciling := apimeta.FindStatusCondition(conds, meta.ReconcilingCondition)

	st := Status{State: StateUnknown}
	if ready != nil {
		st.Reason, st.Message = ready.Reason, ready.Message
	}

	switch {
	case suspended:
		st.State = StateSuspended
	case stalled != nil && stalled.Status == metav1.ConditionTrue:
		st.State, st.Reason, st.Message = StateFailed, stalled.Reason, stalled.Message
	case ready != nil && ready.Status == metav1.ConditionFalse:
		st.State = StateFailed
	case reconciling != nil && reconciling.Status == metav1.ConditionTrue,
		ready != nil && ready.Status == metav1.ConditionUnknown,
		ready != nil && observedGeneration < generation:
		st.State = StateProgressing
	case ready != nil && ready.Status == metav1.ConditionTrue:
		st.State = StateReady
	}
	return st
}
