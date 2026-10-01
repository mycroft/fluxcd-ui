package diff

import (
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func obj(t *testing.T, y string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func paths(changes []Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Path
	}
	return out
}

const desiredDeployment = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: apps
  labels: {app: podinfo}
  annotations: {example.com/owner: team-a}
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/stefanprodan/podinfo:6.7.1
          resources: {limits: {cpu: 500m, memory: 1Gi}}
          env:
            - {name: LEVEL, value: info}
        - name: sidecar
          image: busybox
`

func TestCompareUnchangedIgnoresLiveOnlyFields(t *testing.T) {
	live := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: apps
  uid: 1234
  resourceVersion: "42"
  creationTimestamp: "2026-10-01T00:00:00Z"
  labels: {app: podinfo, kustomize.toolkit.fluxcd.io/name: apps}
  annotations: {example.com/owner: team-a, deployment.kubernetes.io/revision: "3"}
spec:
  replicas: 2
  strategy: {type: RollingUpdate}
  template:
    spec:
      containers:
        # Reordered, with defaults the API server added.
        - name: sidecar
          image: busybox
          imagePullPolicy: Always
        - name: podinfo
          image: ghcr.io/stefanprodan/podinfo:6.7.1
          terminationMessagePath: /dev/termination-log
          resources: {limits: {cpu: "0.5", memory: 1024Mi}}
          env:
            - {name: LEVEL, value: info}
status: {replicas: 2}
`)
	if changes := compareObjects(obj(t, desiredDeployment), live); len(changes) != 0 {
		t.Fatalf("unexpected changes: %+v", changes)
	}
}

func TestCompareReportsDrift(t *testing.T) {
	live := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: podinfo
  namespace: apps
  labels: {app: podinfo}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: podinfo
          image: ghcr.io/stefanprodan/podinfo:6.7.0
          resources: {limits: {cpu: 500m, memory: 1Gi}}
          env:
            - {name: LEVEL, value: debug}
`)
	changes := compareObjects(obj(t, desiredDeployment), live)
	want := []string{
		`metadata.annotations["example.com/owner"]`,
		"spec.replicas",
		"spec.template.spec.containers[name=podinfo].env[name=LEVEL].value",
		"spec.template.spec.containers[name=podinfo].image",
		"spec.template.spec.containers[name=sidecar]",
	}
	if got := paths(changes); !slices.Equal(got, want) {
		t.Fatalf("changes = %q\nwant      %q", got, want)
	}
	if c := changes[1]; c.Live != "3" || c.Desired != "2" {
		t.Errorf("replicas change = %+v", c)
	}
	if c := changes[0]; c.Live != notSet || c.Desired != "team-a" {
		t.Errorf("missing annotation change = %+v", c)
	}
}

func TestCompareUnnamedLists(t *testing.T) {
	desired := obj(t, `{spec: {args: [--port, "9898"]}}`)
	if c := compareObjects(desired, obj(t, `{spec: {args: [--port, "9898"]}}`)); len(c) != 0 {
		t.Errorf("same list: %+v", c)
	}
	if c := compareObjects(desired, obj(t, `{spec: {args: [--port, "8080"]}}`)); !slices.Equal(paths(c), []string{"spec.args[1]"}) {
		t.Errorf("changed item: %+v", c)
	}
	if c := compareObjects(desired, obj(t, `{spec: {args: [--port]}}`)); !slices.Equal(paths(c), []string{"spec.args"}) {
		t.Errorf("different length: %+v", c)
	}
}

func TestScalarEqual(t *testing.T) {
	tests := []struct {
		desired, live any
		equal         bool
	}{
		{int64(2), float64(2), true},
		{"500m", "0.5", true},
		{"1Gi", "1024Mi", true},
		{"1Gi", "1G", false},
		{"8080", int64(8080), false}, // a string port is not a number port
		{true, true, true},
		{true, "true", false},
		{"info", "debug", false},
	}
	for _, tt := range tests {
		if got := scalarEqual(tt.desired, tt.live); got != tt.equal {
			t.Errorf("scalarEqual(%#v, %#v) = %t", tt.desired, tt.live, got)
		}
	}
}

func TestFormatTruncates(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := format(string(long)); len(got) != maxValueLen+len("…") {
		t.Errorf("len = %d", len(got))
	}
	if got := format(map[string]any{"a": int64(1)}); got != `{"a":1}` {
		t.Errorf("map = %s", got)
	}
}
