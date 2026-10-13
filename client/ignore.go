package client

import (
	"context"
	"path"
	"regexp"
	"strings"

	"github.com/popstarapps/john/sandbox"
)

// What apply offers unticked.
//
// Apply shows every change the sandbox holds — what to write is the
// developer's choice, and nothing is hidden. But a sandbox that ran
// `npm install` holds two thousand files of node_modules built for Linux, and
// a review that ticks them alongside the eleven files of the app writes them
// into a Mac's project with one keypress. So what the project's .gitignore
// ignores starts unticked, as do the dependency and build directories every
// toolchain makes when there is no .gitignore to say; ticking them is a
// keypress away.

// unselectedDirs are left unticked when the project has no .gitignore: what
// package managers and builds write, and nobody commits.
var unselectedDirs = []string{
	"node_modules", "dist", "build", "out", "target", ".next", ".nuxt", ".svelte-kit",
	"__pycache__", ".venv", "venv", ".pytest_cache", ".mypy_cache", ".gradle",
	".cache", ".parcel-cache", ".turbo", "coverage",
}

// unselectedByDefault returns what apply leaves unticked, and why — for the
// notice that says so. The .gitignore is the sandbox's: one the agent wrote
// counts, and the host's shows through when it did not.
func unselectedByDefault(ctx context.Context, sb sandbox.Sandbox) (skip func(string) bool, why string) {
	if c, err := sb.Read(ctx, ".gitignore", 256<<10); err == nil && len(c.Data) > 0 {
		m := parseGitignore(string(c.Data))
		return m.ignored, "ignored by .gitignore"
	}
	return func(p string) bool {
		for _, part := range strings.Split(path.Dir(p), "/") {
			for _, d := range unselectedDirs {
				if part == d {
					return true
				}
			}
		}
		return false
	}, "dependencies and build output"
}

// gitignore is the root .gitignore's rules, enough of the format for what
// projects write: comments, negation, directory-only rules, anchored and
// unanchored patterns, * ? [..] and **. Nested .gitignore files are not read.
type gitignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

func parseGitignore(text string) *gitignore {
	g := &gitignore{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		}
		line = strings.TrimPrefix(line, `\`)
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		// A pattern with a slash before its end is relative to the root;
		// one without matches at any depth.
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		expr := globToRegexp(line)
		if !anchored {
			expr = `(?:.*/)?` + expr
		}
		re, err := regexp.Compile(`^` + expr + `$`)
		if err != nil {
			continue
		}
		r.re = re
		g.rules = append(g.rules, r)
	}
	return g
}

// ignored reports whether a file is ignored: matched itself, or under a
// directory that is. The last rule to match decides, as git's does.
func (g *gitignore) ignored(file string) bool {
	parts := strings.Split(file, "/")
	for i := 1; i <= len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		isDir := i < len(parts)
		ignored := false
		for _, r := range g.rules {
			if r.dirOnly && !isDir {
				continue
			}
			if r.re.MatchString(p) {
				ignored = !r.negate
			}
		}
		if ignored {
			return true
		}
	}
	return false
}

func globToRegexp(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			i++
			if i+1 < len(glob) && glob[i+1] == '/' {
				i++
				b.WriteString(`(?:.*/)?`) // "**/": any number of directories
			} else {
				b.WriteString(`.*`)
			}
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		case c == '[':
			if j := strings.IndexByte(glob[i:], ']'); j > 0 {
				class := glob[i+1 : i+j]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("[" + class + "]")
				i += j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
