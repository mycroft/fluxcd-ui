package flux

import (
	"testing"
	"time"
)

func TestShortRevision(t *testing.T) {
	tests := map[string]string{
		"generated@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc":                        "generated@cb89c25",
		"refs/heads/main@sha1:cb89c25f8d709a17d3800304fad747848f3a42cc":                  "refs/heads/main@cb89c25",
		"latest@sha256:a911ac64b97befdcd44ec69e5ac10d92a3b4562b505fa314297e48ce531c31f9": "latest@a911ac6",
		"1.13.0@sha256:79424a0affe3dbb4d553336268469799051d98f1551e235e56ea98cc96d26c99": "1.13.0@79424a0",
		"sha1:cb89c25f8d709a17d3800304fad747848f3a42cc":                                  "cb89c25",
		"sha256:7109707984ef623368e15635d2b88c38d74a9e977e48d70bafa2dd101b4a2e12":        "sha256:7109707",
		"1.13.0":         "1.13.0",
		"v1.2.3+build.4": "v1.2.3+build.4",
		"":               "",
	}
	for in, want := range tests {
		if got := ShortRevision(in); got != want {
			t.Errorf("ShortRevision(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[string]string{"1m0s": "1m", "1h0m0s": "1h", "1h30m0s": "1h30m", "30s": "30s", "1m30s": "1m30s"}
	for in, want := range tests {
		d, err := time.ParseDuration(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", in, got, want)
		}
	}
}
