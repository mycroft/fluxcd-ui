package web

import (
	"fmt"
	"html/template"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mycroft/fluxcd-ui/internal/diff"
	"github.com/mycroft/fluxcd-ui/internal/flux"
	"github.com/mycroft/fluxcd-ui/internal/store"
)

// Class names below are picked up by Tailwind (see styles/app.css @source).

var pillClasses = map[flux.State]string{
	flux.StateReady:       "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-400 dark:ring-emerald-500/20",
	flux.StateFailed:      "bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-500/10 dark:text-red-400 dark:ring-red-500/20",
	flux.StateProgressing: "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-400 dark:ring-amber-500/20",
	flux.StateSuspended:   "bg-slate-100 text-slate-600 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-300 dark:ring-slate-400/20",
	flux.StateStatic:      "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-400 dark:ring-sky-500/20",
	flux.StateUnknown:     "bg-zinc-50 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/20",
}

var dotClasses = map[flux.State]string{
	flux.StateReady:       "bg-emerald-500",
	flux.StateFailed:      "bg-red-500",
	flux.StateProgressing: "bg-amber-500",
	flux.StateSuspended:   "bg-slate-400",
	flux.StateStatic:      "bg-sky-500",
	flux.StateUnknown:     "bg-zinc-400",
}

var messageClasses = map[flux.State]string{
	flux.StateFailed:      "text-red-600 dark:text-red-400",
	flux.StateProgressing: "text-amber-700 dark:text-amber-400",
}

var calloutClasses = map[flux.State]string{
	flux.StateReady:       "border-emerald-200 bg-emerald-50 text-emerald-900 dark:border-emerald-500/20 dark:bg-emerald-500/10 dark:text-emerald-200",
	flux.StateFailed:      "border-red-200 bg-red-50 text-red-900 dark:border-red-500/20 dark:bg-red-500/10 dark:text-red-200",
	flux.StateProgressing: "border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-500/20 dark:bg-amber-500/10 dark:text-amber-200",
}

const defaultCalloutClass = "border-slate-200 bg-slate-50 text-slate-700 dark:border-slate-700 dark:bg-slate-800/50 dark:text-slate-300"

var diffClasses = map[diff.Action]string{
	diff.ActionChanged:   "bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-500/10 dark:text-amber-400 dark:ring-amber-500/20",
	diff.ActionCreated:   "bg-sky-50 text-sky-700 ring-sky-600/20 dark:bg-sky-500/10 dark:text-sky-400 dark:ring-sky-500/20",
	diff.ActionPruned:    "bg-red-50 text-red-700 ring-red-600/20 dark:bg-red-500/10 dark:text-red-400 dark:ring-red-500/20",
	diff.ActionOrphaned:  "bg-slate-100 text-slate-600 ring-slate-500/20 dark:bg-slate-500/10 dark:text-slate-300 dark:ring-slate-400/20",
	diff.ActionSkipped:   "bg-zinc-50 text-zinc-600 ring-zinc-500/20 dark:bg-zinc-500/10 dark:text-zinc-400 dark:ring-zinc-400/20",
	diff.ActionUnchanged: "bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-500/10 dark:text-emerald-400 dark:ring-emerald-500/20",
}

// diffActionOrder is the order of the diff summary.
var diffActionOrder = []diff.Action{
	diff.ActionChanged, diff.ActionCreated, diff.ActionPruned, diff.ActionOrphaned, diff.ActionSkipped, diff.ActionUnchanged,
}

var healthDotClasses = map[store.Health]string{
	store.HealthReady:       "bg-emerald-500",
	store.HealthProgressing: "bg-amber-500",
	store.HealthTerminating: "bg-amber-500",
	store.HealthFailed:      "bg-red-500",
	store.HealthMissing:     "bg-red-500",
	store.HealthSuspended:   "bg-slate-400",
	store.HealthUnknown:     "bg-zinc-300 dark:bg-zinc-600",
	store.HealthNoAccess:    "bg-zinc-300 dark:bg-zinc-600",
}

var healthClasses = map[store.Health]string{
	store.HealthReady:       "text-emerald-700 dark:text-emerald-400",
	store.HealthProgressing: "text-amber-700 dark:text-amber-400",
	store.HealthTerminating: "text-amber-700 dark:text-amber-400",
	store.HealthFailed:      "font-medium text-red-700 dark:text-red-400",
	store.HealthMissing:     "font-medium text-red-700 dark:text-red-400",
	store.HealthSuspended:   "text-slate-500",
	store.HealthUnknown:     "text-zinc-500",
	store.HealthNoAccess:    "text-zinc-500",
}

var conditionClasses = map[metav1.ConditionStatus]string{
	metav1.ConditionTrue:    "text-emerald-700 dark:text-emerald-400",
	metav1.ConditionFalse:   "text-red-700 dark:text-red-400",
	metav1.ConditionUnknown: "text-amber-700 dark:text-amber-400",
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"shortRev":       flux.ShortRevision,
		"ago":            ago,
		"timestamp":      timestamp,
		"datetime":       datetime,
		"pillClass":      func(s flux.State) string { return pillClasses[s] },
		"dotClass":       func(s flux.State) string { return dotClasses[s] },
		"messageClass":   func(s flux.State) string { return messageClasses[s] },
		"conditionClass": func(s metav1.ConditionStatus) string { return conditionClasses[s] },
		"calloutClass": func(s flux.State) string {
			if c, ok := calloutClasses[s]; ok {
				return c
			}
			return defaultCalloutClass
		},
		// showMessage reports whether a row should surface its status message.
		"clock":          func(t time.Time) string { return t.UTC().Format("01-02 15:04:05") },
		"displayVersion": displayVersion,
		"logLevelClass": func(level string) string {
			switch level {
			case "error", "dpanic", "panic", "fatal":
				return "font-semibold text-red-600 dark:text-red-400"
			case "warn":
				return "text-amber-700 dark:text-amber-400"
			case "debug":
				return "text-slate-400"
			default:
				return "text-sky-700 dark:text-sky-400"
			}
		},
		"healthDot":   func(h store.Health) string { return healthDotClasses[h] },
		"diffClass":   func(a diff.Action) string { return diffClasses[a] },
		"diffActions": func() []diff.Action { return diffActionOrder },
		"healthClass": func(h store.Health) string { return healthClasses[h] },
		"releaseStatusClass": func(status string) string {
			switch status {
			case "deployed":
				return "font-medium text-emerald-700 dark:text-emerald-400"
			case "failed":
				return "font-medium text-red-700 dark:text-red-400"
			case "pending-install", "pending-upgrade", "pending-rollback", "uninstalling":
				return "font-medium text-amber-700 dark:text-amber-400"
			default: // superseded, uninstalled, unknown
				return "text-slate-500"
			}
		},
		"eventClass": func(typ string) string {
			if typ == corev1.EventTypeWarning {
				return "bg-red-50 text-red-700 dark:bg-red-500/10 dark:text-red-400"
			}
			return "bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300"
		},
		"showMessage": func(s flux.Status) bool {
			return s.Message != "" && (s.State == flux.StateFailed || s.State == flux.StateProgressing || s.State == flux.StateUnknown)
		},
	}
}

// releaseVersion matches the versions of release builds, e.g. "0.2.0".
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+`)

// displayVersion shows release versions as "v0.2.0"; other builds keep their
// version as is: a commit for builds from main, "dev" for local ones.
func displayVersion(v string) string {
	if releaseVersion.MatchString(v) {
		return "v" + v
	}
	return v
}

var now = time.Now

// ago formats the time elapsed since t compactly: "42s", "5m", "3h", "12d".
// app.js mirrors this format.
func ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// datetime formats t for <time datetime>, which app.js uses to keep
// relative times current.
func datetime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
