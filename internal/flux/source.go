package flux

// SourceStatus is the state of the source an object reconciles from, shown
// next to its reference. Describe functions set the reference; the store
// resolves it.
type SourceStatus struct {
	Kind      string // e.g. GitRepository
	Namespace string
	Name      string
	// Checked reports whether the source's kind is watched and synced, so
	// that Found and Status are meaningful.
	Checked bool
	Found   bool
	Status  Status
}

// State is the source's state; a missing source counts as failed, as the
// object cannot reconcile without it.
func (s *SourceStatus) State() State {
	if !s.Found {
		return StateFailed
	}
	return s.Status.State
}

// OK reports whether the source needs no attention.
func (s *SourceStatus) OK() bool {
	st := s.State()
	return st == StateReady || st == StateStatic
}

// Label names the source's state in a few words, e.g. "Source failed".
func (s *SourceStatus) Label() string {
	switch {
	case !s.Found:
		return "Source not found"
	case s.Status.State == StateReady:
		return "Source ready"
	case s.Status.State == StateFailed:
		return "Source failed"
	case s.Status.State == StateProgressing:
		return "Source progressing"
	case s.Status.State == StateSuspended:
		return "Source suspended"
	case s.Status.State == StateStatic:
		return "Source static"
	default:
		return "Source state unknown"
	}
}

// Summary describes the source and its state, e.g. for a tooltip.
func (s *SourceStatus) Summary() string {
	ref := s.Kind + " " + s.Namespace + "/" + s.Name
	if !s.Found {
		return ref + " not found"
	}
	summary := ref + ": " + string(s.Status.State)
	if s.Status.Message != "" {
		summary += ". " + s.Status.Message
	}
	return summary
}

// KindByGroupKind returns the kind with the given API group and kind name.
func KindByGroupKind(group, kind string) (Kind, bool) {
	id, ok := kindIDs[[2]string{group, kind}]
	if !ok {
		return Kind{}, false
	}
	return KindByID(id)
}
