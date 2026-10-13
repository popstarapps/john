package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/store"
)

// standaloneOptions are what the `john` entrypoint passes through when this
// machine is set up to run solo. Standalone is no longer a command of its
// own — `john login standalone` chooses it once, and plain `john` runs it —
// so the flags come from there rather than being parsed again here.
type standaloneOptions struct {
	Listen  string
	Data    string
	Plain   bool
	ASCII   bool
	Sandbox string
	// SandboxInstance names which of this workspace's sandboxes to use, when
	// there is more than one and john would otherwise not know.
	SandboxInstance string
	Spinner         string
	Effect          string
}

// standalone runs john solo: the full backend in this process on loopback,
// the embedded identity provider signing in the local user, and then the
// ordinary client against it — the same
// WebSocket, the same protocol, the same sandbox as an organization
// deployment. It returns the process exit code.
func standalone(o standaloneOptions) int {
	srv, token, email, err := startStandalone(o.Listen, o.Data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	defer srv.Close()

	// A session needs a model; the console, which is where one is added, does
	// not, so this is checked here rather than in startStandalone. With someone
	// at the terminal, the console is opened on the dialog and the session
	// starts once a model is there; a script is told what to run instead.
	m, err := firstEnabledModel(srv.Store())
	if err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	if m == nil {
		if !stdinIsATerminal() {
			fmt.Fprintln(os.Stderr, "john: no models yet — run `john console` to add one")
			return 1
		}
		if !addModelInConsole(srv) {
			fmt.Println("\nNo model yet. Run `john` again when you're ready.")
			return 1
		}
	}

	url := "http://" + srv.Addr()
	fmt.Printf("standalone backend on %s · console at %s\n", srv.Addr(), url)

	opts := Options{
		BackendURL: url,
		Token:      token,
		Email:      email,
		Standalone: true,
		Version:    buildVersion,
		Plain:      o.Plain,
		ASCII:      o.ASCII,
	}
	opts.BusySpinner, opts.BusyEffect = o.Spinner, o.Effect
	opts.Workdir, _ = os.Getwd()
	// The same resolver the ordinary session uses, so -sandbox means the same
	// thing in standalone mode.
	sb, err := namedSandbox(o.Sandbox, opts.Workdir, o.SandboxInstance)
	if err != nil {
		if err != errSignInAgain {
			fmt.Fprintln(os.Stderr, "john:", err)
		}
		return 1
	}
	opts.Sandbox = sb
	return Session(opts)
}

const standaloneListen = "127.0.0.1:7171"

// startStandalone brings up the standalone backend in this process and signs
// the local user in to it. The caller closes srv.
func startStandalone(listen, dataDir string) (srv *backend.Server, token, email string, err error) {
	if listen == "" {
		listen = standaloneListen
	}
	if dataDir == "" {
		if dataDir, err = standaloneDataDir(); err != nil {
			return nil, "", "", err
		}
	}

	// Its identity provider signs in whoever asks, so it listens on loopback
	// and nowhere else, and answers only requests addressed to this machine.
	if h, _, err := net.SplitHostPort(listen); err != nil || !net.ParseIP(h).IsLoopback() {
		return nil, "", "", fmt.Errorf("standalone listens on a loopback address only, not %q", listen)
	}
	identity := localIdentity()
	srv, err = backend.New(backend.Config{
		DBPath:     filepath.Join(dataDir, "john.db"),
		Listen:     listen,
		Version:    buildVersion,
		DevIdP:     &identity,
		Standalone: true,
	})
	if err != nil {
		// One standalone backend per machine: a second would share its
		// database and its port. Say which one is in the way.
		if standaloneRunning(listen) {
			return nil, "", "", fmt.Errorf("john standalone is already running on %s, in another `john` "+
				"or `john console`; close that one first", listen)
		}
		return nil, "", "", err
	}
	go srv.Serve(context.Background())

	token, email, err = bootstrapStandalone(srv, dataDir, identity)
	if err != nil {
		srv.Close()
		return nil, "", "", err
	}
	return srv, token, email, nil
}

// standaloneRunning reports whether a standalone john is answering on listen.
// Only standalone serves the embedded identity provider, so its discovery
// document tells it apart from anything else that took the port.
func standaloneRunning(listen string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + listen + "/devidp/.well-known/openid-configuration")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// standaloneConsole is `john console` on a standalone machine. The backend
// lives only as long as the process running it, so when no `john` is running
// this one serves the console itself until interrupted: the console is where
// models and policies are set up, and needing an agent session open to reach
// it would be backwards.
func standaloneConsole() int {
	// With no model yet, the console opens where one is added.
	next := "/"
	if has, err := standaloneHasModel(); err == nil && !has {
		next = modelsPage
	}
	if standaloneRunning(standaloneListen) {
		fmt.Printf("opening http://%s\n", standaloneListen)
		openBrowser(consoleLink(standaloneListen, next))
		return 0
	}
	srv, _, _, err := startStandalone("", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "john console:", err)
		return 1
	}
	defer srv.Close()

	fmt.Printf("console at http://%s · Ctrl-C to stop\n", srv.Addr())
	openBrowser(consoleLink(srv.Addr(), next))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	return 0
}

// loginStandaloneModel is the rest of `john login standalone`: a machine with
// no model yet is walked to the console to add one, so that the `john` after
// it starts a session rather than stopping to ask.
func loginStandaloneModel() int {
	has, err := standaloneHasModel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "john login:", err)
		return 1
	}
	switch {
	case has:
		fmt.Println("Run `john` from a project directory to start.")
		return 0
	case !stdinIsATerminal():
		fmt.Println("Add a model with `john console`, then run `john` from a project directory.")
		return 0
	case standaloneRunning(standaloneListen):
		fmt.Printf("john standalone is already running. Add a model in its console:\n  http://%s%s\n",
			standaloneListen, modelsPath)
		openBrowser(consoleLink(standaloneListen, modelsPage))
		return 0
	}

	srv, _, _, err := startStandalone("", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "john login:", err)
		return 1
	}
	defer srv.Close()
	if !addModelInConsole(srv) {
		fmt.Println("\nNo model yet. `john console` opens the console when you're ready.")
		return 1
	}
	fmt.Println("Run `john` from a project directory to start.")
	fmt.Println("The console closes with this command; `john console` reopens it.")
	return 0
}

const (
	modelsPath = "/admin/models"
	modelsPage = modelsPath + "?add=1" // opens the add-model dialog
)

// addModelInConsole opens the console on the add-model dialog and waits until
// an enabled model exists. It reports false when interrupted first.
func addModelInConsole(srv *backend.Server) bool {
	fmt.Printf("john needs a model to work with. Opening the console to add one:\n"+
		"  http://%s%s\n"+
		"Waiting for a model… (Ctrl-C to stop)\n", srv.Addr(), modelsPath)
	openBrowser(consoleLink(srv.Addr(), modelsPage))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	m, err := waitForModel(ctx, srv.Store(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	fmt.Printf("✓ %s added\n", m.Name)
	return true
}

// waitForModel polls until an enabled model exists and returns it.
func waitForModel(ctx context.Context, st *store.Store, every time.Duration) (*store.Model, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if m, err := firstEnabledModel(st); err != nil || m != nil {
			return m, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// consoleLink goes through the sign-in, which standalone's identity provider
// completes without asking, so the browser lands on next already signed in
// rather than on a sign-in page with nothing to choose.
func consoleLink(addr, next string) string {
	return "http://" + addr + "/auth/login?next=" + url.QueryEscape(next)
}

// standaloneDataDir is where standalone keeps its database: beside the setup
// that chose standalone, so JOHN_HOME moves both.
func standaloneDataDir() (string, error) {
	home := os.Getenv("JOHN_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".john")
	}
	return filepath.Join(home, "standalone"), nil
}

// standaloneHasModel looks in the database without starting a backend, so it
// works beside a running one and costs nothing on a machine that has none.
func standaloneHasModel() (bool, error) {
	dir, err := standaloneDataDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, "john.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	st, err := store.Open(path)
	if err != nil {
		return false, err
	}
	defer st.Close()
	m, err := firstEnabledModel(st)
	return m != nil, err
}

func localIdentity() devidp.Identity {
	name, email := "developer", "developer@localhost"
	if u, err := user.Current(); err == nil {
		if u.Name != "" {
			name = u.Name
		} else if u.Username != "" {
			name = u.Username
		}
		// Windows names the account DOMAIN\user, and an address with the
		// computer's name and a backslash in it is no address.
		if login := u.Username[strings.LastIndex(u.Username, `\`)+1:]; login != "" {
			email = login + "@localhost"
		}
	}
	return devidp.Identity{Subject: "standalone-" + email, Email: email, Name: name}
}

// bootstrapStandalone makes the backend usable with nobody to run --setup:
// the org exists, the issuer points at the embedded IdP, a trace key is
// generated (identity kept beside the store), and the local user holds a
// machine token. Models are the developer's to add, in the console.
func bootstrapStandalone(srv *backend.Server, dataDir string, id devidp.Identity) (token, email string, err error) {
	st := srv.Store()

	org, orgErr := st.Org()
	issuer := "http://" + srv.Addr() + "/devidp"
	if orgErr != nil {
		traceKey, keyErr := standaloneTraceKey(dataDir)
		if keyErr != nil {
			return "", "", keyErr
		}
		org = &store.Org{
			Name:        "Standalone",
			SSOProvider: "Developer IdP",
			AdminEmail:  id.Email,
			TraceKey:    traceKey,
			TracePath:   filepath.Join(dataDir, "traces"),
			// No cap on machine sessions here. The cap exists to make the
			// identity provider confirm the person again, and standalone's
			// identity provider is this process: sending anyone back to it
			// would confirm nothing and cost a sign-in.
			MachineSessionMaxDays: 0,
			SetupComplete:         true,
		}
	}
	// The issuer follows the listen address; standalone owns its org row.
	org.OIDCIssuer = issuer
	org.OIDCClientID = "john-standalone"
	// An org deployment gets its snapshot signing key from `john backend
	// --setup`; standalone has nobody to run that, so the key is born here on
	// first run instead. Without it no policy snapshot can be signed, and the
	// client — which pins the public half exactly as it does against a real
	// backend — would hold nothing and deny every request. Existing standalone
	// databases predating this pick a key up the next time they start.
	if org.SigningKey == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", "", err
		}
		org.SigningKey = base64.StdEncoding.EncodeToString(priv)
	}
	if err := st.SaveOrg(org); err != nil {
		return "", "", err
	}

	// A first run gets a baseline. Nothing is assumed anywhere else in john —
	// a fresh organization protects nothing until an administrator says so,
	// which is right when there is an administrator to say it. Standalone has
	// nobody: the developer is the whole organization, and "you have no
	// workspace protections until you write some" is a setting nobody chose
	// and most people would not know to look for.
	//
	// Two, and only two, because a baseline nobody agreed to should be the
	// things that are true of every project rather than a house style. A
	// dotenv file is secrets by convention, and .git is the record of the work
	// itself — an agent may read it and may not rewrite it, which leaves
	// `git log` and `git diff` working and takes away the commands that lose
	// an afternoon. Everything else is the developer's to add in the console.
	//
	// It is seeded once, on the run that creates the organization. Deleting
	// one afterwards is a decision, and a bootstrap that put it back every
	// start would be overruling it.
	if orgErr != nil {
		if err := seedStandaloneBaseline(st, id.Email); err != nil {
			return "", "", err
		}
	}

	u, err := st.UpsertSSOUser(id.Subject, id.Email, id.Name, id.Email)
	if err != nil {
		return "", "", err
	}
	host, _ := os.Hostname()
	token, err = st.CreateMachineToken(u.ID, host, 365*24*time.Hour)
	if err != nil {
		return "", "", err
	}
	return token, u.Email, nil
}

func firstEnabledModel(st *store.Store) (*store.Model, error) {
	models, err := st.Models()
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.Status == "enabled" {
			return m, nil
		}
	}
	return nil, nil
}

// seedStandaloneBaseline writes the workspace protections a solo developer
// gets without asking. A baseline group applies to every team, which in
// standalone is the one person there is.
func seedStandaloneBaseline(st *store.Store, actor string) error {
	group, err := st.CreatePolicyGroup("Baseline",
		"What john protects in every project on this machine. Edit or remove it in the console.",
		true, actor)
	if err != nil {
		return err
	}
	for _, p := range []struct {
		name, description string
		spec              policy.WorkspaceSpec
	}{
		{
			"Secrets are not readable",
			"Dotenv files hold credentials, so the agent never sees them: they are absent from the sandbox rather than refused, and nothing that runs in there can read one.",
			policy.WorkspaceSpec{Kind: "workspace", Mode: policy.ModeExclude,
				Paths: []string{".env", ".env.*", "**/.env", "**/.env.*"}},
		},
		{
			"Git history is read-only",
			"The agent can read the repository — log, diff, blame — and cannot write it. A rebase or a reset that goes wrong is the one mistake in a sandbox that reaches work the developer has already done.",
			policy.WorkspaceSpec{Kind: "workspace", Mode: policy.ModeReadOnly,
				Paths: []string{".git"}},
		},
	} {
		spec, err := json.Marshal(p.spec)
		if err != nil {
			return err
		}
		if _, err := st.CreatePolicy(group.ID, p.name, p.description, spec, actor); err != nil {
			return err
		}
	}
	return nil
}

// standaloneTraceKey generates the ORGANIZATION's age keypair on first run.
// The identity stays beside the data so a solo developer can read every
// trace; an organization would hold it elsewhere, which is the point of the
// split.
//
// This is not the developer's own key. That one is credstore's, at
// ~/.john/trace-identity.txt, and it opens only the sessions it was wrapped
// for. In standalone the same person holds both, one per role.
func standaloneTraceKey(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "trace-identity.txt")
	if data, err := os.ReadFile(path); err == nil {
		if id, err := age.ParseX25519Identity(string(firstLine(data))); err == nil {
			return id.Recipient().String(), nil
		}
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}

func firstLine(data []byte) []byte {
	for i, b := range data {
		if b == '\n' {
			return data[:i]
		}
	}
	return data
}
