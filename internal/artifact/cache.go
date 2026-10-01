package artifact

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/fluxcd/pkg/apis/meta"
	"golang.org/x/sync/singleflight"
)

const (
	// maxCached is how many extracted artifacts are kept for browsing.
	maxCached = 2
	// cacheTTL drops artifacts nobody browsed for a while.
	cacheTTL = 10 * time.Minute
	// maxViewSize caps the content shown for one file.
	maxViewSize = 1 << 20
	// binarySniffLen is how much of a file is checked for binary content.
	binarySniffLen  = 8000
	downloadTimeout = 2 * time.Minute
)

// ErrNotFound is returned for a path that is not a file of the artifact.
var ErrNotFound = errors.New("no such file in the artifact")

// File is a file in an artifact.
type File struct {
	Path string // slash-separated, relative to the artifact root
	Size int64
}

// Content is a file's content, for display.
type Content struct {
	File
	Text      string
	Binary    bool // not shown
	Truncated bool // only the first maxViewSize bytes are shown
}

// Cache keeps extracted artifacts on disk, by digest, so browsing their
// files does not download them again.
type Cache struct {
	fetcher *Fetcher
	root    string
	group   singleflight.Group

	mu      sync.Mutex
	entries map[string]*entry // by digest
	now     func() time.Time
}

type entry struct {
	dir   string
	files []File
	used  time.Time
}

// NewCache returns a cache extracting artifacts under root ("" for the
// default temporary directory).
func NewCache(f *Fetcher, root string) *Cache {
	return &Cache{fetcher: f, root: root, entries: map[string]*entry{}, now: time.Now}
}

// Files lists an artifact's files, sorted by path.
func (c *Cache) Files(ctx context.Context, a *meta.Artifact) ([]File, error) {
	e, err := c.get(ctx, a)
	if err != nil {
		return nil, err
	}
	return e.files, nil
}

// Read returns one of an artifact's files.
func (c *Cache) Read(ctx context.Context, a *meta.Artifact, path string) (*Content, error) {
	e, err := c.get(ctx, a)
	if err != nil {
		return nil, err
	}
	i, found := slices.BinarySearchFunc(e.files, path, func(f File, p string) int { return cmp.Compare(f.Path, p) })
	if !found {
		return nil, ErrNotFound
	}
	full, err := securejoin.SecureJoin(e.dir, filepath.FromSlash(path))
	if err != nil {
		return nil, ErrNotFound
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxViewSize))
	if err != nil {
		return nil, err
	}

	content := &Content{File: e.files[i], Truncated: e.files[i].Size > maxViewSize}
	if bytes.IndexByte(b[:min(len(b), binarySniffLen)], 0) >= 0 {
		content.Binary = true
		return content, nil
	}
	content.Text = strings.ToValidUTF8(string(b), "�")
	return content, nil
}

// get returns the extracted artifact, downloading it once for concurrent
// callers.
func (c *Cache) get(ctx context.Context, a *meta.Artifact) (*entry, error) {
	if a == nil {
		return nil, errors.New("the source has no artifact yet")
	}
	c.mu.Lock()
	if e, ok := c.entries[a.Digest]; ok {
		e.used = c.now()
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()

	v, err, _ := c.group.Do(a.Digest, func() (any, error) {
		// The download is shared: don't let one caller's cancellation abort it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), downloadTimeout)
		defer cancel()
		dir, err := os.MkdirTemp(c.root, "fluxcd-ui-artifact-")
		if err != nil {
			return nil, err
		}
		if err := c.fetcher.Download(ctx, a, dir+"/files"); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		files, err := listFiles(dir + "/files")
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		e := &entry{dir: dir + "/files", files: files, used: c.now()}
		c.mu.Lock()
		c.entries[a.Digest] = e
		c.evictLocked(a.Digest)
		c.mu.Unlock()
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*entry), nil
}

// evictLocked drops idle artifacts, then the least recently used ones
// beyond maxCached, keeping the one just added.
func (c *Cache) evictLocked(keep string) {
	drop := func(digest string) {
		_ = os.RemoveAll(filepath.Dir(c.entries[digest].dir))
		delete(c.entries, digest)
	}
	for digest, e := range c.entries {
		if digest != keep && c.now().Sub(e.used) > cacheTTL {
			drop(digest)
		}
	}
	for len(c.entries) > maxCached {
		var oldest string
		for digest, e := range c.entries {
			if digest != keep && (oldest == "" || e.used.Before(c.entries[oldest].used)) {
				oldest = digest
			}
		}
		drop(oldest)
	}
}

// Close removes the extracted artifacts.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for digest, e := range c.entries {
		_ = os.RemoveAll(filepath.Dir(e.dir))
		delete(c.entries, digest)
	}
}

// listFiles lists the regular files under dir, sorted by path.
func listFiles(dir string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, File{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing the artifact's files: %w", err)
	}
	slices.SortFunc(files, func(a, b File) int { return cmp.Compare(a.Path, b.Path) })
	return files, nil
}
