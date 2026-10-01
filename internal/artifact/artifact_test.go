package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink pointing outside the artifact: never extracted.
	if err := tw.WriteHeader(&tar.Header{Name: "escape", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serve serves body as an artifact at path, counting downloads.
func serve(t *testing.T, path string, body []byte, hits *atomic.Int32) *meta.Artifact {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	return &meta.Artifact{URL: srv.URL + "/" + path, Path: path, Digest: "sha256:" + hex.EncodeToString(sum[:]), Revision: "main@sha1:abc"}
}

var repoFiles = map[string]string{
	"apps/deployment.yaml":    "kind: Deployment\n",
	"apps/kustomization.yaml": "resources: [deployment.yaml]\n",
	"README.md":               "# fleet\n",
	"logo.png":                "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
	"big.txt":                 strings.Repeat("a", maxViewSize+10),
}

func newCache(t *testing.T) *Cache {
	return NewCache(NewFetcher(http.DefaultClient, nil), t.TempDir())
}

func TestCacheFilesAndRead(t *testing.T) {
	var hits atomic.Int32
	a := serve(t, "gitrepository/flux-system/fleet/abc.tar.gz", tarball(t, repoFiles), &hits)
	c := newCache(t)
	ctx := context.Background()

	files, err := c.Files(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "README.md,apps/deployment.yaml,apps/kustomization.yaml,big.txt,logo.png" {
		t.Fatalf("files = %s (the symlink must not be extracted)", got)
	}

	content, err := c.Read(ctx, a, "apps/deployment.yaml")
	if err != nil || content.Text != "kind: Deployment\n" || content.Binary || content.Size != 17 {
		t.Errorf("deployment.yaml = %+v, %v", content, err)
	}
	if content, err := c.Read(ctx, a, "logo.png"); err != nil || !content.Binary || content.Text != "" {
		t.Errorf("logo.png = %+v, %v", content, err)
	}
	if content, err := c.Read(ctx, a, "big.txt"); err != nil || !content.Truncated || len(content.Text) != maxViewSize {
		t.Errorf("big.txt: truncated=%t len=%d, %v", content.Truncated, len(content.Text), err)
	}
	for _, p := range []string{"../../etc/passwd", "escape", "apps", "nope.yaml"} {
		if _, err := c.Read(ctx, a, p); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) = %v, want ErrNotFound", p, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("%d downloads, want 1 (cached)", n)
	}
}

func TestCacheDownloadsOnceForConcurrentCallers(t *testing.T) {
	var hits atomic.Int32
	a := serve(t, "a.tar.gz", tarball(t, repoFiles), &hits)
	c := newCache(t)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := c.Files(context.Background(), a); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := hits.Load(); n != 1 {
		t.Errorf("%d downloads for concurrent callers, want 1", n)
	}
}

func TestCacheEviction(t *testing.T) {
	c := newCache(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	var hits atomic.Int32
	var arts []*meta.Artifact
	for i := range maxCached + 1 {
		arts = append(arts, serve(t, "a.tar.gz", tarball(t, map[string]string{"f": strings.Repeat("x", i+1)}), &hits))
		now = now.Add(time.Second)
		if _, err := c.Files(context.Background(), arts[i]); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.entries) != maxCached {
		t.Fatalf("%d cached artifacts, want %d", len(c.entries), maxCached)
	}
	if _, ok := c.entries[arts[0].Digest]; ok {
		t.Error("the least recently used artifact was kept")
	}
}

func TestDownloadSingleFile(t *testing.T) {
	var hits atomic.Int32
	a := serve(t, "helmrepository/flux-system/grafana/index-c2d8bc31.yaml", []byte("apiVersion: v1\nentries: {}\n"), &hits)
	c := newCache(t)
	files, err := c.Files(context.Background(), a)
	if err != nil || len(files) != 1 || files[0].Path != "index.yaml" {
		t.Fatalf("files = %+v, %v", files, err)
	}
}

func TestDownloadRejectsDigestMismatch(t *testing.T) {
	var hits atomic.Int32
	a := serve(t, "a.tar.gz", tarball(t, repoFiles), &hits)
	a.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := newCache(t).Files(context.Background(), a); err == nil || !strings.Contains(err.Error(), "does not match its digest") {
		t.Fatalf("err = %v", err)
	}
}

func TestProxyURL(t *testing.T) {
	got, err := ProxyURL("https://10.0.0.1:6443/", "http://source-controller.flux-system.svc.cluster.local./gitrepository/flux-system/fleet/abc.tar.gz")
	want := "https://10.0.0.1:6443/api/v1/namespaces/flux-system/services/source-controller:80/proxy/gitrepository/flux-system/fleet/abc.tar.gz"
	if err != nil || got != want {
		t.Errorf("ProxyURL = %q, %v", got, err)
	}
	if _, err := ProxyURL("https://k8s", "https://example.com/a.tar.gz"); err == nil {
		t.Error("accepted a non-service URL")
	}
}

func TestCacheClose(t *testing.T) {
	var hits atomic.Int32
	a := serve(t, "a.tar.gz", tarball(t, repoFiles), &hits)
	c := newCache(t)
	if _, err := c.Files(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	dir := c.entries[a.Digest].dir
	c.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("extracted artifact still on disk: %v", err)
	}
}
