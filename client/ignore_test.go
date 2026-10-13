package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/sandbox"
)

func TestGitignoreRules(t *testing.T) {
	g := parseGitignore(`
# dependencies
node_modules/
/dist
*.log
!keep.log
build/**/tmp
coverage
.env*
`)
	for path, want := range map[string]bool{
		"node_modules/react/index.js": true,
		"web/node_modules/a.js":       true, // unanchored: at any depth
		"node_modules.txt":            false,
		"dist/index.html":             true,
		"web/dist/index.html":         false, // anchored to the root
		"debug.log":                   true,
		"logs/keep.log":               false, // negated
		"build/a/b/tmp/x":             true,
		"build/a/b/keep/x":            false,
		"coverage/lcov.info":          true,
		".env.local":                  true,
		"src/App.jsx":                 false,
		"package.json":                false,
	} {
		if got := g.ignored(path); got != want {
			t.Errorf("ignored(%q) = %v, want %v", path, got, want)
		}
	}
}

// With no .gitignore, what package managers and builds write starts
// unticked; with one, the project's own rules decide — including one the
// agent wrote inside the sandbox.
func TestApplyLeavesDependenciesUnticked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sb := sandbox.NewFake(dir)
	diffs := []sandbox.FileDiff{
		{Path: "dist/assets/app.js", Created: true, After: []byte("x")},
		{Path: "node_modules/react/index.js", Created: true, After: []byte("x")},
		{Path: "package.json", Created: true, After: []byte("{}")},
		{Path: "src/App.jsx", Created: true, After: []byte("x")},
	}

	skip, why := unselectedByDefault(ctx, sb)
	rev := renderDiffReview("r", "apply", diffs, skip)
	var order []string
	for _, f := range rev.Files {
		order = append(order, f.Path+map[bool]string{true: "+", false: "-"}[f.Selected])
	}
	if got := strings.Join(order, " "); got != "package.json+ src/App.jsx+ dist/assets/app.js- node_modules/react/index.js-" {
		t.Fatalf("review = %s", got)
	}
	note := unselectedNotice(diffs, skip, why)
	if !strings.Contains(note, "2 files not selected (dist/, node_modules/)") || !strings.Contains(note, why) {
		t.Fatalf("notice = %q", note)
	}

	// A .gitignore that keeps dist: the project's rules win.
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules\n"), 0o644)
	skip, why = unselectedByDefault(ctx, sb)
	if skip("dist/assets/app.js") || !skip("node_modules/react/index.js") || why != "ignored by .gitignore" {
		t.Fatalf("with a .gitignore: dist skipped=%v, node_modules skipped=%v, why=%q",
			skip("dist/assets/app.js"), skip("node_modules/react/index.js"), why)
	}
}
