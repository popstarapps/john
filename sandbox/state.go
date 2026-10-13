package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/popstarapps/john/policy"
)

// stateDir is a sandbox's own directory on the host: ~/.john/sandboxes/<key>.
// It holds what has to outlive the sandbox process — the resource limits a
// developer chose, and the workspace protections in force — and nothing the
// agent can reach. The sandbox's disks live here too, deliberately outside
// the working directory: they must not appear inside the workspace they back.
//
// Both implementations share this. The protections file in particular is read
// by `john apply`, which runs with no backend and no session, so what one
// implementation writes another must be able to read — a sandbox whose
// protections could only be understood by the code that wrote them would let
// a protected path through on the one path where nobody is watching.
type stateDir string

// defaultStateDir is where a workspace's sandbox lives when nothing overrides
// it.
func defaultStateDir(key string) (stateDir, error) {
	root, err := sandboxesRoot()
	if err != nil {
		return "", err
	}
	return stateDir(filepath.Join(root, key)), nil
}

func (s stateDir) path(name string) string { return filepath.Join(string(s), name) }

func (s stateDir) ensure() error { return os.MkdirAll(string(s), 0o700) }

// errSandboxBusy is what takeLock answers for a lock another process holds.
var errSandboxBusy = errors.New("busy")

// resources returns the limits this sandbox starts with, falling back to the
// defaults for a sandbox that has never been configured.
func (s stateDir) resources() Resources {
	data, err := os.ReadFile(s.path("resources.json"))
	if err != nil {
		return DefaultResources()
	}
	var r Resources
	if json.Unmarshal(data, &r) != nil {
		return DefaultResources()
	}
	return r
}

// setResources records limits for the next start. A running machine does not
// grow underneath the work inside it.
func (s stateDir) setResources(r Resources) error {
	if err := s.ensure(); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path("resources.json"), data, 0o600)
}

// protections returns what the last session installed, and whether there was
// anything to read. The agent cannot reach this file — it is on the host, and
// the host tree is never written by the sandbox — so what it guards against is
// not tampering but ignorance: `john apply` running later with no backend to
// ask.
func (s stateDir) protections() (policy.Protections, bool) {
	data, err := os.ReadFile(s.path("protections.json"))
	if err != nil {
		return policy.Protections{}, false
	}
	var p policy.Protections
	if json.Unmarshal(data, &p) != nil {
		return policy.Protections{}, false
	}
	return p, true
}

func (s stateDir) setProtections(p policy.Protections) error {
	if err := s.ensure(); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path("protections.json"), data, 0o600)
}
