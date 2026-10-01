package flux

import "strings"

const shortDigestLen = 7

// ShortRevision shortens a Flux revision for display while keeping its ref:
//
//	"main@sha1:cb89c25f8d70…"    → "main@cb89c25"
//	"latest@sha256:a911ac64b9…"  → "latest@a911ac6"
//	"sha1:cb89c25f8d70…"         → "cb89c25"
//	"sha256:7109707984ef…"       → "sha256:7109707"
//	"1.13.0"                     → "1.13.0"
func ShortRevision(rev string) string {
	if i := strings.LastIndex(rev, "@"); i >= 0 {
		return rev[:i+1] + shortHex(rev[i+1:])
	}
	algo, hex, ok := strings.Cut(rev, ":")
	if !ok || !isHex(hex) {
		return rev
	}
	if algo == "sha1" {
		return shortHex(hex)
	}
	return algo + ":" + shortHex(hex)
}

// shortHex truncates a digest, dropping its "algo:" prefix if present.
func shortHex(digest string) string {
	if _, hex, ok := strings.Cut(digest, ":"); ok {
		digest = hex
	}
	if len(digest) > shortDigestLen && isHex(digest) {
		return digest[:shortDigestLen]
	}
	return digest
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
