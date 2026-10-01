package diff

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// maxValueLen truncates the values shown in a change.
const maxValueLen = 200

// notSet is shown for a field the live object lacks.
const notSet = "(not set)"

// Change is one field whose live value differs from the source.
type Change struct {
	Path    string
	Live    string
	Desired string
}

// compareObjects reports the fields set by desired whose value in live
// differs. Fields only set in live (defaults, controller-managed fields) are
// not compared: this is what applying desired would change.
func compareObjects(desired, live map[string]any) []Change {
	var changes []Change
	for _, k := range sortedKeys(desired) {
		switch k {
		case "status", "apiVersion", "kind":
			continue
		case "metadata":
			dm, _ := desired[k].(map[string]any)
			lm, _ := live[k].(map[string]any)
			for _, mk := range sortedKeys(dm) {
				// Identity and server-managed fields are not something to diff.
				if mk == "name" || mk == "namespace" || mk == "creationTimestamp" {
					continue
				}
				walk(&changes, joinPath("metadata", mk), dm[mk], lm[mk], hasKey(lm, mk))
			}
		default:
			walk(&changes, k, desired[k], live[k], hasKey(live, k))
		}
	}
	return changes
}

func walk(changes *[]Change, path string, desired, live any, present bool) {
	// A map the live object lacks entirely: report each field it sets.
	if d, ok := desired.(map[string]any); ok && (!present || live == nil) {
		for _, k := range sortedKeys(d) {
			walk(changes, joinPath(path, k), d[k], nil, false)
		}
		return
	}
	if !present {
		if !isEmpty(desired) {
			*changes = append(*changes, Change{Path: path, Live: notSet, Desired: format(desired)})
		}
		return
	}
	switch d := desired.(type) {
	case map[string]any:
		l, ok := live.(map[string]any)
		if !ok {
			*changes = append(*changes, Change{Path: path, Live: format(live), Desired: format(desired)})
			return
		}
		for _, k := range sortedKeys(d) {
			walk(changes, joinPath(path, k), d[k], l[k], hasKey(l, k))
		}
	case []any:
		l, ok := live.([]any)
		if !ok {
			*changes = append(*changes, Change{Path: path, Live: format(live), Desired: format(desired)})
			return
		}
		walkList(changes, path, d, l)
	default:
		if !scalarEqual(desired, live) {
			*changes = append(*changes, Change{Path: path, Live: format(live), Desired: format(desired)})
		}
	}
}

// walkList matches list items by name when every item has one (containers,
// env vars, ports, volumes...), as server-side apply merges such lists, and
// by position otherwise.
func walkList(changes *[]Change, path string, desired, live []any) {
	if names, ok := itemNames(desired); ok {
		if liveNames, ok := itemNames(live); ok {
			byName := map[string]any{}
			for i, n := range liveNames {
				byName[n] = live[i]
			}
			for i, n := range names {
				itemPath := fmt.Sprintf("%s[name=%s]", path, n)
				l, found := byName[n]
				if !found { // a whole item to add, shown as one change
					*changes = append(*changes, Change{Path: itemPath, Live: notSet, Desired: format(desired[i])})
					continue
				}
				walk(changes, itemPath, desired[i], l, true)
			}
			return
		}
	}
	if len(desired) != len(live) {
		*changes = append(*changes, Change{Path: path, Live: format(live), Desired: format(desired)})
		return
	}
	for i := range desired {
		walk(changes, fmt.Sprintf("%s[%d]", path, i), desired[i], live[i], true)
	}
}

func itemNames(items []any) ([]string, bool) {
	if len(items) == 0 {
		return nil, false
	}
	names := make([]string, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := m["name"].(string)
		if !ok || name == "" {
			return nil, false
		}
		names[i] = name
	}
	return names, true
}

// scalarEqual compares values as the API server stores them: numbers by
// value, and quantities by amount ("500m" equals "0.5").
func scalarEqual(desired, live any) bool {
	if dn, ok := number(desired); ok {
		ln, ok := number(live)
		return ok && dn == ln
	}
	ds, ok1 := desired.(string)
	ls, ok2 := live.(string)
	if ok1 && ok2 {
		if ds == ls {
			return true
		}
		dq, err1 := resource.ParseQuantity(ds)
		lq, err2 := resource.ParseQuantity(ls)
		return err1 == nil && err2 == nil && dq.Cmp(lq) == 0
	}
	return fmt.Sprint(desired) == fmt.Sprint(live) && fmt.Sprintf("%T", desired) == fmt.Sprintf("%T", live)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	default:
		return false
	}
}

// format renders a value compactly for display.
func format(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		s = "null"
	case string:
		s = x
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			s = fmt.Sprint(x)
		} else {
			s = string(b)
		}
	default:
		s = fmt.Sprint(x)
	}
	if len(s) > maxValueLen {
		s = s[:maxValueLen] + "…"
	}
	return s
}

// joinPath appends a key, quoting keys that would read ambiguously, such as
// annotation and label names.
func joinPath(path, key string) string {
	if strings.ContainsAny(key, "./[] ") {
		return path + "[" + strconv.Quote(key) + "]"
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
