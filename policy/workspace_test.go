package policy

import (
	"errors"
	"testing"
)

// TestWorkspacePatternsCoverWhatAnAdministratorMeant is the whole point of the
// matcher: an administrator writing `.git` in the console means the directory
// and everything in it, and a rule that protected only a file literally named
// `.git` would protect nothing at all.
func TestWorkspacePatternsCoverWhatAnAdministratorMeant(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{".git", ".git/config", true},          // a directory covers its contents
		{".git", ".git/refs/heads/main", true}, // however deep
		{".git/**", ".git/config", true},       // the explicit spelling agrees
		{".git", ".gitignore", false},          // and stops at the segment boundary
		{".git", "src/.git/config", false},     // an unanchored name is not a match
		{"**/.git", "src/.git/config", true},   // ** is how you say "anywhere"
		{".env", ".env", true},                 // a plain file
		{".env", ".env.local", false},          // not its neighbours
		{".env*", ".env.local", true},          // unless you say so
		{"*.pem", "server.pem", true},          // a glob within one segment
		{"*.pem", "certs/server.pem", false},   // which does not cross a slash
		{"**/*.pem", "certs/server.pem", true}, // ** does
		{".github/workflows", ".github/workflows/ci.yml", true},
		{".github/workflows", ".github/dependabot.yml", false},
		{"", "anything", false}, // an empty rule protects nothing

		// Case is not a way around a rule. The sandbox is Linux and would call
		// these different files; a macOS or Windows working directory would
		// then apply one over the other.
		{".secure", ".SECURE", true},
		{".env", ".Env", true},
		{".git", ".GIT/config", true},
		{"*.pem", "SERVER.PEM", true},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestWorkspaceProtectionsComeFromTheSnapshot(t *testing.T) {
	snap := &Snapshot{Workspace: []WorkspacePolicy{
		{ID: "p_secrets", Spec: WorkspaceSpec{Kind: "workspace", Mode: ModeExclude, Paths: []string{".env", "**/*.pem"}}},
		{ID: "p_git", Spec: WorkspaceSpec{Kind: "workspace", Mode: ModeReadOnly, Paths: []string{".git"}}},
		{ID: "p_ci", Spec: WorkspaceSpec{Kind: "workspace", Mode: ModeNoDelete, Paths: []string{".github/workflows"}}},
		// A mode this build does not understand is dropped, not guessed at.
		{ID: "p_future", Spec: WorkspaceSpec{Kind: "workspace", Mode: "quarantine", Paths: []string{"src"}}},
		// An empty pattern would match nothing; it never becomes a rule.
		{ID: "p_blank", Spec: WorkspaceSpec{Kind: "workspace", Mode: ModeExclude, Paths: []string{"", "  "}}},
	}}

	prot := snap.Protections()
	if got, want := prot.Rules(), 4; got != want {
		t.Fatalf("rules = %d, want %d: %+v", got, want, prot)
	}
	if prot.Empty() {
		t.Fatal("protections report themselves empty")
	}
	if !prot.Excluded("certs/server.pem") {
		t.Error("**/*.pem did not exclude certs/server.pem")
	}
	if prot.Excluded("src/main.go") {
		t.Error("an unknown mode leaked into an exclusion")
	}

	// The refusal names the rule behind it, not just the fact of it.
	var pe *ProtectedError
	if err := prot.CheckRead(".env"); !errors.As(err, &pe) {
		t.Fatalf("CheckRead(.env) = %v, want a ProtectedError", err)
	}
	if pe.Mode != ModeExclude || pe.PolicyID != "p_secrets" || pe.Pattern != ".env" {
		t.Errorf("refusal = %+v", pe)
	}
}

// TestTheModesAreALadder: reading is the least a policy can forbid and
// deleting the most, so each mode forbids everything the ones above it do.
func TestTheModesAreALadder(t *testing.T) {
	snap := &Snapshot{Workspace: []WorkspacePolicy{
		{ID: "p_secrets", Spec: WorkspaceSpec{Mode: ModeExclude, Paths: []string{".env"}}},
		{ID: "p_git", Spec: WorkspaceSpec{Mode: ModeReadOnly, Paths: []string{".git"}}},
		{ID: "p_ci", Spec: WorkspaceSpec{Mode: ModeNoDelete, Paths: []string{".github/workflows"}}},
	}}
	prot := snap.Protections()

	check := func(name string, err error, wantMode string) {
		t.Helper()
		var pe *ProtectedError
		switch {
		case wantMode == "" && err != nil:
			t.Errorf("%s: refused with %v, want allowed", name, err)
		case wantMode == "":
		case !errors.As(err, &pe):
			t.Errorf("%s: %v, want a %s refusal", name, err, wantMode)
		case pe.Mode != wantMode:
			t.Errorf("%s: refused as %s, want %s", name, pe.Mode, wantMode)
		}
	}

	// Excluded: not even readable.
	check("read .env", prot.CheckRead(".env"), ModeExclude)
	check("write .env", prot.CheckWrite(".env"), ModeExclude)
	check("delete .env", prot.CheckDelete(".env"), ModeExclude)

	// Read-only: readable, and deleting it is the largest write there is.
	check("read .git/config", prot.CheckRead(".git/config"), "")
	check("write .git/config", prot.CheckWrite(".git/config"), ModeReadOnly)
	check("delete .git/config", prot.CheckDelete(".git/config"), ModeReadOnly)

	// No-delete: editable, not removable.
	check("read ci.yml", prot.CheckRead(".github/workflows/ci.yml"), "")
	check("write ci.yml", prot.CheckWrite(".github/workflows/ci.yml"), "")
	check("delete ci.yml", prot.CheckDelete(".github/workflows/ci.yml"), ModeNoDelete)

	// An unprotected path stays unprotected.
	check("delete src/main.go", prot.CheckDelete("src/main.go"), "")
}

// TestNoProtectionsForbidNothing: a workspace with no policies behaves exactly
// as it did before there were any.
func TestNoProtectionsForbidNothing(t *testing.T) {
	var prot Protections
	if !prot.Empty() || prot.Rules() != 0 {
		t.Fatal("the zero Protections is not empty")
	}
	for _, p := range []string{".env", ".git/config", "src/main.go"} {
		if err := prot.CheckDelete(p); err != nil {
			t.Errorf("CheckDelete(%q) = %v, want nil", p, err)
		}
	}
}
