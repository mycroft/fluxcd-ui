package diff

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/fluxcd/pkg/tar"
)

const (
	// maxArtifactSize bounds an artifact download.
	maxArtifactSize = 100 << 20
	// maxUntarSize bounds an artifact's extracted content.
	maxUntarSize = 256 << 20
)

// fetchArtifact downloads a source artifact, verifies it against its digest
// ("<algorithm>:<hex>") and extracts it into dir.
func fetchArtifact(ctx context.Context, hc *http.Client, artifactURL, digest, dir string) error {
	h, want, err := parseDigest(digest)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("downloading the source artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the source artifact: %s", resp.Status)
	}

	f, err := os.CreateTemp(dir, "artifact-*.tar.gz")
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArtifactSize+1))
	switch {
	case err != nil:
		return fmt.Errorf("downloading the source artifact: %w", err)
	case n > maxArtifactSize:
		return fmt.Errorf("the source artifact is larger than %d MiB", maxArtifactSize>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the source artifact does not match its digest %s", digest)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	src := dir + "/src"
	if err := tar.Untar(f, src, tar.WithMaxUntarSize(maxUntarSize), tar.WithSkipSymlinks()); err != nil {
		return fmt.Errorf("extracting the source artifact: %w", err)
	}
	return nil
}

func parseDigest(digest string) (hash.Hash, string, error) {
	algo, want, ok := strings.Cut(digest, ":")
	if !ok || want == "" {
		return nil, "", fmt.Errorf("unsupported artifact digest %q", digest)
	}
	switch algo {
	case "sha256":
		return sha256.New(), want, nil
	case "sha384":
		return sha512.New384(), want, nil
	case "sha512":
		return sha512.New(), want, nil
	default:
		return nil, "", fmt.Errorf("unsupported artifact digest algorithm %q", algo)
	}
}

// ProxyURL rewrites an in-cluster artifact URL, such as
// http://source-controller.flux-system.svc.cluster.local./gitrepository/...,
// to go through the API server's service proxy, for running outside the
// cluster.
func ProxyURL(apiServer, artifactURL string) (string, error) {
	u, err := url.Parse(artifactURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) < 3 || parts[2] != "svc" {
		return "", errors.New("artifact URL " + artifactURL + " is not an in-cluster service URL")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	return strings.TrimSuffix(apiServer, "/") + "/api/v1/namespaces/" + parts[1] + "/services/" + parts[0] + ":" + port + "/proxy" + u.EscapedPath(), nil
}
