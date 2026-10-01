package web

import (
	"fmt"
	"html/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mycroft/fluxcd-ui/internal/flux"
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
		"showMessage": func(s flux.Status) bool {
			return s.Message != "" && (s.State == flux.StateFailed || s.State == flux.StateProgressing || s.State == flux.StateUnknown)
		},
	}
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
