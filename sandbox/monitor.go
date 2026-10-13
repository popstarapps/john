package sandbox

// The machine's host process.
//
// On Linux the sandbox's virtual machine runs in a process of its own, because
// libkrun's entry point takes over the process it is called on and does not
// return — see machine_krun.go. john starts a second copy of itself to be that
// process, and this is what it runs there.
//
// It exists on every platform, the way GrantAdminAccess does, so that
// `john __machine` is one command with one behaviour rather than a build tag
// in main. Nobody types it: the sandbox starts it, with the machine's
// configuration as a single argument.
func RunMachineMonitor(args []string) error { return runMachineMonitor(args) }
