package sandbox

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
)

// The agent that ships is `john __guest`, not a function called in-process.
// This runs that one: the real binary, started the way a machine starts it,
// with its configuration as a single JSON argument, talking back over a real
// socket. Everything else about the sandbox is tested through the in-process
// agent because it is faster; this is the test that the thing which will
// actually be inside the machine is wired up.
func TestTheAgentThatShipsRunsAsItsOwnProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the john binary")
	}
	bin := buildJohn(t)

	workdir := hostTree(t, map[string]string{
		"main.go": "package main\n",
		".env":    "SECRET=real\n",
	})
	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})

	vm, err := NewVM(workdir, VMOptions{
		StateDir:   t.TempDir(),
		Protect:    prot,
		Hypervisor: &loopback{AgentCmd: []string{bin, "__guest"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vm.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vm.Build(ctx, nil); err != nil {
		t.Fatalf("build: %v", err)
	}

	content, err := vm.Read(ctx, "main.go", 0)
	if err != nil || string(content.Data) != "package main\n" {
		t.Fatalf("read = %q %v", content.Data, err)
	}
	// A shell in the sandbox, where this host can give the agent a workspace
	// to run one in. Under the loopback machine that needs FUSE, so on macOS
	// or Windows this part is left to the real machine, which is Linux inside
	// and always has it.
	if mounted, why := vm.WorkspaceMounted(); mounted {
		res, err := vm.Exec(ctx, "cat main.go && ls -a", "", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(res.Stdout), "package main") {
			t.Fatalf("exec stdout = %q", res.Stdout)
		}
		if strings.Contains(string(res.Stdout), ".env") {
			t.Fatalf("an excluded file reached the shipped agent: %q", res.Stdout)
		}
	} else {
		t.Logf("no shell in the sandbox here: %s", why)
	}

	// And a refusal survives the process boundary as a refusal. Crossing the
	// link turns errors into text, and a workspace-protection refusal that
	// arrived on the host as a plain string would be reported to the model as
	// a broken sandbox instead of a rule it should stop retrying.
	_, err = vm.Read(ctx, ".env", 0)
	wantProtected(t, "read .env through the shipped agent", err, policy.ModeExclude)
}

func buildJohn(t *testing.T) string {
	t.Helper()
	// .exe on Windows, where a file without it is not something the operating
	// system will start — the build writes it happily and exec then reports a
	// binary that is not on the path, which is a confusing way to be told the
	// name is wrong.
	name := "john"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build john here: %v\n%s", err, out)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no binary built: %v", err)
	}
	return bin
}

// The forwarder carries no more than maxProxyStreams at once, and the rest
// wait their turn rather than being refused. The machine's socket device drops
// everything past a limit of its own — see maxProxyStreams — so a burst has to
// queue here, inside the sandbox, before it reaches it.
func TestTheForwarderQueuesABurstInsteadOfOpeningIt(t *testing.T) {
	var mu sync.Mutex
	open, peak := 0, 0
	release := make(chan struct{})
	f, err := startProxyForwarder("x", func(string) (net.Conn, error) {
		mu.Lock()
		open++
		peak = max(peak, open)
		mu.Unlock()
		inside, outside := net.Pipe()
		go func() {
			<-release
			outside.Write([]byte("ok"))
			outside.Close()
			mu.Lock()
			open--
			mu.Unlock()
		}()
		return inside, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const burst = maxProxyStreams + 50
	answered := make(chan error, burst)
	for range burst {
		go func() {
			c, err := net.Dial("tcp", f.Addr())
			if err != nil {
				answered <- err
				return
			}
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err = io.ReadAll(c)
			answered <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == maxProxyStreams || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	for range burst {
		if err := <-answered; err != nil {
			t.Fatalf("a queued connection was not carried: %v", err)
		}
	}
	if peak != maxProxyStreams {
		t.Fatalf("%d streams were open at once; the cap is %d", peak, maxProxyStreams)
	}
}

// A finished request gives its slot back. The proxy keeps a connection open
// between requests, as keep-alive does, until it reads the end of it, so the
// forwarder has to pass on the client's hang-up; when it did not, every
// request kept its slot and the forwarder stopped after maxProxyStreams.
func TestTheForwarderFreesASlotWhenTheClientHangsUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
					c.Write([]byte("ok"))
				}
			}()
		}
	}()
	f, err := startProxyForwarder("x", func(string) (net.Conn, error) {
		return net.Dial("tcp", ln.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for i := range maxProxyStreams + 20 {
		c, err := net.Dial("tcp", f.Addr())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 2)
		if _, err := c.Write([]byte("GET")); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("request %d was not carried: %v", i+1, err)
		}
		c.Close()
	}
}
