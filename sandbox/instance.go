package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A workspace has one sandbox directory and any number of sandboxes in it.
//
// The two halves are split by what they are. The workspace's directory holds
// what belongs to the project — the resource limits, the workspace
// protections, the proxy's certificate authority, and the built environment,
// which is expensive and identical for everyone. An instance's directory holds
// what belongs to one line of work: its disk, and therefore its own edits and
// its own unapplied changes.
//
//	~/.john/sandboxes/billing-api-9f8e7d6c/
//	    ca/  protections.json  resources.json      the project's
//	    env-<envid>.img                            the environment, shared
//	    instances/1/state.img                      one line of work
//	    instances/2/state.img                      another
//
// The disks are .vhdx on Windows, where Hyper-V will attach nothing else; see
// diskExt.
//
// Most of the time there is one, and nothing about this is visible: john finds
// it, uses it, and picks up where the last session left off — including after
// the terminal has been closed and reopened, because an instance outlives the
// process that made it. Two terminals at once get two, because two agents
// doing different things must not share an upper layer. And a developer who
// comes back to more than one is asked which they meant, because guessing
// would silently strand the other one's work.

// instanceDir is one sandbox's own directory.
type instanceDir string

func (i instanceDir) path(name string) string { return filepath.Join(string(i), name) }

// instance is what a developer chooses between.
type instance struct {
	// ID is the short name, "1", "2" — an index rather than a hash, because
	// it exists to be typed.
	ID string
	// Dir is where it lives.
	Dir instanceDir
	// Busy reports whether a john is using it right now. A busy instance is
	// never offered: two processes over one disk image is the thing all of
	// this is arranged to prevent.
	Busy bool
	// LastUsed, Changes and Note are what the last session left behind, so a
	// developer choosing between two can tell them apart. Note is what that
	// session was working on — the first thing it was asked to do — because
	// "last used two days ago" identifies a sandbox and "refactor the core
	// interfaces" identifies the work.
	LastUsed time.Time
	Changes  int
	Note     string
}

// Describe is one line about a sandbox, for a developer choosing between them.
func (in instance) Describe() string {
	line := "last used " + humanSince(in.LastUsed)
	if in.Note != "" {
		line += ` ("` + truncateNote(in.Note, 48) + `")`
	}
	switch {
	case in.Changes == 1:
		line += " · 1 unapplied change"
	case in.Changes > 1:
		line += fmt.Sprintf(" · %d unapplied changes", in.Changes)
	}
	return line
}

// humanSince is a duration a person reads rather than parses.
func humanSince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func truncateNote(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max-1]) + "…"
}

// instanceSummary is what a session records on its way out. The upper layer is
// inside a filesystem image the host cannot read, so what a developer needs in
// order to choose has to be written down while the sandbox is still open.
type instanceSummary struct {
	LastUsed time.Time `json:"last_used"`
	Changes  int       `json:"changes"`
	Note     string    `json:"note,omitempty"`
}

func (s stateDir) instancesDir() string { return s.path("instances") }

// The workspace a sandbox directory belongs to, written down when it is made.
//
// The directory is named after the project, which is enough to recognise but
// not enough to act on: `john sandbox` has to say which path a sandbox backs,
// and it cannot work that out from a name and a hash. So the path is recorded
// beside it, and a sandbox whose workspace has since been deleted can be
// listed as exactly that.
func (s stateDir) setWorkspace(path string) {
	if _, err := os.Stat(s.path("workspace")); err == nil {
		return
	}
	if err := s.ensure(); err == nil {
		os.WriteFile(s.path("workspace"), []byte(path+"\n"), 0o600)
	}
}

func (s stateDir) workspace() string {
	raw, err := os.ReadFile(s.path("workspace"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// instances lists the sandboxes in this workspace, most recently used first.
func (s stateDir) instances() []instance {
	entries, err := os.ReadDir(s.instancesDir())
	if err != nil {
		return nil
	}
	var out []instance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := instanceDir(filepath.Join(s.instancesDir(), e.Name()))
		in := instance{ID: e.Name(), Dir: dir, Busy: locked(dir.path("lock"))}
		if raw, err := os.ReadFile(dir.path("summary.json")); err == nil {
			var sum instanceSummary
			if json.Unmarshal(raw, &sum) == nil {
				in.LastUsed, in.Changes, in.Note = sum.LastUsed, sum.Changes, sum.Note
			}
		}
		if in.LastUsed.IsZero() {
			if info, err := os.Stat(string(dir)); err == nil {
				in.LastUsed = info.ModTime()
			}
		}
		out = append(out, in)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].LastUsed.After(out[b].LastUsed) })
	return out
}

// free is the instances nothing is using.
func freeInstances(all []instance) []instance {
	var out []instance
	for _, in := range all {
		if !in.Busy {
			out = append(out, in)
		}
	}
	return out
}

// newInstance makes one. Names are indexes and are not reused while anything
// else exists, so "instance 2" means the same thing for as long as it is
// there.
func (s stateDir) newInstance() (instanceDir, error) {
	if err := os.MkdirAll(s.instancesDir(), 0o700); err != nil {
		return "", err
	}
	next := 1
	for _, in := range s.instances() {
		if n, err := strconv.Atoi(in.ID); err == nil && n >= next {
			next = n + 1
		}
	}
	dir := instanceDir(filepath.Join(s.instancesDir(), strconv.Itoa(next)))
	if err := os.MkdirAll(string(dir), 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// errChooseInstance means john will not decide which of this workspace's
// sandboxes to carry on with.
//
// It is not an error in the sense of something going wrong. It is the one
// question about a sandbox a developer has to answer, and it is asked exactly
// when the answer is not obvious: never when there are none, never when there
// is one and it is free, and always otherwise — including when the only
// sandbox here is one another terminal is using, because "john quietly made
// you a second sandbox" is a thing to be told rather than to discover later
// with the work split across two of them.
type errChooseInstance struct {
	Workdir string
	// Instances is every sandbox in this workspace, including the ones in
	// use — they are what makes the situation what it is, so they are shown
	// even though they cannot be picked.
	Instances []instance
}

func (e *errChooseInstance) Error() string {
	var b strings.Builder
	b.WriteString(e.Question() + "\n")
	for _, in := range e.Instances {
		fmt.Fprintf(&b, "  %s: %s\n", in.ID, in.Describe())
	}
	b.WriteString("  n: start a new sandbox, empty, with nothing carried over\n")
	b.WriteString("Choose one with -sandbox-instance <id>, or -sandbox-instance new.")
	return b.String()
}

// Question is the one line asked at the top of the dialog.
func (e *errChooseInstance) Question() string {
	free := len(freeInstances(e.Instances))
	switch {
	case free == 0:
		return "Every sandbox for this workspace is in use by another john."
	case len(e.Instances) == 1:
		return "You have a sandbox for this workspace. Which one do you want to use?"
	}
	return fmt.Sprintf("You have %d sandboxes for this workspace. Which one do you want to use?",
		len(e.Instances))
}

// Choices describes them, for a client to show.
func (e *errChooseInstance) Choices() []instance { return e.Instances }

// newInstanceName is what a developer says to mean "not any of those".
const newInstanceName = "new"

// resolveInstance picks the sandbox to use, or says it will not.
//
// Two cases are obvious and silent. Nothing here yet means make one — a first
// run should not begin with a question. Exactly one, and free, means that one:
// a developer who closes their terminal and opens it again gets the sandbox
// they were working in, with everything they had not applied still in it.
//
// Everything else asks. Not because john could not choose, but because every
// other case is one where choosing quietly is how work gets stranded: a second
// sandbox nobody knew was made, or the wrong one of two carried on with while
// the other is forgotten. The dialog always offers starting a new one, so that
// what would have happened silently is the thing being agreed to.
func (s stateDir) resolveInstance(workdir, want string) (instanceDir, error) {
	all := s.instances()
	if want == newInstanceName {
		return s.newInstance()
	}
	if want != "" {
		for _, in := range all {
			if in.ID != want {
				continue
			}
			if in.Busy {
				return "", fmt.Errorf("sandbox %s for %s is already in use by another john",
					want, workdir)
			}
			return in.Dir, nil
		}
		return "", fmt.Errorf("this workspace has no sandbox %q", want)
	}

	free := freeInstances(all)
	if len(all) == 0 {
		return s.newInstance()
	}
	if len(all) == 1 && len(free) == 1 {
		return free[0].Dir, nil
	}
	return "", &errChooseInstance{Workdir: workdir, Instances: all}
}

// claim takes an instance for this process. The lock is on an open file, so
// the operating system releases it when john exits however it exits — a
// sandbox is never stranded by a crash.
func (i instanceDir) claim(workdir string) (*os.File, error) {
	if err := os.MkdirAll(string(i), 0o700); err != nil {
		return nil, err
	}
	f, err := takeLock(i.path("lock"))
	if errors.Is(err, errSandboxBusy) {
		return nil, fmt.Errorf("sandbox: another john took the sandbox for %s while this one "+
			"was starting", workdir)
	}
	return f, err
}

// recordSummary writes what a developer needs in order to choose this sandbox
// later, while the sandbox is still open enough to be asked.
func (i instanceDir) recordSummary(changes int, note string) {
	raw, err := json.Marshal(instanceSummary{LastUsed: time.Now(), Changes: changes, Note: note})
	if err != nil {
		return
	}
	os.WriteFile(i.path("summary.json"), raw, 0o600)
}

// locked reports whether another process holds a lock file. It is only ever
// used to describe an instance, never to decide whether to take it — that is
// the lock's own job, and asking first would be a race.
func locked(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	f, err := takeLock(path)
	if err != nil {
		return true
	}
	releaseLock(f)
	return false
}

// NewInstance is what a caller asks for to mean "not any of those".
const NewInstance = newInstanceName

// InstanceChoice is the question a client has to put to the developer: which
// of this workspace's sandboxes to carry on with. It is exported because
// answering it is the client's job — john does not decide, and the answer is a
// choice made in the moment rather than a flag to start again with.
type InstanceChoice struct {
	// Question is the one line at the top.
	Question string
	// Instances is every sandbox here, in use ones included: they are why the
	// question is being asked even though they cannot be picked.
	Instances []InstanceInfo
}

// InstanceInfo is one sandbox, as a developer sees it.
type InstanceInfo struct {
	ID   string
	Busy bool
	// Describe is the line under the name: when it was last used, what that
	// session was doing, and what it is still holding.
	Describe string
}

// AsInstanceChoice reports whether an error is john asking which sandbox to
// use, and what to ask.
func AsInstanceChoice(err error) (InstanceChoice, bool) {
	var choose *errChooseInstance
	if !errors.As(err, &choose) {
		return InstanceChoice{}, false
	}
	out := InstanceChoice{Question: choose.Question()}
	for _, in := range choose.Instances {
		out.Instances = append(out.Instances, InstanceInfo{
			ID: in.ID, Busy: in.Busy, Describe: in.Describe(),
		})
	}
	return out, true
}
