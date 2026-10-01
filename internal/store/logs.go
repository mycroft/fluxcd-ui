package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

const (
	// logTailLines is how far back each controller pod's log is read. The
	// controllers log every reconciliation, so this covers a few hours on
	// a typical cluster.
	logTailLines = 10000
	// maxLogLines caps the lines returned for one object.
	maxLogLines = 200
	// logContainer is the container running the controller in Flux pods.
	logContainer = "manager"
)

// ErrLogsUnavailable is returned when the store cannot read pod logs.
var ErrLogsUnavailable = errors.New("controller logs are not available")

// LogLine is one controller log entry about a Flux object.
type LogLine struct {
	Time    time.Time
	Level   string
	Message string
	Error   string
}

// logEntry is the part of a Flux controller's JSON log line we use.
type logEntry struct {
	Time           logTime `json:"ts"`
	Level          string  `json:"level"`
	Message        string  `json:"msg"`
	Error          string  `json:"error"`
	ControllerKind string  `json:"controllerKind"`
	Namespace      string  `json:"namespace"`
	Name           string  `json:"name"`
}

// ControllerLogs returns the recent log lines, newest first, that the
// controller reconciling objects of kind k wrote about the given object.
// Controllers are found by their "app" label in the Flux namespace, and
// every replica is read.
func (s *Store) ControllerLogs(ctx context.Context, k flux.Kind, namespace, name string) ([]LogLine, error) {
	if s.clientset == nil {
		return nil, ErrLogsUnavailable
	}
	pods, err := s.clientset.CoreV1().Pods(s.fluxNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + k.Controller})
	if err != nil {
		return nil, fmt.Errorf("listing %s pods in %s: %w", k.Controller, s.fluxNamespace, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no %s pod found in namespace %s", k.Controller, s.fluxNamespace)
	}

	var lines []LogLine
	for _, pod := range pods.Items {
		tail := int64(logTailLines)
		stream, err := s.clientset.CoreV1().Pods(pod.Namespace).
			GetLogs(pod.Name, &corev1.PodLogOptions{Container: logContainer, TailLines: &tail}).
			Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading logs of %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		found, err := filterLogs(stream, k.GVK.Kind, namespace, name)
		_ = stream.Close()
		if err != nil {
			return nil, fmt.Errorf("reading logs of %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		lines = append(lines, found...)
	}

	slices.SortStableFunc(lines, func(a, b LogLine) int { return b.Time.Compare(a.Time) })
	if len(lines) > maxLogLines {
		lines = lines[:maxLogLines]
	}
	return lines, nil
}

// filterLogs keeps the JSON log lines about one object, skipping lines that
// are not JSON (such as a controller's startup banner).
func filterLogs(r io.Reader, kind, namespace, name string) ([]LogLine, error) {
	var lines []LogLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e logEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.ControllerKind != kind || e.Namespace != namespace || e.Name != name {
			continue
		}
		lines = append(lines, LogLine{Time: time.Time(e.Time), Level: e.Level, Message: e.Message, Error: e.Error})
	}
	return lines, sc.Err()
}

// logTime parses the "ts" field, which Flux writes as RFC 3339 but zap
// writes as epoch seconds unless configured otherwise.
type logTime time.Time

func (t *logTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		parsed, err := time.Parse(time.RFC3339Nano, s)
		*t = logTime(parsed)
		return err
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	*t = logTime(time.Unix(0, int64(secs*float64(time.Second))).UTC())
	return nil
}
