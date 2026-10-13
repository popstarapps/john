package client

import (
	"fmt"
	"sort"
	"strings"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// splitProtected separates the changes a workspace-protection policy holds
// back from the ones the developer may apply. The held-back ones are not
// hidden — that the agent wrote to a protected path is exactly the kind of
// thing a developer should hear about — they are simply not offered as a
// choice, because the sandbox will refuse them at Apply either way.
func splitProtected(diffs []sandbox.FileDiff) (open, held []sandbox.FileDiff) {
	for _, d := range diffs {
		if d.Protected != "" {
			held = append(held, d)
		} else {
			open = append(open, d)
		}
	}
	return open, held
}

// heldBackNotice is the one line that tells a developer what policy kept out
// of their working directory, naming a few paths rather than all of them: a
// sandbox that ran `git init` under an excluded `.git` can hold back hundreds.
func heldBackNotice(held []sandbox.FileDiff) string {
	const show = 3
	named := make([]string, 0, show)
	for _, d := range held {
		if len(named) == show {
			break
		}
		named = append(named, d.Path+" ("+d.Protected+")")
	}
	line := fmt.Sprintf("%s held back by workspace protection: %s",
		plural(len(held), "change"), strings.Join(named, ", "))
	if rest := len(held) - len(named); rest > 0 {
		line += fmt.Sprintf(" and %d more", rest)
	}
	return line
}

// renderDiffReview turns the sandbox's file diffs into the review the
// developer decides on. The review is built from what the sandbox holds, not
// from what the agent said it did.
//
// A file skip says is offered unticked, and after the ticked ones, so the
// review opens on the work rather than on node_modules. Nil ticks everything.
func renderDiffReview(id, title string, diffs []sandbox.FileDiff, skip func(string) bool) terminalui.DiffReview {
	rev := terminalui.DiffReview{ID: id, Title: title}
	defer func() {
		sort.SliceStable(rev.Files, func(i, j int) bool { return rev.Files[i].Selected && !rev.Files[j].Selected })
	}()
	for _, d := range diffs {
		file := terminalui.DiffFile{Path: d.Path, Selected: skip == nil || !skip(d.Path), Binary: d.Binary}
		if d.Binary {
			file.Added, file.Removed = 1, 1
			rev.Files = append(rev.Files, file)
			continue
		}
		before, after := splitLines(d.Before), splitLines(d.After)
		if d.Created {
			before = nil
		}
		if d.Deleted {
			after = nil
		}
		lines := diffLines(before, after)
		for _, l := range lines {
			switch l.Kind {
			case terminalui.DiffAdd:
				file.Added++
			case terminalui.DiffRemove:
				file.Removed++
			}
		}
		file.Hunks = []terminalui.DiffHunk{{
			Header: fmt.Sprintf("@@ -1,%d +1,%d @@ %s", len(before), len(after), d.Path),
			Lines:  lines,
		}}
		rev.Files = append(rev.Files, file)
	}
	return rev
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// diffLines is a line diff over the longest common subsequence.
//
// Its safety property is under test: every line of the new content appears in
// the output, because a review that hides a line is a review that approves a
// line nobody read.
func diffLines(before, after []string) []terminalui.DiffLine {
	n, m := len(before), len(after)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if before[i] == after[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []terminalui.DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case before[i] == after[j]:
			out = append(out, terminalui.DiffLine{Kind: terminalui.DiffContext, Text: before[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, terminalui.DiffLine{Kind: terminalui.DiffRemove, Text: before[i]})
			i++
		default:
			out = append(out, terminalui.DiffLine{Kind: terminalui.DiffAdd, Text: after[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, terminalui.DiffLine{Kind: terminalui.DiffRemove, Text: before[i]})
	}
	for ; j < m; j++ {
		out = append(out, terminalui.DiffLine{Kind: terminalui.DiffAdd, Text: after[j]})
	}
	return out
}

// unselectedNotice says what apply left unticked, and why, naming the
// directories rather than two thousand files. Empty when it left nothing.
func unselectedNotice(diffs []sandbox.FileDiff, skip func(string) bool, why string) string {
	n := 0
	var dirs []string
	seen := map[string]bool{}
	for _, d := range diffs {
		if !skip(d.Path) {
			continue
		}
		n++
		top, _, nested := strings.Cut(d.Path, "/")
		if !nested {
			top = d.Path
		} else {
			top += "/"
		}
		if !seen[top] {
			seen[top] = true
			dirs = append(dirs, top)
		}
	}
	if n == 0 {
		return ""
	}
	const show = 3
	named := strings.Join(dirs[:min(show, len(dirs))], ", ")
	if len(dirs) > show {
		named += fmt.Sprintf(" and %d more", len(dirs)-show)
	}
	return fmt.Sprintf("%s not selected (%s) · %s · tick them in the review to apply them too", plural(n, "file"), named, why)
}
