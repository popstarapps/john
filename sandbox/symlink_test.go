package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/policy"
)

// Symbolic links, in both directions.
//
// A checkout that uses them is ordinary — a pnpm workspace, a node_modules
// /.bin, a monorepo linking one package into another — and a sandbox that
// serves a link as "no such file" makes such a project look broken in a way
// nothing in it explains. So links cross.
//
// The reason they did not for so long is the other half of these tests: a link
// is a path to anywhere, and following one on the host is the way out of the
// workspace. Nothing on the host side follows a link out. The guest resolves
// them itself, inside the workspace, and what it cannot resolve there it
// refuses by name.

func linkTree(t *testing.T, files map[string]string, links map[string]string) string {
	t.Helper()
	dir := hostTree(t, files)
	// Probed even when there are no links to make: a test that has the union
	// make its own writes them to a host directory too. Windows refuses an
	// unelevated process without Developer Mode; in production the union runs
	// inside a Linux guest and never asks the host.
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink("target", probe); err != nil {
		t.Skipf("this filesystem cannot make symbolic links: %v", err)
	}
	for name, target := range links {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Skipf("this filesystem cannot make symbolic links: %v", err)
		}
	}
	return dir
}

func unionOver(t *testing.T, dir string, prot policy.Protections) *union {
	t.Helper()
	u, err := newUnion(&directLower{newHostFS(dir, prot)}, t.TempDir(), prot)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestALinkInTheDevelopersTreeIsServedAsALink(t *testing.T) {
	dir := linkTree(t,
		map[string]string{"pkg/lib.go": "package pkg\n", "notes.md": "hello\n"},
		map[string]string{"linked-pkg": "pkg", "alias.md": "notes.md"})
	u := unionOver(t, dir, policy.Protections{})
	ctx := context.Background()

	st, err := u.Stat(ctx, "alias.md")
	if err != nil {
		t.Fatalf("stat alias.md: %v", err)
	}
	if st.Link != "notes.md" {
		t.Fatalf("alias.md link = %q, want notes.md", st.Link)
	}
	if st.Dir {
		t.Fatal("a link is not a directory")
	}

	// It is in the listing under its own name, not missing and not resolved.
	if got := names(mustReadDir(t, u, "")); !named(got, "alias.md") || !named(got, "linked-pkg") {
		t.Fatalf("listing = %v, want the links in it", got)
	}

	// And a path through a link reaches what it points at.
	rel, err := u.follow(ctx, "linked-pkg/lib.go", true)
	if err != nil {
		t.Fatalf("follow linked-pkg/lib.go: %v", err)
	}
	if rel != "pkg/lib.go" {
		t.Fatalf("linked-pkg/lib.go resolved to %q, want pkg/lib.go", rel)
	}
	if got := mustRead(t, u, rel); got != "package pkg\n" {
		t.Fatalf("read through the link = %q", got)
	}
}

// The half that is a control rather than a convenience. A link out of the
// workspace is refused, and — the part no guest-side rule could enforce — the
// host never follows one on the guest's behalf, so a path through it is not a
// second way to ask.
func TestALinkIsNotAWayOutOfTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := linkTree(t, map[string]string{"main.go": "package main\n"},
		map[string]string{"out": outside, "up": "../..", "loop": "loop2", "loop2": "loop"})
	u := unionOver(t, dir, policy.Protections{})
	ctx := context.Background()

	for _, p := range []string{"out/secret", "up/etc/passwd"} {
		if rel, err := u.follow(ctx, p, true); err == nil {
			t.Fatalf("follow %s resolved to %q, want a refusal", p, rel)
		} else if !strings.Contains(err.Error(), "outside the workspace") {
			t.Fatalf("follow %s: %v — want a refusal that says why", p, err)
		}
	}

	// A cycle ends, rather than being walked until something else gives out.
	if _, err := u.follow(ctx, "loop", true); err == nil ||
		!strings.Contains(err.Error(), "too many levels") {
		t.Fatalf("follow loop: %v — want it to give up naming the reason", err)
	}

	// The lower layer refuses to walk one even when asked directly, which is
	// what makes the refusal above enforceable rather than merely polite: the
	// operating system would have resolved this path happily.
	h := newHostFS(dir, policy.Protections{})
	if _, err := h.stat("out/secret"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("host stat out/secret = %v, want no such file", err)
	}
	if _, err := h.readAt("out/secret", 0, 16); err == nil {
		t.Fatal("the host read a file outside the workspace through a link")
	}
	// The link itself is served, so the guest can see what it is and say so.
	if st, err := h.stat("out"); err != nil || st.Link != linkTarget(outside) {
		t.Fatalf("host stat out = %+v, %v — want the link itself", st, err)
	}
}

// A link is a path, so it is also a way to spell a path a policy excludes.
// The protections are checked on what a path resolves to, not on how it was
// spelled.
func TestALinkDoesNotWalkAroundAnExcludedPath(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			dir := linkTree(t,
				map[string]string{".env": "STRIPE_KEY=sk_live_real\n", "main.go": "package main\n"},
				map[string]string{"secrets": ".env", "cfg/env": "../.env"})
			prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})
			sb := im.build(t, dir, prot)
			ctx := context.Background()

			for _, spelling := range []string{".env", "secrets", "cfg/env"} {
				if _, err := sb.Read(ctx, spelling, 0); err == nil {
					t.Fatalf("read %s succeeded — the exclusion was walked around", spelling)
				}
			}
		})
	}
}

// npm install does almost nothing else, so a sandbox where it fails is a
// sandbox a JavaScript project cannot be worked on in.
func TestTheSandboxCanMakeItsOwnLinks(t *testing.T) {
	dir := linkTree(t, map[string]string{"node_modules/lib/cli.js": "#!/usr/bin/env node\n"}, nil)
	u := unionOver(t, dir, policy.Protections{})
	ctx := context.Background()

	if err := u.Symlink(ctx, "../lib/cli.js", "node_modules/.bin/lib"); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target, err := u.Readlink(ctx, "node_modules/.bin/lib")
	if err != nil || target != "../lib/cli.js" {
		t.Fatalf("readlink = %q, %v", target, err)
	}
	rel, err := u.follow(ctx, "node_modules/.bin/lib", true)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if got := mustRead(t, u, rel); got != "#!/usr/bin/env node\n" {
		t.Fatalf("through the sandbox's own link = %q", got)
	}

	// Deleting a link removes the link, not what it names.
	if err := u.Remove(ctx, "node_modules/.bin/lib", false); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if _, err := u.Stat(ctx, "node_modules/.bin/lib"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link is still there: %v", err)
	}
	if got := mustRead(t, u, "node_modules/lib/cli.js"); got != "#!/usr/bin/env node\n" {
		t.Fatalf("deleting the link took the file with it: %q", got)
	}
}

// The last thing standing between a link and the developer's files. Apply is
// the only code that writes that tree, and these are the two answers it may
// give: follow a link that stays inside the workspace, refuse one that does
// not, and never write through either.
func TestApplyDoesNotWriteThroughALink(t *testing.T) {
	outside := t.TempDir()
	dir := linkTree(t,
		map[string]string{"packages/app/main.go": "package main\n", "keep": "original\n"},
		map[string]string{"app": "packages/app", "out": outside, "keep-link": "keep"})

	// Inside the workspace: followed, because a monorepo that links one
	// package into another is an ordinary checkout.
	got, err := hostPathFor(dir, "app/main.go", false)
	if err != nil {
		t.Fatalf("app/main.go: %v", err)
	}
	if want := filepath.Join(dir, "packages", "app", "main.go"); got != want {
		t.Fatalf("app/main.go resolved to %q, want %q", got, want)
	}

	// Out of it: refused by name, and the file outside is never created.
	if _, err := hostPathFor(dir, "out/planted", false); err == nil {
		t.Fatal("a path through a link out of the workspace was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("something was written outside the workspace")
	}

	// The last element is not followed: it is replaced, so a link the
	// developer has does not become the route the sandbox's bytes take.
	got, err = hostPathFor(dir, "keep-link", false)
	if err != nil {
		t.Fatalf("keep-link: %v", err)
	}
	if want := filepath.Join(dir, "keep-link"); got != want {
		t.Fatalf("keep-link resolved to %q, want the link's own path %q", got, want)
	}
}

func mustReadDir(t *testing.T, u *union, rel string) []lowerStat {
	t.Helper()
	entries, err := u.ReadDir(context.Background(), rel)
	if err != nil {
		t.Fatalf("readdir %q: %v", rel, err)
	}
	return entries
}

func named(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
