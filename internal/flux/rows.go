package flux

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const staticHelmRepositoryMessage = "OCI HelmRepositories are not reconciled: charts are pulled directly by HelmCharts and HelmReleases."

func describeGitRepository(o *sourcev1.GitRepository) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	ref := gitRef(o.Spec.Reference)
	d.Cells = []Cell{{Text: o.Spec.URL}, {Text: ref}}
	d.Revision = artifactRevision(o.Status.Artifact)
	d.HasArtifact = o.Status.Artifact != nil

	var f fields
	f.add("URL", o.Spec.URL)
	f.add("Ref", ref)
	f.addArtifact(o.Status.Artifact)
	f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	d.Fields = f
	return d
}

func describeOCIRepository(o *sourcev1.OCIRepository) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	ref := ociRef(o.Spec.Reference)
	d.Cells = []Cell{{Text: o.Spec.URL}, {Text: ref}}
	d.Revision = artifactRevision(o.Status.Artifact)
	d.HasArtifact = o.Status.Artifact != nil

	var f fields
	f.add("URL", o.Spec.URL)
	f.add("Ref", ref)
	f.add("Provider", o.Spec.Provider)
	f.addArtifact(o.Status.Artifact)
	f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	d.Fields = f
	return d
}

func describeBucket(o *sourcev1.Bucket) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	provider := cmp.Or(o.Spec.Provider, sourcev1.BucketProviderGeneric)
	bucket := o.Spec.BucketName
	if o.Spec.Prefix != "" {
		bucket += "/" + strings.TrimPrefix(o.Spec.Prefix, "/")
	}
	d.Cells = []Cell{{Text: o.Spec.Endpoint}, {Text: bucket}, {Text: provider}}
	d.Revision = artifactRevision(o.Status.Artifact)
	d.HasArtifact = o.Status.Artifact != nil

	var f fields
	f.add("Endpoint", o.Spec.Endpoint)
	f.add("Bucket", o.Spec.BucketName)
	f.addMono("Prefix", o.Spec.Prefix)
	f.add("Provider", provider)
	f.add("Region", o.Spec.Region)
	if o.Spec.Insecure {
		f.add("Insecure", "true (plain HTTP)")
	}
	if sts := o.Spec.STS; sts != nil {
		f.add("STS", sts.Provider+" "+sts.Endpoint)
	}
	f.addArtifact(o.Status.Artifact)
	f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	d.Fields = f
	return d
}

func describeHelmRepository(o *sourcev1.HelmRepository) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	typ := o.Spec.Type
	if typ == "" {
		typ = sourcev1.HelmRepositoryTypeDefault
	}
	if typ == sourcev1.HelmRepositoryTypeOCI && !o.Spec.Suspend && len(o.Status.Conditions) == 0 {
		d.Status = Status{State: StateStatic, Message: staticHelmRepositoryMessage}
	}
	d.Cells = []Cell{{Text: o.Spec.URL}, {Text: typ}}
	d.Revision = artifactRevision(o.Status.Artifact)
	d.HasArtifact = o.Status.Artifact != nil

	var f fields
	f.add("URL", o.Spec.URL)
	f.add("Type", typ)
	f.add("Provider", o.Spec.Provider)
	f.addArtifact(o.Status.Artifact)
	if typ != sourcev1.HelmRepositoryTypeOCI {
		f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	}
	d.Fields = f
	return d
}

func describeHelmChart(o *sourcev1.HelmChart) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	version := o.Spec.Version
	if version == "" {
		version = "*"
	}
	source := sourceRef(o.Spec.SourceRef.Kind, o.Spec.SourceRef.Name, "", o.Namespace)
	d.Cells = []Cell{{Text: o.Spec.Chart}, {Text: version}, {Text: source}}
	d.Revision = artifactRevision(o.Status.Artifact)
	d.HasArtifact = o.Status.Artifact != nil
	d.HasSource = true

	var f fields
	f.add("Chart", o.Spec.Chart)
	f.add("Version", version)
	f.add("Source", source)
	f.add("Reconcile strategy", o.Spec.ReconcileStrategy)
	f.add("Values files", strings.Join(o.Spec.ValuesFiles, ", "))
	f.addArtifact(o.Status.Artifact)
	f.addSchedule(o.Spec.Interval, nil)
	d.Fields = f
	return d
}

func describeHelmRelease(o *helmv2.HelmRelease) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)

	var chart, version, source string
	switch {
	case o.Spec.ChartRef != nil:
		r := o.Spec.ChartRef
		chart = r.Name
		source = sourceRef(r.Kind, r.Name, r.Namespace, o.Namespace)
	case o.Spec.Chart != nil:
		s := o.Spec.Chart.Spec
		chart, version = s.Chart, s.Version
		source = sourceRef(s.SourceRef.Kind, s.SourceRef.Name, s.SourceRef.Namespace, o.Namespace)
	}

	latest := LatestSnapshot(o.Status.History)
	d.Revision = o.Status.LastAttemptedRevision
	var appVersion string
	if latest != nil {
		if latest.ChartName != "" {
			chart = latest.ChartName
		}
		d.Revision = latest.ChartVersion
		appVersion = latest.AppVersion
	}
	d.Cells = []Cell{{Text: chart}, {Text: source}, {Text: appVersion, Mono: true}}
	d.HasSource = source != ""

	var f fields
	f.add("Chart", chart)
	f.add("Version constraint", version)
	f.add("Source", source)
	f.add("Release", o.GetReleaseNamespace()+"/"+o.GetReleaseName())
	if latest != nil {
		f.addMono("Deployed chart", latest.ChartVersion)
		f.addMono("App version", latest.AppVersion)
		f.add("Release status", latest.Status+" (revision "+strconv.Itoa(latest.Version)+")")
		f.addTime("Last deployed", latest.LastDeployed.Time)
	}
	f.addMono("Last attempted revision", o.Status.LastAttemptedRevision)
	if o.Status.Failures > 0 {
		f.add("Failures", strconv.FormatInt(o.Status.Failures, 10))
	}
	f.add("Depends on", dependsOn(o.Spec.DependsOn, o.Namespace))
	f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	d.Fields = f

	d.HasInventory = true
	if inv := o.Status.Inventory; inv != nil {
		refs := make([]inventoryRef, len(inv.Entries))
		for i, e := range inv.Entries {
			refs[i] = inventoryRef{id: e.ID, version: e.Version}
		}
		d.Inventory = groupInventory(refs)
	}
	d.History = releaseHistory(o.Status.History)
	return d
}

// releaseHistory returns the release revisions, newest first, without
// reordering the (shared, cached) history itself.
func releaseHistory(history helmv2.Snapshots) []Release {
	releases := make([]Release, 0, len(history))
	for _, s := range history {
		if s == nil {
			continue
		}
		releases = append(releases, Release{
			Revision:   s.Version,
			Chart:      s.ChartName + "@" + s.ChartVersion,
			AppVersion: s.AppVersion,
			Status:     s.Status,
			Action:     string(s.Action),
			Deployed:   s.LastDeployed.Time,
		})
	}
	slices.SortFunc(releases, func(a, b Release) int { return cmp.Compare(b.Revision, a.Revision) })
	return releases
}

func describeKustomization(o *kustomizev1.Kustomization) Detail {
	d := newDetail(o, o.Spec.Suspend, o.Status.ObservedGeneration, o.Status.Conditions, o.Status.LastHandledReconcileAt)
	source := sourceRef(o.Spec.SourceRef.Kind, o.Spec.SourceRef.Name, o.Spec.SourceRef.Namespace, o.Namespace)
	path := o.Spec.Path
	if path == "" {
		path = "./"
	}
	d.Cells = []Cell{{Text: source}, {Text: path, Mono: true}}
	if ref := o.Spec.SourceRef; ref.Name != "" {
		d.Cells[0].Source = &SourceStatus{Kind: ref.Kind, Namespace: cmp.Or(ref.Namespace, o.Namespace), Name: ref.Name}
	}
	d.Revision = o.Status.LastAppliedRevision
	d.HasSource = true

	var f fields
	f.add("Source", source)
	f.addMono("Path", path)
	f.add("Target namespace", o.Spec.TargetNamespace)
	f.add("Prune", strconv.FormatBool(o.Spec.Prune))
	f.addMono("Last applied revision", o.Status.LastAppliedRevision)
	f.addMono("Last attempted revision", o.Status.LastAttemptedRevision)
	f.add("Depends on", dependsOn(o.Spec.DependsOn, o.Namespace))
	f.addSchedule(o.Spec.Interval, o.Spec.Timeout)
	d.Fields = f

	d.HasInventory = true
	if inv := o.Status.Inventory; inv != nil {
		refs := make([]inventoryRef, len(inv.Entries))
		for i, e := range inv.Entries {
			refs[i] = inventoryRef{id: e.ID, version: e.Version}
		}
		d.Inventory = groupInventory(refs)
	}
	return d
}

func newDetail(obj client.Object, suspended bool, observedGeneration int64, conds []metav1.Condition, lastHandledReconcileAt string) Detail {
	updated := obj.GetCreationTimestamp().Time
	if ready := apimeta.FindStatusCondition(conds, meta.ReadyCondition); ready != nil {
		updated = ready.LastTransitionTime.Time
	}

	// The controller copies the request token to status once it handled it.
	// Suspended objects are never reconciled, so they are never pending.
	requested := obj.GetAnnotations()[meta.ReconcileRequestAnnotation]
	var requestedAt time.Time
	if t, err := time.Parse(time.RFC3339Nano, requested); err == nil {
		requestedAt = t
	}

	return Detail{
		Row: Row{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
			Status:    computeStatus(suspended, obj.GetGeneration(), observedGeneration, conds),
			Updated:   updated,
			Pending:   requested != "" && requested != lastHandledReconcileAt && !suspended,

			ReconcileRequestedAt: requestedAt,
		},
		Conditions:         conds,
		Generation:         obj.GetGeneration(),
		ObservedGeneration: observedGeneration,
	}
}

// LatestSnapshot returns the release snapshot with the highest version.
// Snapshots.Latest sorts in place, which would mutate the shared cached object.
func LatestSnapshot(history helmv2.Snapshots) *helmv2.Snapshot {
	var latest *helmv2.Snapshot
	for _, s := range history {
		if s != nil && (latest == nil || s.Version > latest.Version) {
			latest = s
		}
	}
	return latest
}

func artifactRevision(a *meta.Artifact) string {
	if a == nil {
		return ""
	}
	return a.Revision
}

func gitRef(ref *sourcev1.GitRepositoryRef) string {
	switch {
	case ref == nil:
		return ""
	case ref.Commit != "":
		return "commit " + shortHex(ref.Commit)
	case ref.Name != "":
		return ref.Name
	case ref.SemVer != "":
		return "semver " + ref.SemVer
	case ref.Tag != "":
		return "tag " + ref.Tag
	default:
		return ref.Branch
	}
}

func ociRef(ref *sourcev1.OCIRepositoryRef) string {
	switch {
	case ref == nil:
		return "latest"
	case ref.Digest != "":
		return ShortRevision(ref.Digest)
	case ref.SemVer != "":
		return "semver " + ref.SemVer
	case ref.Tag != "":
		return ref.Tag
	default:
		return "latest"
	}
}

// sourceRef formats a reference as Kind/name, or Kind/namespace/name when it
// points outside the referencing object's namespace.
func sourceRef(kind, name, namespace, objNamespace string) string {
	if name == "" {
		return ""
	}
	if namespace != "" && namespace != objNamespace {
		return kind + "/" + namespace + "/" + name
	}
	return kind + "/" + name
}

func dependsOn(deps []meta.DependencyReference, objNamespace string) string {
	names := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep.Namespace != "" && dep.Namespace != objNamespace {
			names = append(names, dep.Namespace+"/"+dep.Name)
		} else {
			names = append(names, dep.Name)
		}
	}
	return strings.Join(names, ", ")
}

// fields accumulates detail fields, skipping empty values.
type fields []Field

func (f *fields) add(label, value string) {
	if value != "" {
		*f = append(*f, Field{Label: label, Value: value})
	}
}

func (f *fields) addMono(label, value string) {
	if value != "" {
		*f = append(*f, Field{Label: label, Value: value, Mono: true})
	}
}

func (f *fields) addTime(label string, t time.Time) {
	if !t.IsZero() {
		*f = append(*f, Field{Label: label, Time: t})
	}
}

func (f *fields) addArtifact(a *meta.Artifact) {
	if a == nil {
		return
	}
	f.addMono("Revision", a.Revision)
	f.addMono("Digest", a.Digest)
	f.addTime("Artifact updated", a.LastUpdateTime.Time)
}

func (f *fields) addSchedule(interval metav1.Duration, timeout *metav1.Duration) {
	if interval.Duration > 0 {
		f.add("Interval", formatDuration(interval.Duration))
	}
	if timeout != nil && timeout.Duration > 0 {
		f.add("Timeout", formatDuration(timeout.Duration))
	}
}

// formatDuration renders durations the way they are usually written in
// manifests: "1m" rather than "1m0s", "1h" rather than "1h0m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
