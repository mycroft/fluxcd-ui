package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

// statusFilter is one of the status chips of the filter bar.
type statusFilter struct {
	Value  string
	Label  string
	States []flux.State // the states it matches; empty for "All"
}

var statusFilters = []statusFilter{
	{Value: "", Label: "All"},
	// Static objects (OCI HelmRepositories) are healthy, just never
	// reconciled, so they count as ready.
	{Value: "ready", Label: "Ready", States: []flux.State{flux.StateReady, flux.StateStatic}},
	{Value: "failing", Label: "Failing", States: []flux.State{flux.StateFailed}},
	{Value: "progressing", Label: "Progressing", States: []flux.State{flux.StateProgressing}},
	{Value: "suspended", Label: "Suspended", States: []flux.State{flux.StateSuspended}},
}

// matches reports whether an object in state st passes this filter.
func (s statusFilter) matches(st flux.State) bool {
	return len(s.States) == 0 || slices.Contains(s.States, st)
}

// Dot is the state whose color the chip shows, if any.
func (s statusFilter) Dot() flux.State {
	if len(s.States) == 0 {
		return ""
	}
	return s.States[0]
}

// Filter is the filter bar state, carried in the query string.
type Filter struct {
	Query     string // case-insensitive substring
	Namespace string
	Status    string // one of statusFilters' values
}

func parseFilter(r *http.Request) Filter {
	q := r.URL.Query()
	f := Filter{
		Query:     strings.TrimSpace(q.Get("q")),
		Namespace: q.Get("ns"),
		Status:    q.Get("status"),
	}
	if !slices.ContainsFunc(statusFilters, func(s statusFilter) bool { return s.Value == f.Status }) {
		f.Status = ""
	}
	return f
}

// Active reports whether any filter is set.
func (f Filter) Active() bool {
	return f.Query != "" || f.Namespace != "" || f.Status != ""
}

// URL returns the page URL reflecting this filter.
func (f Filter) URL() string {
	v := url.Values{}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.Namespace != "" {
		v.Set("ns", f.Namespace)
	}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// matchText applies the search and namespace filters.
func (f Filter) matchText(r flux.Row) bool {
	if f.Namespace != "" && r.Namespace != f.Namespace {
		return false
	}
	if f.Query == "" {
		return true
	}
	q := strings.ToLower(f.Query)
	if contains(r.Namespace+"/"+r.Name, q) || contains(r.Revision, q) {
		return true
	}
	for _, c := range r.Cells {
		if contains(c.Text, q) {
			return true
		}
	}
	return false
}

// matchStatus applies the status filter.
func (f Filter) matchStatus(r flux.Row) bool {
	for _, s := range statusFilters {
		if s.Value == f.Status {
			return s.matches(r.Status.State)
		}
	}
	return true
}

func contains(s, lowerSubstr string) bool {
	return strings.Contains(strings.ToLower(s), lowerSubstr)
}
