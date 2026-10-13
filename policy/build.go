package policy

// Where an image build's network traffic goes.
//
// Building the sandbox environment is the one thing in john that has a reason
// to want the network wide open: it is how a project gets its compilers, its
// language runtimes and its internal packages installed, and it is what makes
// the sandbox useful afterwards under a policy that allows no egress at all.
// A build that could not reach a package index would leave the agent with
// nothing to work with in exactly the organizations that restrict the most.
//
// So the default is direct. But it is a decision an organization should be
// able to make, not one john makes for it — a build is still a program the
// project's own repository describes, and an organization that wants every
// byte its machines fetch to pass the same policy as everything else should
// be able to say so.
const (
	// BuildDirect gives the build unrestricted network access. The default.
	BuildDirect = "direct"
	// BuildProxied puts the build behind the proxy under the network policies
	// that govern everything else, so a build cannot fetch what a session
	// could not.
	BuildProxied = "proxied"
)

// BuildNetwork is what the snapshot says about image builds.
//
// The unset value means direct: a backend that predates this field is not
// making a claim about builds, and the behaviour it shipped with is the one
// its administrators are expecting. A value this build does not recognise
// means proxied, which is the other direction on purpose — an instruction we
// cannot carry out is not a licence to open the network, and the rest of the
// policy engine fails closed for the same reason. That includes "observed",
// a setting that was offered once and never did anything but this.
func (s *Snapshot) BuildNetwork() string {
	switch s.Build {
	case "":
		return BuildDirect
	case BuildDirect, BuildProxied:
		return s.Build
	default:
		return BuildProxied
	}
}

// BuildIsProxied reports whether the build is held to the network policies.
func (s *Snapshot) BuildIsProxied() bool {
	return s.BuildNetwork() == BuildProxied
}
