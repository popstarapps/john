package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// loopback runs the guest agent as an ordinary process on the host instead of
// inside a virtual machine. It isolates nothing, and it is not a sandbox.
//
// It exists for two reasons. Everything above the hypervisor — the union, the
// live lower layer, the protections, the diff, the apply path, the whole
// control protocol — is the same code on every platform, and this machine
// lets all of it be tested and developed anywhere, including on a build
// machine with no hardware virtualization. And when john is being worked on,
// it is the difference between a five-second edit-run loop and a VM boot.
//
// Because it is not a sandbox, it is never chosen automatically. It has to be
// asked for by name, and asking for it says so on the terminal.
type loopback struct {
	// AgentCmd runs the guest agent as a subprocess. Empty runs it in this
	// process instead, which is what the tests do: no build step, and a
	// failure inside the agent surfaces as a Go stack rather than an exit
	// code.
	AgentCmd []string
}

func (l *loopback) Name() string { return "loopback" }

func (l *loopback) Available() error { return nil }

func (l *loopback) Boot(ctx context.Context, cfg bootConfig) (machine, error) {
	root := filepath.Join(cfg.Dir, "guestroot")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}

	// The two streams. Unix sockets rather than an in-process pipe, so the
	// framing, the buffering and the concurrency are the same code the
	// virtual machines run over vsock.
	sockDir, err := os.MkdirTemp("", "john-lb-")
	if err != nil {
		return nil, err
	}
	ctrlAddr := filepath.Join(sockDir, "c.sock")
	lowerAddr := filepath.Join(sockDir, "l.sock")
	ctrlLn, err := net.Listen("unix", ctrlAddr)
	if err != nil {
		os.RemoveAll(sockDir)
		return nil, err
	}
	lowerLn, err := net.Listen("unix", lowerAddr)
	if err != nil {
		ctrlLn.Close()
		os.RemoveAll(sockDir)
		return nil, err
	}

	m := &loopbackMachine{root: root, sockDir: sockDir, proxySocket: cfg.ProxySocket}
	guestCfg := GuestConfig{
		Root:           root,
		Mount:          filepath.Join(root, filepath.FromSlash(cfg.Mount)),
		Upper:          filepath.Join(root, filepath.FromSlash(cfg.Upper)),
		Control:        ctrlAddr,
		Lower:          lowerAddr,
		MaxOutputBytes: cfg.MaxOutputBytes,
	}

	if err := l.start(ctx, m, guestCfg, cfg); err != nil {
		ctrlLn.Close()
		lowerLn.Close()
		m.Stop(context.Background())
		return nil, err
	}

	// The guest dials the lower stream first and the control stream second,
	// so accepting in that order is accepting them in the order they arrive.
	m.lower, err = acceptWithin(lowerLn, 30*time.Second)
	lowerLn.Close()
	if err != nil {
		ctrlLn.Close()
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: guest agent did not connect: %w", err)
	}
	m.control, err = acceptWithin(ctrlLn, 30*time.Second)
	ctrlLn.Close()
	if err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: guest agent did not connect: %w", err)
	}
	return m, nil
}

// start launches the agent, in this process or beside it.
func (l *loopback) start(ctx context.Context, m *loopbackMachine, guestCfg GuestConfig, cfg bootConfig) error {
	if len(l.AgentCmd) == 0 {
		agentCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		m.cancel = cancel
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.err = RunGuest(agentCtx, guestCfg, func(addr string) (net.Conn, error) {
				return net.Dial("unix", addr)
			})
		}()
		return nil
	}
	raw, err := jsonMarshal(guestCfg)
	if err != nil {
		return err
	}
	c := exec.Command(l.AgentCmd[0], append(append([]string{}, l.AgentCmd[1:]...), string(raw))...)
	c.Env = append(os.Environ(), cfg.Env...)
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	m.proc = c
	return nil
}

// acceptWithin waits for the guest, rather than forever. A machine that never
// dials back is a failure to report, not a session that hangs.
func acceptWithin(ln net.Listener, d time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-time.After(d):
		return nil, fmt.Errorf("timed out after %s", d)
	}
}

type loopbackMachine struct {
	root        string
	sockDir     string
	proxySocket string
	control     net.Conn
	lower       net.Conn

	cancel context.CancelFunc
	wg     sync.WaitGroup
	err    error
	proc   *exec.Cmd
	once   sync.Once
}

func (m *loopbackMachine) Control() net.Conn { return m.control }
func (m *loopbackMachine) Lower() net.Conn   { return m.lower }
func (m *loopbackMachine) Root() string      { return m.root }

// The agent is a process on the host here, so the proxy's socket is a path it
// can already open. There is nothing to forward and nothing to wire: the
// address the guest link understands for a Unix socket is the socket.
func (m *loopbackMachine) ProxyAddr() string { return m.proxySocket }

func (m *loopbackMachine) Stop(ctx context.Context) error {
	m.once.Do(func() {
		// Closing control ends the agent's serve loop, which unmounts on its
		// way out. Cancelling without closing would leave the mount behind.
		if m.control != nil {
			m.control.Close()
		}
		if m.lower != nil {
			m.lower.Close()
		}
		if m.cancel != nil {
			m.cancel()
		}
		if m.proc != nil && m.proc.Process != nil {
			// Give the agent the chance to unmount. Closing the control
			// stream ends its serve loop, and unmounting is what it does on
			// the way out; killing it first would leave the mount behind and
			// the directory underneath unremovable.
			done := make(chan struct{})
			go func() { m.proc.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				m.proc.Process.Kill()
				<-done
			}
		}
		m.wg.Wait()
		os.RemoveAll(m.sockDir)
	})
	return nil
}

// NewLoopbackVM is the VM sandbox running its agent as a host process. It
// isolates nothing; it exists so that john can be developed and tested on a
// machine with no hypervisor, and every caller of it says so out loud.
func NewLoopbackVM(workdir string) (*VM, error) {
	return NewVM(workdir, VMOptions{Hypervisor: &loopback{}})
}
