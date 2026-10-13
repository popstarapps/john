package client

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/popstarapps/john/sandbox"
)

// `john sandbox` — what is on this machine, and how to get rid of it.
//
// A sandbox outlives the session that made it, which is the point: a developer
// closes their terminal and opens it again and their unapplied work is still
// there. The cost of that is disks accumulating in a directory nobody looks
// at, so there has to be a way to look at it. None of this starts a machine —
// asking what is on your disk should not cost four virtual machine boots.
func Sandbox(args []string) int {
	// The verb comes first and its flags belong to it, so this reads the verb
	// itself rather than handing everything to one flag set. `john sandbox
	// --all` is the listing with a flag, not an unknown command.
	verb, rest := "list", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, rest = args[0], args[1:]
	}
	switch verb {
	case "list", "ls":
		return sandboxList(rest)
	case "rm", "remove":
		return sandboxRemove(rest)
	case "prune":
		return sandboxPrune(rest)
	case "rebuild":
		return sandboxRebuild(rest)
	case "help", "-h", "--help":
		sandboxUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "john sandbox: unknown command %q\n\n", verb)
		sandboxUsage(os.Stderr)
		return 2
	}
}

// hoistFlags moves flags in front of the arguments they were typed after.
//
// Go's flag package stops at the first thing that is not a flag, so `john
// sandbox rm 2 --force` would silently ignore --force — which on that command
// is the difference between refusing to discard a developer's unapplied work
// and discarding it. Flags after arguments are what people type; this makes
// them mean what they look like.
//
// It has to ask the flag set what it is looking at, because a flag that takes
// a value owns the argument after it: in `login host --username bob`, "bob"
// is not a second positional and moving the flag without it makes "--username"
// swallow whatever followed. Boolean flags own nothing, which is why a simpler
// version of this worked for as long as every flag here was one.
func hoistFlags(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Everything after it is an argument by definition.
			rest = append(rest, args[i+1:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // --flag=value carries its own
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, rest...)
}

// isBoolFlag is how the flag package itself tells the two apart.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func sandboxUsage(w *os.File) {
	fmt.Fprint(w, `usage: john sandbox [command]

  john sandbox               the sandboxes for this project
  john sandbox --all         every sandbox on this machine
  john sandbox rm <id>       remove one (--force if it holds unapplied work)
  john sandbox prune         reclaim dormant sandboxes
  john sandbox rebuild       rebuild this project's environment, keeping your work

A sandbox outlives the session that made it, so that closing your terminal does
not lose what you had not applied yet. Two terminals in one project get two
sandboxes; john asks which to carry on with when the answer is not obvious.
`)
}

func sandboxList(args []string) int {
	fs := flag.NewFlagSet("john sandbox list", flag.ContinueOnError)
	all := fs.Bool("all", false, "every sandbox on this machine, not just this project's")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return 2
	}

	workdir := ""
	if !*all {
		workdir, _ = os.Getwd()
	}
	spaces, err := sandbox.Sandboxes(workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john sandbox:", err)
		return 1
	}
	if len(spaces) == 0 {
		if *all {
			fmt.Println("no sandboxes on this machine.")
		} else {
			fmt.Println("no sandbox for this project yet — `john` makes one.")
		}
		return 0
	}

	var total int64
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, w := range spaces {
		total += w.Size()
		name := w.Path
		if name == "" {
			name = w.Dir + " (workspace unrecorded)"
		} else if w.Gone {
			name += "  (this directory no longer exists)"
		}
		if *all || len(spaces) > 1 {
			fmt.Fprintf(tw, "\n%s\n", displayPath(name))
		}
		if w.Environment > 0 {
			fmt.Fprintf(tw, "  environment\t%s\tshared by all of them\n",
				sandbox.FormatSize(w.Environment))
		}
		for _, s := range w.Sandboxes {
			state := ""
			if s.Running {
				state = "  (running)"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s%s\n", s.ID, sandbox.FormatSize(s.Size), s.Describe(), state)
		}
		if len(w.Sandboxes) == 0 {
			fmt.Fprintf(tw, "  (no sandboxes)\t\t\n")
		}
	}
	tw.Flush()
	fmt.Printf("\n%s in total. `john sandbox prune` reclaims what is dormant.\n",
		sandbox.FormatSize(total))
	return 0
}

func sandboxRemove(args []string) int {
	fs := flag.NewFlagSet("john sandbox rm", flag.ContinueOnError)
	force := fs.Bool("force", false, "remove it even though it holds work you never applied")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: john sandbox rm <id> — `john sandbox` lists them")
		return 2
	}
	workdir, _ := os.Getwd()
	size, err := sandbox.RemoveSandbox(workdir, fs.Arg(0), *force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john sandbox rm:", err)
		if !*force && strings.Contains(err.Error(), "never applied") {
			fmt.Fprintln(os.Stderr, "  `john apply` writes them to your working directory first,")
			fmt.Fprintln(os.Stderr, "  or `john sandbox rm --force` discards them.")
		}
		return 1
	}
	fmt.Printf("removed sandbox %s (%s reclaimed)\n", fs.Arg(0), sandbox.FormatSize(size))
	return 0
}

func sandboxPrune(args []string) int {
	fs := flag.NewFlagSet("john sandbox prune", flag.ContinueOnError)
	olderThan := fs.String("older-than", "30d", "reclaim sandboxes unused for longer than this")
	gone := fs.Bool("gone", false, "only sandboxes whose working directory no longer exists")
	withChanges := fs.Bool("with-changes", false, "include sandboxes holding work you never applied")
	dry := fs.Bool("dry-run", false, "say what would go without removing anything")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return 2
	}
	age, err := parseAge(*olderThan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john sandbox prune:", err)
		return 2
	}

	taken, err := sandbox.Prune(sandbox.PruneOptions{
		OlderThan: age, GoneOnly: *gone, WithChanges: *withChanges, DryRun: *dry,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "john sandbox prune:", err)
		return 1
	}
	if len(taken) == 0 {
		fmt.Println("nothing to reclaim.")
		return 0
	}
	var total int64
	for _, p := range taken {
		total += p.Size
		verb := "removed"
		if *dry {
			verb = "would remove"
		}
		fmt.Printf("%s %s/%s (%s) — %s\n", verb, displayPath(p.Workspace), p.ID,
			sandbox.FormatSize(p.Size), p.Reason)
	}
	if *dry {
		fmt.Printf("\n%s would be reclaimed.\n", sandbox.FormatSize(total))
	} else {
		fmt.Printf("\n%s reclaimed.\n", sandbox.FormatSize(total))
	}
	return 0
}

func sandboxRebuild(args []string) int {
	fs := flag.NewFlagSet("john sandbox rebuild", flag.ContinueOnError)
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return 2
	}
	workdir, _ := os.Getwd()
	if err := sandbox.RebuildEnvironment(workdir); err != nil {
		fmt.Fprintln(os.Stderr, "john sandbox rebuild:", err)
		return 1
	}
	file, _, custom, _ := sandbox.Environment(workdir)
	from := "the built-in image"
	if custom {
		from = file
	}
	fmt.Printf("the environment will be built again from %s on the next start.\n", from)
	fmt.Println("your sandboxes keep everything they had not applied.")
	return 0
}

// parseAge reads "30d", "12h", "90m" — the durations a person types about
// sandboxes, which Go's own parser does not accept because it has no day.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var days float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &days); err != nil {
			return 0, fmt.Errorf("%q is not an age like 30d, 12h or 90m", s)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not an age like 30d, 12h or 90m", s)
	}
	return d, nil
}
