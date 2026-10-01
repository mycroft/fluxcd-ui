// Package artifact downloads and reads the artifacts source-controller
// stores for Flux sources.
package artifact

import (
	"cmp"
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
	"path"
	"path/filepath"
	"strings"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/tar"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// maxArtifactSize bounds an artifact download.
	maxArtifactSize = 100 << 20
	// maxUntarSize bounds an artifact's extracted content.
	maxUntarSize = 256 << 20
)

// Of returns the artifact a source object holds, if any.
func Of(obj client.Object) *meta.Artifact {
	switch o := obj.(type) {
	case *sourcev1.GitRepository:
		return o.Status.Artifact
	case *sourcev1.OCIRepository:
		return o.Status.Artifact
	case *sourcev1.Bucket:
		return o.Status.Artifact
	case *sourcev1.HelmRepository:
		return o.Status.Artifact
	case *sourcev1.HelmChart:
		return o.Status.Artifact
	default:
		return nil
	}
}

// Fetcher downloads artifacts.
type Fetcher struct {
	http    *http.Client
	rewrite func(string) (string, error) // nil to fetch artifact URLs directly
}

// NewFetcher returns a Fetcher. rewrite, when set, maps artifact URLs to
// reachable ones (see ProxyURL).
func NewFetcher(hc *http.Client, rewrite func(string) (string, error)) *Fetcher {
	return &Fetcher{http: hc, rewrite: rewrite}
}

// Download fetches an artifact, verifies it against its digest, and
// extracts it into dir. Archives (.tar.gz, .tgz) are extracted without
// symlinks; other artifacts, such as a HelmRepository's index, are saved as a
// single file named after the artifact.
func (f *Fetcher) Download(ctx context.Context, a *meta.Artifact, dir string) error {
	h, want, err := parseDigest(a.Digest)
	if err != nil {
		return err
	}
	artifactURL := a.URL
	if f.rewrite != nil {
		if artifactURL, err = f.rewrite(artifactURL); err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("downloading the source artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the source artifact: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dir), "download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxArtifactSize+1))
	switch {
	case err != nil:
		return fmt.Errorf("downloading the source artifact: %w", err)
	case n > maxArtifactSize:
		return fmt.Errorf("the source artifact is larger than %d MiB", maxArtifactSize>>20)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the source artifact does not match its digest %s", a.Digest)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if isArchive(cmp.Or(a.Path, a.URL)) {
		if err := tar.Untar(tmp, dir, tar.WithMaxUntarSize(maxUntarSize), tar.WithSkipSymlinks()); err != nil {
			return fmt.Errorf("extracting the source artifact: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(dir, singleFileName(cmp.Or(a.Path, a.URL))))
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, tmp); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func isArchive(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}

// singleFileName names a non-archive artifact: a HelmRepository's
// "index-<digest>.yaml" becomes "index.yaml".
func singleFileName(p string) string {
	base := path.Base(p)
	if strings.HasPrefix(base, "index-") && strings.HasSuffix(base, ".yaml") {
		return "index.yaml"
	}
	return base
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
