package web

import (
	"cmp"
	"net/url"
	"slices"
	"strings"

	"github.com/mycroft/fluxcd-ui/internal/artifact"
)

// expandAbove is the number of files above which directories start
// collapsed in the artifact browser.
const expandAbove = 40

// treeNode is a directory or a file of an artifact.
type treeNode struct {
	Name     string
	Size     int64  // files
	URL      string // files: where their content is served
	Dir      bool
	Files    int  // directories: files below
	Open     bool // directories: rendered expanded
	Children []*treeNode
}

// buildTree arranges an artifact's files into directories, sorted with
// directories first. fileURL is where file contents are served.
func buildTree(fileURL string, files []artifact.File) (*treeNode, int, int64) {
	root := &treeNode{Dir: true}
	var total int64
	for _, f := range files {
		total += f.Size
		parts := strings.Split(f.Path, "/")
		dir := root
		for _, p := range parts[:len(parts)-1] {
			dir.Files++
			i := slices.IndexFunc(dir.Children, func(n *treeNode) bool { return n.Dir && n.Name == p })
			if i < 0 {
				dir.Children = append(dir.Children, &treeNode{Name: p, Dir: true})
				i = len(dir.Children) - 1
			}
			dir = dir.Children[i]
		}
		dir.Files++
		dir.Children = append(dir.Children, &treeNode{
			Name: parts[len(parts)-1], Size: f.Size, URL: fileURL + "?path=" + url.QueryEscape(f.Path),
		})
	}
	sortTree(root, len(files) <= expandAbove)
	return root, len(files), total
}

func sortTree(n *treeNode, open bool) {
	n.Open = open
	slices.SortFunc(n.Children, func(a, b *treeNode) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	for _, c := range n.Children {
		if c.Dir {
			// A lone directory, like a Helm chart's top-level one, always opens.
			sortTree(c, open || len(n.Children) == 1)
		}
	}
}
