package sandbox

import "errors"

// Sometimes a machine cannot start a sandbox for a reason that is one
// administrative step away from being fixed, and the step is small, specific
// and permanent. Windows is the case that exists: creating a virtual machine
// needs a privilege an ordinary developer's token does not carry, and the fix
// is a single change to which local group they are in.
//
// A message telling them to run a command is not good enough for that. It is
// the first thing john does on a new machine, it is the difference between a
// product that works and one that does not, and the developer is sitting at a
// terminal that can ask. So the sandbox describes the step and how to carry it
// out, and the client puts it to them as a question.
//
// It is deliberately narrow. An AdminSetup is offered only for a failure john
// can name exactly, it does exactly the one thing it says, and nothing it does
// leaves a process running with rights it did not start with — the elevated
// half is `john __sandbox-admin`, which changes one group membership and
// exits. A machine that needs a Windows feature turned on and a reboot is NOT
// one of these: that is a larger change to somebody's computer than a
// yes/no question should carry, so it stays a message that says what to run.
type AdminSetup struct {
	// Reason is why the sandbox cannot start, in the words Available used.
	Reason string
	// Question is what to put to the developer, phrased for a yes.
	Question string
	// Detail is what accepting does and what it costs, a line at a time.
	Detail []string
	// Done is what to tell them once it has worked — which, for a change to a
	// group membership, includes the part where it does not take effect until
	// they sign in again.
	Done string

	// Take carries the step out. It asks the operating system for
	// administrator rights, so it puts a consent prompt on the developer's
	// screen and does not return until they have answered it; a developer who
	// answers no gets ErrAdminDeclined, which is an answer rather than a
	// failure.
	//
	// It is a field and not a method because the four lines above are the
	// whole of what the client needs to put the question, and a client test
	// then needs no machine that has anything to grant.
	Take func() error
}

// AdminSetupFor is the step that would fix err, or nil when there is not one —
// which is the answer on every platform but Windows, and the answer on Windows
// for every reason but this one.
func AdminSetupFor(err error) *AdminSetup {
	if err == nil {
		return nil
	}
	return adminSetupFor(err)
}

// ErrAdminDeclined means the developer said no to the operating system's
// consent prompt. It is not a failure — they were asked and they answered —
// so the client says nothing more about it than that.
var ErrAdminDeclined = errors.New("sandbox: the request for administrator rights was declined")
