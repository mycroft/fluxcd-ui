package flux

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(typ string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: status, Reason: reason, Message: msg}
}

func TestComputeStatus(t *testing.T) {
	ready := cond("Ready", metav1.ConditionTrue, "Succeeded", "stored artifact")
	notReady := cond("Ready", metav1.ConditionFalse, "GitOperationFailed", "auth failed")
	readyUnknown := cond("Ready", metav1.ConditionUnknown, "Progressing", "reconciliation in progress")
	reconciling := cond("Reconciling", metav1.ConditionTrue, "ProgressingWithRetry", "retrying")
	stalled := cond("Stalled", metav1.ConditionTrue, "InvalidURL", "bad url")

	tests := []struct {
		name        string
		suspended   bool
		gen, obsGen int64
		conds       []metav1.Condition
		want        State
		wantReason  string
	}{
		{name: "ready", gen: 2, obsGen: 2, conds: []metav1.Condition{ready}, want: StateReady, wantReason: "Succeeded"},
		{name: "ready false", gen: 1, obsGen: 1, conds: []metav1.Condition{notReady}, want: StateFailed, wantReason: "GitOperationFailed"},
		{name: "failing while retrying", gen: 1, obsGen: 1, conds: []metav1.Condition{notReady, reconciling}, want: StateFailed},
		{name: "stalled wins over ready", gen: 1, obsGen: 1, conds: []metav1.Condition{ready, stalled}, want: StateFailed, wantReason: "InvalidURL"},
		{name: "ready unknown", gen: 1, obsGen: 1, conds: []metav1.Condition{readyUnknown}, want: StateProgressing},
		{name: "reconciling", gen: 1, obsGen: 1, conds: []metav1.Condition{ready, reconciling}, want: StateProgressing},
		{name: "spec not yet observed", gen: 3, obsGen: 2, conds: []metav1.Condition{ready}, want: StateProgressing},
		{name: "suspended wins", suspended: true, gen: 1, obsGen: 1, conds: []metav1.Condition{notReady}, want: StateSuspended, wantReason: "GitOperationFailed"},
		{name: "no conditions", gen: 1, want: StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeStatus(tt.suspended, tt.gen, tt.obsGen, tt.conds)
			if got.State != tt.want {
				t.Errorf("state = %s, want %s", got.State, tt.want)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}
