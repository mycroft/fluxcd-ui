// Package web serves the HTML UI: full pages, htmx fragments and the SSE
// stream that drives live updates.
package web

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/mycroft/fluxcd-ui/internal/artifact"
	"github.com/mycroft/fluxcd-ui/internal/authz"
	"github.com/mycroft/fluxcd-ui/internal/diff"
	"github.com/mycroft/fluxcd-ui/internal/flux"
	"github.com/mycroft/fluxcd-ui/internal/store"
)

//go:embed templates static
var assets embed.FS

const contentSecurityPolicy = "default-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// Backend provides the Flux objects to display and acts on them.
type Backend interface {
	State(kindID string) store.KindState
	Ready() bool
	List(ctx context.Context, k flux.Kind) ([]flux.Row, error)
	Get(ctx context.Context, k flux.Kind, namespace, name string) (flux.Detail, error)
	SetSuspended(ctx context.Context, k flux.Kind, namespace, name string, suspend bool) error
	ReconcilePlan(ctx context.Context, k flux.Kind, namespace, name string, withSource bool) ([]store.ObjectRef, error)
	RequestReconcile(ctx context.Context, refs []store.ObjectRef) error
	Related(ctx context.Context, k flux.Kind, namespace, name string) []store.ObjectRef
	Events(ctx context.Context, refs ...store.ObjectRef) []store.Event
	ControllerLogs(ctx context.Context, k flux.Kind, namespace, name string) ([]store.LogLine, error)
	ManagedHealth(ctx context.Context, entries []flux.InventoryEntry) map[string]store.ObjectHealth
	DiffKustomization(ctx context.Context, namespace, name string) (*diff.Result, error)
	ArtifactFiles(ctx context.Context, k flux.Kind, namespace, name string) (*meta.Artifact, []artifact.File, error)
	ArtifactFile(ctx context.Context, k flux.Kind, namespace, name, path string) (*artifact.Content, error)
	HelmReleaseContent(ctx context.Context, namespace, name string) (*store.ReleaseContent, error)
	ObjectYAML(ctx context.Context, k flux.Kind, namespace, name string) (string, error)
	ResolveDependencies(ctx context.Context, k flux.Kind, d *flux.Detail) error
}

// Options configures a Server.
type Options struct {
	Version string
	// Actions enables suspend, resume and reconcile.
	Actions bool
	// UserHeader names a request header carrying the user name set by an
	// authenticating proxy. When set, actions require it and are logged with it.
	UserHeader string
	// GroupsHeader names a request header carrying the user's groups, split
	// on GroupsSeparator (default ",").
	GroupsHeader    string
	GroupsSeparator string
	// Authorizer decides who may act. nil lets every user act.
	Authorizer authz.Authorizer
	// Logs offers the controller logs about an object in its drawer.
	Logs bool
	// ManagedStatus checks the health of the objects a Kustomization or
	// HelmRelease manages when its drawer opens.
	ManagedStatus bool
	// Diff offers to compare a Kustomization's source with the cluster.
	Diff bool
	// ArtifactBrowser offers to browse the files of a source's artifact.
	ArtifactBrowser bool
	// ReleaseContent offers the values and manifest of a HelmRelease's
	// current Helm release to users allowed to inspect it.
	ReleaseContent bool
	// YAMLView offers each object as YAML in its drawer.
	YAMLView bool
	// SignOutURL, when set, is linked next to the user's name: the
	// authenticating proxy's sign-out endpoint, which ends the session it
	// keeps (and with it, the user's groups as of signing in).
	SignOutURL string
}

// Server is the HTTP handler of the UI.
type Server struct {
	backend   Backend
	broker    *store.Broker
	tmpl      *template.Template
	opts      Options
	authz     authz.Authorizer
	log       *slog.Logger
	heartbeat time.Duration
	mux       *http.ServeMux
	handler   http.Handler
}

// New returns a Server reading objects from backend and change notifications
// from broker.
func New(backend Backend, broker *store.Broker, opts Options, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}

	s := &Server{
		backend:   backend,
		broker:    broker,
		tmpl:      tmpl,
		opts:      opts,
		authz:     opts.Authorizer,
		log:       log,
		heartbeat: 20 * time.Second,
		mux:       http.NewServeMux(),
	}
	if s.authz == nil {
		s.authz = authz.AllowAll{}
	}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /fragments/content", s.handleContent)
	s.mux.HandleFunc("GET /fragments/summary", s.handleSummary)
	s.mux.HandleFunc("GET /fragments/rows/{kind}", s.handleRows)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}", s.handleObject)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}/logs", s.handleLogs)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}/inventory", s.handleInventory)
	s.mux.HandleFunc("GET /objects/kustomizations/{namespace}/{name}/diff", s.handleDiff)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}/artifact", s.handleArtifact)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}/artifact/file", s.handleArtifactFile)
	s.mux.HandleFunc("GET /objects/helmreleases/{namespace}/{name}/release", s.handleRelease)
	s.mux.HandleFunc("GET /objects/{kind}/{namespace}/{name}/yaml", s.handleYAML)
	s.mux.HandleFunc("POST /objects/{kind}/{namespace}/{name}/{action}", s.handleAction)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.Handle("GET /static/", s.staticHandler(static))

	// Actions are POSTs made with the session of whoever is authenticated at
	// the proxy, so reject cross-site requests (CSRF).
	s.handler = http.NewCrossOriginProtection().Handler(s.mux)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	s.handler.ServeHTTP(w, r)
}

// section is one collapsible kind section of the page.
type section struct {
	Kind     flux.Kind
	State    store.KindState
	Err      error      // listing failed
	Rows     []flux.Row // rows matching the filter
	Total    int        // objects of this kind, unfiltered
	Failing  int        // failed objects matching the search and namespace filters
	Filtered bool
	OOB      bool // render the count badge as an out-of-band swap
}

// Open reports whether the section renders expanded: always, unless a filter
// hides all of its objects.
func (s section) Open() bool {
	return s.State.Installed && (!s.Filtered || len(s.Rows) > 0)
}

// Colspan is the number of table columns.
func (s section) Colspan() int { return len(s.Kind.Columns) + 4 }

type chip struct {
	statusFilter
	Count    int
	Selected bool
}

type summary struct {
	Chips []chip
	OOB   bool
}

type page struct {
	Filter     Filter
	Summary    summary
	Sections   []section
	Namespaces []string
	Version    string
	User       string
	SignOutURL string
	Theme      string // "light" or "dark" when chosen with the toggle; empty follows the OS
}

// buildPage lists every kind once and derives sections, summary counts and the
// namespace options from the result.
func (s *Server) buildPage(ctx context.Context, f Filter) page {
	p := page{Filter: f, Version: s.opts.Version}
	counts := map[flux.State]int{}
	total := 0
	namespaces := map[string]bool{}
	if f.Namespace != "" {
		namespaces[f.Namespace] = true // keep the selection even if it has no objects
	}

	for _, k := range flux.Kinds {
		sec, all := s.loadSection(ctx, k, f)
		p.Sections = append(p.Sections, sec)
		for _, r := range all {
			namespaces[r.Namespace] = true
			if f.matchText(r) {
				counts[r.Status.State]++
				total++
			}
		}
	}

	for _, sf := range statusFilters {
		c := chip{statusFilter: sf, Selected: sf.Value == f.Status}
		if len(sf.States) == 0 {
			c.Count = total
		}
		for _, st := range sf.States {
			c.Count += counts[st]
		}
		p.Summary.Chips = append(p.Summary.Chips, c)
	}
	p.Namespaces = slices.Sorted(maps.Keys(namespaces))
	return p
}

// loadSection returns the section of kind k under filter f, along with all of
// the kind's rows.
func (s *Server) loadSection(ctx context.Context, k flux.Kind, f Filter) (section, []flux.Row) {
	sec := section{Kind: k, State: s.backend.State(k.ID), Filtered: f.Active()}
	if !sec.State.Installed || !sec.State.Synced {
		return sec, nil
	}
	all, err := s.backend.List(ctx, k)
	if err != nil {
		sec.Err = err
		return sec, nil
	}
	sec.Total = len(all)
	for _, r := range all {
		if !f.matchText(r) {
			continue
		}
		if r.Status.State == flux.StateFailed {
			sec.Failing++
		}
		if f.matchStatus(r) {
			sec.Rows = append(sec.Rows, r)
		}
	}
	return sec, all
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	p := s.buildPage(r.Context(), parseFilter(r))
	p.User, p.SignOutURL = s.user(r), s.opts.SignOutURL
	p.Theme = theme(r)
	s.render(w, http.StatusOK, "index", p)
}

// themeCookie holds the theme picked with the header toggle (set by app.js).
// Rendering it server-side avoids a flash of the wrong theme on load.
const themeCookie = "theme"

func theme(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil || (c.Value != "light" && c.Value != "dark") {
		return ""
	}
	return c.Value
}

// handleContent serves all sections plus an out-of-band summary, for filter
// changes. The page URL follows the filter so it can be shared or reloaded.
func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	f := parseFilter(r)
	p := s.buildPage(r.Context(), f)
	p.Summary.OOB = true
	w.Header().Set("Hx-Push-Url", f.URL())
	s.render(w, http.StatusOK, "content-fragment", p)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "summary", s.buildPage(r.Context(), parseFilter(r)).Summary)
}

// handleRows serves one section's table body plus its count badge, for live
// updates of that kind.
func (s *Server) handleRows(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sec, _ := s.loadSection(r.Context(), k, parseFilter(r))
	sec.OOB = true
	s.render(w, http.StatusOK, "rows", sec)
}

type drawer struct {
	Kind      flux.Kind
	Namespace string
	Name      string
	Detail    flux.Detail
	Events    []store.Event // about the object and its sources, newest first
	Inventory inventoryView
	Missing   bool
	Actions   bool        // actions are enabled
	Logs      bool        // controller logs are offered
	Diff      bool        // a source diff is offered
	Browse    bool        // the source artifact browser is offered
	Release   bool        // the Helm release content is offered
	YAML      bool        // the object's YAML is offered
	Can       permissions // what the current user may do on this object
	OOB       bool        // render the header as an out-of-band swap
}

// permissions are the actions offered to the current user in the drawer.
type permissions struct {
	Reconcile bool
	Suspend   bool
	Inspect   bool // read the Helm release's values and manifest
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d := drawer{Kind: k, Namespace: r.PathValue("namespace"), Name: r.PathValue("name"), Actions: s.opts.Actions, Logs: s.opts.Logs,
		Diff: s.opts.Diff && k.ID == "kustomizations", YAML: s.opts.YAMLView}
	// ?part=body is the live refresh of an open drawer: its body, plus the
	// header out of band.
	tmpl := "drawer"
	if r.URL.Query().Get("part") == "body" {
		tmpl, d.OOB = "drawer-refresh", true
	}

	st := s.backend.State(k.ID)
	if !st.Installed || !st.Synced {
		d.Missing = true
		s.render(w, http.StatusNotFound, tmpl, d)
		return
	}
	detail, err := s.backend.Get(r.Context(), k, d.Namespace, d.Name)
	switch {
	case apierrors.IsNotFound(err):
		d.Missing = true
		s.render(w, http.StatusNotFound, tmpl, d)
	case err != nil:
		s.log.Error("getting object", "kind", k.ID, "namespace", d.Namespace, "name", d.Name, "err", err)
		http.Error(w, "failed to get object", http.StatusInternalServerError)
	default:
		if err := s.backend.ResolveDependencies(r.Context(), k, &detail); err != nil {
			s.log.Warn("resolving dependencies", "kind", k.ID, "namespace", d.Namespace, "name", d.Name, "err", err)
		}
		d.Detail = detail
		d.Events = s.backend.Events(r.Context(), s.backend.Related(r.Context(), k, d.Namespace, d.Name)...)
		d.Browse = s.opts.ArtifactBrowser && detail.HasArtifact
		if detail.HasInventory && !d.OOB {
			d.Inventory = newInventoryView(objectPath(k, d.Namespace, d.Name), detail, s.opts.ManagedStatus, nil)
		}
		ref := store.ObjectRef{Kind: k.GVK.Kind, Namespace: d.Namespace, Name: d.Name}
		if d.Actions {
			d.Can = s.permissions(r, ref)
		}
		if d.Release = s.opts.ReleaseContent && k.ID == "helmreleases"; d.Release && !d.OOB {
			d.Can.Inspect = s.allowed(r, authz.VerbInspect, ref)
		}
		s.render(w, http.StatusOK, tmpl, d)
	}
}

// permissions asks the authorizer, with caching, what the request's user may
// do on ref. It only decides which buttons to show: actions are re-checked.
func (s *Server) permissions(r *http.Request, ref store.ObjectRef) permissions {
	return permissions{Reconcile: s.allowed(r, authz.VerbReconcile, ref), Suspend: s.allowed(r, authz.VerbSuspend, ref)}
}

// allowed asks the authorizer, with caching, whether the request's user may
// apply verb to ref. A user header that is configured but missing denies.
func (s *Server) allowed(r *http.Request, verb string, ref store.ObjectRef) bool {
	id := s.identity(r)
	if s.opts.UserHeader != "" && id.User == "" {
		return false
	}
	ok, err := s.authz.Allowed(r.Context(), id, verb, ref, false)
	if err != nil {
		s.log.Warn("checking permissions", "verb", verb, "object", ref.String(), "user", id.User, "err", err)
	}
	return ok && err == nil
}

type toast struct {
	Message string
	Error   bool
}

// handleAction runs suspend, resume, reconcile or reconcile-with-source on an
// object. It answers with a toast; the object itself refreshes over SSE.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Actions {
		s.render(w, http.StatusForbidden, "toast", toast{Error: true, Message: "Actions are disabled (start fluxcd-ui with --enable-actions)."})
		return
	}
	id := s.identity(r)
	if s.opts.UserHeader != "" && id.User == "" {
		s.render(w, http.StatusUnauthorized, "toast", toast{Error: true, Message: "Not authenticated: the " + s.opts.UserHeader + " header is missing."})
		return
	}
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ref := store.ObjectRef{Kind: k.GVK.Kind, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	action := r.PathValue("action")
	ctx := r.Context()

	var (
		msg string
		err error
	)
	switch action {
	case "suspend", "resume":
		suspend := action == "suspend"
		if err = s.authorize(ctx, id, authz.VerbSuspend, ref); err == nil {
			err = s.backend.SetSuspended(ctx, k, ref.Namespace, ref.Name, suspend)
		}
		if suspend {
			msg = "Suspended " + ref.String() + "."
		} else {
			msg = "Resumed " + ref.String() + "."
		}
	case "reconcile", "reconcile-with-source":
		var refs []store.ObjectRef
		refs, err = s.backend.ReconcilePlan(ctx, k, ref.Namespace, ref.Name, action == "reconcile-with-source")
		for _, r := range refs {
			if err == nil && !r.Implied {
				err = s.authorize(ctx, id, authz.VerbReconcile, r)
			}
		}
		if err == nil {
			err = s.backend.RequestReconcile(ctx, refs)
		}
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = r.String()
		}
		msg = "Reconciliation requested: " + strings.Join(names, " → ") + "."
	default:
		http.NotFound(w, r)
		return
	}

	log := s.log.With("action", action, "kind", ref.Kind, "namespace", ref.Namespace, "name", ref.Name, "user", id.User, "groups", id.Groups)
	if err != nil {
		log.Warn("action failed", "err", err)
		s.render(w, actionErrorStatus(err), "toast", toast{Error: true, Message: "Could not " + strings.ReplaceAll(action, "-", " ") + " " + ref.String() + ": " + err.Error()})
		return
	}
	log.Info("action performed")
	s.render(w, http.StatusOK, "toast", toast{Message: msg})
}

// deniedError reports an action the authorizer refused.
type deniedError struct {
	user, verb string
	ref        store.ObjectRef
}

func (e deniedError) Error() string {
	return fmt.Sprintf("%s is not allowed to %s %s (needs the %q verb on %s)", cmp.Or(e.user, "anonymous"), e.verb, e.ref, e.verb, e.ref.GroupResource())
}

// authorize checks, bypassing any cache, that id may apply verb to ref.
func (s *Server) authorize(ctx context.Context, id authz.Identity, verb string, ref store.ObjectRef) error {
	ok, err := s.authz.Allowed(ctx, id, verb, ref, true)
	switch {
	case err != nil:
		return fmt.Errorf("checking permissions: %w", err)
	case !ok:
		return deniedError{user: id.User, verb: verb, ref: ref}
	}
	return nil
}

func actionErrorStatus(err error) int {
	switch {
	case errors.As(err, &deniedError{}), apierrors.IsForbidden(err):
		return http.StatusForbidden
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case errors.Is(err, store.ErrSuspended), errors.Is(err, store.ErrNotReconciled), apierrors.IsConflict(err):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// identity returns the user and groups set by the authenticating proxy.
func (s *Server) identity(r *http.Request) authz.Identity {
	id := authz.Identity{User: s.user(r)}
	if s.opts.GroupsHeader == "" {
		return id
	}
	sep := cmp.Or(s.opts.GroupsSeparator, ",")
	for _, v := range r.Header.Values(s.opts.GroupsHeader) {
		for g := range strings.SplitSeq(v, sep) {
			if g = strings.TrimSpace(g); g != "" {
				id.Groups = append(id.Groups, g)
			}
		}
	}
	return id
}

// user returns the authenticated user name set by the proxy, if configured.
func (s *Server) user(r *http.Request) string {
	if s.opts.UserHeader == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(s.opts.UserHeader))
}

type logsView struct {
	Kind  flux.Kind
	Lines []store.LogLine
	Err   error
	Now   time.Time
}

// handleLogs serves the controller log lines about an object, on demand:
// reading them means fetching the controller pods' recent logs.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok || !s.opts.Logs {
		http.NotFound(w, r)
		return
	}
	v := logsView{Kind: k, Now: time.Now()}
	v.Lines, v.Err = s.backend.ControllerLogs(r.Context(), k, r.PathValue("namespace"), r.PathValue("name"))
	if v.Err != nil {
		s.log.Warn("reading controller logs", "kind", k.ID, "namespace", r.PathValue("namespace"), "name", r.PathValue("name"), "err", v.Err)
	}
	s.render(w, http.StatusOK, "logs", v)
}

// handleInventory serves the managed objects section with each object's
// health, which the drawer loads once it is open.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok || !s.opts.ManagedStatus {
		http.NotFound(w, r)
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	detail, err := s.backend.Get(r.Context(), k, namespace, name)
	switch {
	case apierrors.IsNotFound(err):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("getting object", "kind", k.ID, "namespace", namespace, "name", name, "err", err)
		http.Error(w, "failed to get object", http.StatusInternalServerError)
		return
	}
	var entries []flux.InventoryEntry
	for _, g := range detail.Inventory {
		entries = append(entries, g.Entries...)
	}
	health := s.backend.ManagedHealth(r.Context(), entries)
	s.render(w, http.StatusOK, "inventory", newInventoryView(objectPath(k, namespace, name), detail, true, health))
}

type diffView struct {
	Result *diff.Result
	Err    error
}

// handleDiff serves the diff of a Kustomization's source with the cluster,
// on demand: it downloads and builds the source.
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Diff {
		http.NotFound(w, r)
		return
	}
	var v diffView
	v.Result, v.Err = s.backend.DiffKustomization(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if v.Err != nil {
		s.log.Warn("diffing kustomization", "namespace", r.PathValue("namespace"), "name", r.PathValue("name"), "err", v.Err)
	}
	s.render(w, http.StatusOK, "diff", v)
}

type artifactView struct {
	Artifact *meta.Artifact
	Tree     *treeNode
	Count    int
	Total    int64
	Err      error
}

// handleArtifact serves the file tree of a source's artifact.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok || !s.opts.ArtifactBrowser {
		http.NotFound(w, r)
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	var v artifactView
	var files []artifact.File
	v.Artifact, files, v.Err = s.backend.ArtifactFiles(r.Context(), k, namespace, name)
	if v.Err != nil {
		s.log.Warn("listing artifact files", "kind", k.ID, "namespace", namespace, "name", name, "err", v.Err)
	} else {
		v.Tree, v.Count, v.Total = buildTree(objectPath(k, namespace, name)+"/artifact/file", files)
	}
	s.render(w, http.StatusOK, "artifact", v)
}

type artifactFileView struct {
	Content *artifact.Content
	Err     error
}

// handleArtifactFile serves one file of a source's artifact.
func (s *Server) handleArtifactFile(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok || !s.opts.ArtifactBrowser {
		http.NotFound(w, r)
		return
	}
	var v artifactFileView
	v.Content, v.Err = s.backend.ArtifactFile(r.Context(), k, r.PathValue("namespace"), r.PathValue("name"), r.URL.Query().Get("path"))
	status := http.StatusOK
	if errors.Is(v.Err, artifact.ErrNotFound) {
		status = http.StatusNotFound
	}
	s.render(w, status, "artifact-file", v)
}

type yamlView struct {
	YAML string
	Err  error
}

// handleYAML serves an object as YAML.
func (s *Server) handleYAML(w http.ResponseWriter, r *http.Request) {
	k, ok := flux.KindByID(r.PathValue("kind"))
	if !ok || !s.opts.YAMLView {
		http.NotFound(w, r)
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	var v yamlView
	v.YAML, v.Err = s.backend.ObjectYAML(r.Context(), k, namespace, name)
	status := http.StatusOK
	switch {
	case apierrors.IsNotFound(v.Err):
		status = http.StatusNotFound
	case v.Err != nil:
		s.log.Warn("encoding object", "kind", k.ID, "namespace", namespace, "name", name, "err", v.Err)
	}
	s.render(w, status, "yaml", v)
}

// maxReleaseView caps the values and the manifest shown. Charts templating
// their CRDs render manifests of a few MiB.
const maxReleaseView = 4 << 20

type releaseView struct {
	Content           *store.ReleaseContent
	Values            string
	Manifest          string
	ValuesTruncated   bool
	ManifestTruncated bool
	ManifestSize      int64 // before truncation
	Err               error
}

// handleRelease serves the values and manifest of a HelmRelease's current
// Helm release. Values may hold credentials, so every request is authorized.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if !s.opts.ReleaseContent {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // neither the browser nor a proxy may keep the values
	ctx := r.Context()
	id := s.identity(r)
	ref := store.ObjectRef{Kind: helmv2.HelmReleaseKind, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	log := s.log.With("namespace", ref.Namespace, "name", ref.Name, "user", id.User)
	var v releaseView

	if s.opts.UserHeader != "" && id.User == "" {
		v.Err = errors.New("not authenticated: the " + s.opts.UserHeader + " header is missing")
		s.render(w, http.StatusUnauthorized, "release", v)
		return
	}
	if err := s.authorize(ctx, id, authz.VerbInspect, ref); err != nil {
		log.Warn("reading Helm release refused", "err", err)
		v.Err = err
		s.render(w, actionErrorStatus(err), "release", v)
		return
	}
	v.Content, v.Err = s.backend.HelmReleaseContent(ctx, ref.Namespace, ref.Name)
	if v.Err != nil {
		if !errors.Is(v.Err, store.ErrNoRelease) {
			log.Warn("reading Helm release", "err", v.Err)
		}
		s.render(w, http.StatusOK, "release", v)
		return
	}
	log.Info("Helm release read") // values may hold credentials: keep track of who read them
	v.Values, v.ValuesTruncated = truncate(v.Content.Values, maxReleaseView)
	v.Manifest, v.ManifestTruncated = truncate(v.Content.Manifest, maxReleaseView)
	v.ManifestSize = int64(len(v.Content.Manifest))
	s.render(w, http.StatusOK, "release", v)
}

// truncate cuts s to at most n bytes, on a UTF-8 boundary.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func objectPath(k flux.Kind, namespace, name string) string {
	return "/objects/" + k.ID + "/" + namespace + "/" + name
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.backend.Ready() {
		http.Error(w, "caches not synced", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// staticHandler serves embedded assets. Templates reference them with
// ?v=<version>, so released builds can cache them forever.
func (s *Server) staticHandler(static fs.FS) http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Version != "dev" && r.URL.Query().Get("v") == s.opts.Version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// render executes a template into a buffer first, so that a template error
// yields a clean 500 rather than a truncated page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("rendering template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
