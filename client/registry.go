package client

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/popstarapps/john/keychain"
	"github.com/popstarapps/john/registryauth"
	"github.com/popstarapps/john/sandbox"
)

// `john registry` — how a private base image gets pulled.
//
// A project's environment starts from an image, and an organization's image is
// usually in an organization's registry. john pulls it on the HOST, because
// that is where the proxy is, so signing in is a thing this machine does and
// not something an administrator configures centrally: the developer who can
// pull the image already has the credential, and a second copy of it in a
// console would only go stale.
//
// Most people will never type any of this. john reads ~/.docker/config.json
// and the credential helpers it names first, so a machine that can already
// `docker pull` the image can already build the environment. This is for the
// machine that has no docker on it — which, now that john builds the image
// itself, is most of them.
func Registry(args []string) int {
	verb, rest := "list", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, rest = args[0], args[1:]
	}
	switch verb {
	case "login":
		return registryLogin(rest)
	case "logout":
		return registryLogout(rest)
	case "list", "ls":
		return registryList(rest)
	case "help", "-h", "--help":
		registryUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "john registry: unknown command %q\n\n", verb)
		registryUsage(os.Stderr)
		return 2
	}
}

func registryUsage(w *os.File) {
	fmt.Fprint(w, `usage: john registry [command]

  john registry                     the registries this machine can pull from
  john registry login <registry>    sign in to one, and remember it
  john registry logout <registry>   forget one

john reads ~/.docker/config.json and its credential helpers first, so a
registry you have already signed in to with docker needs nothing here. Sign in
with this when there is no docker on the machine.

The credential is used on this machine, to pull your environment's base image.
It never enters the sandbox: the machine is handed the image's layers, not the
password that fetched them.
`)
}

// registryLogin takes a credential, proves it works, and only then keeps it.
//
// Proving it first is the whole point of the command. A password that is wrong
// by one character is otherwise found by a build, days later, in a message
// about a registry — and the person who typed it is not the person who reads
// that message.
func registryLogin(args []string) int {
	fs := flag.NewFlagSet("john registry login", flag.ContinueOnError)
	username := fs.String("username", "", "the account to sign in as")
	fromStdin := fs.Bool("password-stdin", false, "read the password from standard input")
	if err := fs.Parse(hoistFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: john registry login <registry>")
		return 2
	}
	host := fs.Arg(0)

	cred, err := askForCredential(*username, *fromStdin, os.Stdin, stdinIsATerminal())
	if err != nil {
		fmt.Fprintln(os.Stderr, "john registry:", err)
		return 1
	}

	fmt.Printf("checking %s… ", host)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := sandbox.CheckRegistryCredential(ctx, host, sandbox.RegistryCredential{
		Username: cred.Username, Secret: cred.Secret, Identity: cred.Identity,
	}); err != nil {
		fmt.Println()
		fmt.Fprintln(os.Stderr, "john registry:", strings.TrimPrefix(err.Error(), "sandbox: "))
		return 1
	}
	fmt.Println("ok")

	if err := registryauth.Save(host, cred); err != nil {
		fmt.Fprintln(os.Stderr, "john registry:", err)
		return 1
	}
	who := cred.Username
	if cred.Identity {
		who = "a token"
	}
	fmt.Printf("signed in to %s as %s, kept in %s\n", host, who, credentialHome())
	return 0
}

// credentialHome names where the secret just went, because those are two
// different places and a developer clearing one out should be told which.
func credentialHome() string {
	if name := keychain.Name(); name != "" {
		return name
	}
	path, _ := registryauth.StorePath()
	return displayPath(path)
}

// askForCredential reads the two halves, from wherever this run has them.
//
// A password typed at a terminal is never echoed, and a machine with no
// terminal is told what to pipe rather than shown a prompt nobody can answer —
// the same rule the first-run question follows, for the same reason: a script
// that hangs on an invisible prompt is worse than one that fails.
func askForCredential(username string, fromStdin bool, in *os.File,
	interactive bool) (registryauth.Credential, error) {
	if fromStdin {
		if username == "" {
			return registryauth.Credential{}, fmt.Errorf(
				"--password-stdin needs --username as well")
		}
		secret, err := io.ReadAll(io.LimitReader(in, 1<<20))
		if err != nil {
			return registryauth.Credential{}, err
		}
		s := strings.TrimRight(string(secret), "\r\n")
		if s == "" {
			return registryauth.Credential{}, fmt.Errorf("nothing arrived on standard input")
		}
		return registryauth.Credential{Username: username, Secret: s}, nil
	}
	if !interactive {
		return registryauth.Credential{}, fmt.Errorf(
			"there is no terminal to ask on — pass --username and pipe the password to " +
				"--password-stdin")
	}
	if username == "" {
		fmt.Print("username: ")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			return registryauth.Credential{}, err
		}
		username = strings.TrimSpace(line)
		if username == "" {
			return registryauth.Credential{}, fmt.Errorf("no username")
		}
	}
	fmt.Print("password: ")
	secret, err := term.ReadPassword(int(in.Fd()))
	fmt.Println()
	if err != nil {
		return registryauth.Credential{}, err
	}
	if len(secret) == 0 {
		return registryauth.Credential{}, fmt.Errorf("no password")
	}
	return registryauth.Credential{Username: username, Secret: string(secret)}, nil
}

// registryLogout forgets what john stored, and explains itself when there was
// nothing of john's to forget but the registry works anyway — which is what a
// machine with a docker credential looks like, and would otherwise read as a
// logout that did not take.
func registryLogout(args []string) int {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: john registry logout <registry>")
		return 2
	}
	host := args[0]
	removed, err := registryauth.Delete(host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john registry:", err)
		return 1
	}
	if removed {
		fmt.Printf("forgotten: %s\n", host)
		return 0
	}
	found, err := registryauth.Lookup(host)
	if err == nil && !found.Empty() {
		fmt.Printf("john had nothing stored for %s — it is signed in through %s, which john "+
			"reads and does not change.\n", host, found.From)
		return 0
	}
	fmt.Printf("nothing stored for %s.\n", host)
	return 0
}

func registryList(args []string) int {
	fs := flag.NewFlagSet("john registry list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	listed, err := registryauth.Configured()
	if err != nil {
		fmt.Fprintln(os.Stderr, "john registry:", err)
		return 1
	}
	if len(listed) == 0 {
		fmt.Println("no registry credentials on this machine. Public images need none;")
		fmt.Println("`john registry login <registry>` adds one.")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REGISTRY\tACCOUNT\tFROM")
	for _, l := range listed {
		who := l.Username
		if who == "" {
			who = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", l.Host, who, l.From)
	}
	tw.Flush()
	return 0
}

// registryCredentials is what the sandbox asks when a base image needs a
// sign-in. It is the client's job rather than the sandbox's because the
// credential is the developer's: it is read here, on the host, and what
// crosses into the machine is the image, never the password.
func registryCredentials(host string) (sandbox.RegistryCredential, error) {
	c, err := registryauth.Resolve(host)
	if err != nil {
		return sandbox.RegistryCredential{}, err
	}
	return sandbox.RegistryCredential{
		Username: c.Username, Secret: c.Secret, Identity: c.Identity,
	}, nil
}
