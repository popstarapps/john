//go:build darwin && !cgo

package sandbox

import (
	"context"
	"errors"
)

// errNoCgo is the whole of what this build can say about the machine.
var errNoCgo = errors.New("sandbox: this macOS build of john was made without cgo, " +
	"so it does not carry Virtualization.framework and cannot run the sandbox")

// Virtualization.framework is an Objective-C API, so the machine needs cgo.
// A macOS build made with CGO_ENABLED=0 is a real build — the backend is
// cross-compiled that way — it just cannot run a sandbox, and saying so is
// better than not compiling.
type vz struct{}

func platformHypervisor() hypervisor { return &vz{} }

func (v *vz) Name() string { return "virtualization.framework" }

func (v *vz) Available() error { return errNoCgo }

func (v *vz) Boot(context.Context, bootConfig) (machine, error) { return nil, errNoCgo }
