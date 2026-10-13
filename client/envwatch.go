package client

import (
	"context"
	"strings"
	"time"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// The sandbox's environment is a file in the project, .john/Dockerfile, and
// a developer edits it while john is running — that is when they find out a
// tool is missing. So john watches .john/ and says, once per change, that the
// running sandbox was built from something else now, and that /rebuild brings
// it up to date without losing unapplied work. It does not rebuild by itself:
// a rebuild takes as long as the first build did, and an editor saves several
// times on the way to the version the developer meant.

// envPoll is how often .john/ is looked at. A change is acted on once it has
// read the same on two looks in a row, so an editor's burst of saves is one
// change and not several.
const envPoll = time.Second

// watchEnvironment runs for the life of the session.
func (a *app) watchEnvironment(ctx context.Context, vm *sandbox.VM) {
	t := time.NewTicker(envPoll)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		running, _ := vm.Environment()
		a.mu.Lock()
		busy := a.rebuilding
		a.mu.Unlock()
		if running == "" || busy {
			last = ""
			continue
		}
		_, now, _, err := sandbox.Environment(a.workdir)
		if err != nil {
			continue
		}
		if now != last {
			last = now
			continue
		}
		a.environmentIs(now, running)
	}
}

// environmentIs acts on the project's environment reading now while the
// sandbox runs on running: says so once when they differ, and clears the
// status line when they agree again.
func (a *app) environmentIs(now, running string) {
	a.mu.Lock()
	if now == running {
		changed := a.envChanged
		a.envChanged, a.envSeen = false, now
		a.mu.Unlock()
		if changed {
			a.status()
		}
		return
	}
	if now == a.envSeen {
		a.mu.Unlock()
		return
	}
	a.envSeen = now
	a.mu.Unlock()

	if err := sandbox.CheckEnvironment(a.workdir); err != nil {
		a.notice(terminalui.NoticeWarn, sandbox.EnvFile+" changed, and a rebuild would fail: "+
			err.Error()+" · john checks it again when it changes")
		return
	}
	a.mu.Lock()
	a.envChanged = true
	a.mu.Unlock()
	a.status()
	if _, _, custom, _ := sandbox.Environment(a.workdir); !custom {
		a.notice(terminalui.NoticeInfo, sandbox.EnvFile+" was removed · /rebuild moves the sandbox to the "+
			"built-in image now and keeps your unapplied work; otherwise that happens the next time john starts")
		return
	}
	a.notice(terminalui.NoticeInfo, sandbox.EnvFile+" changed · /rebuild applies it now and keeps your "+
		"unapplied work; otherwise it is used the next time john starts")
}

// touchesEnvironment reports whether applied paths include the sandbox's
// environment definition or its build context.
func touchesEnvironment(paths []string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, sandbox.EnvDir+"/") {
			return true
		}
	}
	return false
}
