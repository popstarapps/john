package client

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/keychain"
)

// Login is the `john login` entrypoint. It takes where to sign in, as a
// positional argument, and it is also where a machine says which john it
// runs:
//
//	john login https://john.acme.example   sign in to an organization
//	john login standalone                  run solo, backend in-process
//	john login                             sign in again where you already are
//
// Both forms record the machine's mode, so plain `john` afterwards needs no
// arguments and no remembering. It returns the process exit code.
func Login(args []string) int {
	fs := flag.NewFlagSet("john login", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: john login [<backend-url> | standalone]

  john login https://john.acme.example   sign in to your organization
  john login standalone                  run john solo on this machine
  john login                             sign in again where you already are
`)
	}
	fs.Parse(args)

	target := strings.TrimSpace(fs.Arg(0))

	// `john login standalone` is a choice about this machine, not a sign-in:
	// standalone's identity provider is john itself, and it signs the local
	// user in when the session starts.
	if target == credstore.ModeStandalone {
		if s, err := credstore.LoadSetup(); err == nil && s.Mode == credstore.ModeStandalone {
			fmt.Println("This machine already runs john standalone.")
		} else {
			if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeStandalone}); err != nil {
				fmt.Fprintln(os.Stderr, "john login:", err)
				return 1
			}
			fmt.Println("This machine now runs john standalone.")
		}
		return loginStandaloneModel()
	}

	url := strings.TrimSuffix(target, "/")
	if url == "" {
		// No argument: sign in again where this machine already signed in,
		// which is what an expired sign-in needs and is worth not having to
		// look up.
		if c, err := credstore.Load(); err == nil {
			url = c.BackendURL
		}
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "john login: no backend known — `john login https://<your-john-backend>`,")
		fmt.Fprintln(os.Stderr, "  or `john login standalone` to run john on this machine alone.")
		return 2
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		fmt.Fprintf(os.Stderr, "john login: %q is not a backend URL or `standalone`\n", target)
		return 2
	}

	creds, err := loginFlow(url, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john login:", err)
		return 1
	}
	creds.BackendURL = url
	if err := credstore.Save(creds); err != nil {
		fmt.Fprintln(os.Stderr, "john login: storing credentials:", err)
		return 1
	}
	if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeBackend}); err != nil {
		fmt.Fprintln(os.Stderr, "john login: recording this machine's setup:", err)
		return 1
	}
	// The trace key this machine reads its own sessions with. Made once and
	// kept: signing in again must not re-key, because that would silently
	// cost the developer every session they can currently read.
	if _, err := credstore.EnsureIdentity(); err != nil {
		fmt.Fprintln(os.Stderr, "john login: could not store a trace key:", err)
		fmt.Fprintln(os.Stderr, "  sessions will still run; you will not be able to read your own traces back.")
	}
	if name := keychain.Name(); name != "" {
		fmt.Printf("signed in as %s · credentials kept in %s\n", creds.Email, name)
	} else {
		fmt.Printf("signed in as %s\n", creds.Email)
	}
	return 0
}

var loginPollInterval = 2 * time.Second

// loginFlow runs the CLI half of the sign-in against a backend and returns
// the credential pair the backend minted.
func loginFlow(backendURL string, out *os.File) (*credstore.Credentials, error) {
	hostname, _ := os.Hostname()

	var start struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	if err := postJSON(backendURL+"/api/v1/auth/cli/start", map[string]string{}, &start, nil); err != nil {
		return nil, fmt.Errorf("reaching the backend: %w", err)
	}

	fmt.Fprintf(out, "Opening your browser to sign in…\n\n    %s\n\n", start.URL)
	openBrowser(start.URL)
	fmt.Fprintln(out, "Waiting for the sign-in to complete…")

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var done struct {
			Pending          bool      `json:"pending"`
			Token            string    `json:"token"`
			ExpiresAt        time.Time `json:"expiresAt"`
			RefreshToken     string    `json:"refreshToken"`
			RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
			User             struct {
				Email string `json:"email"`
			} `json:"user"`
		}
		status := 0
		if err := postJSON(backendURL+"/api/v1/auth/cli/poll",
			map[string]string{"code": start.Code, "host": hostname}, &done, &status); err != nil {
			return nil, err
		}
		if status == http.StatusOK && done.Token != "" {
			return &credstore.Credentials{
				BackendURL:       backendURL,
				Token:            done.Token,
				ExpiresAt:        done.ExpiresAt,
				Refresh:          done.RefreshToken,
				RefreshExpiresAt: done.RefreshExpiresAt,
				Email:            done.User.Email,
			}, nil
		}
		if status != http.StatusAccepted {
			return nil, fmt.Errorf("sign-in failed (status %d)", status)
		}
		time.Sleep(loginPollInterval)
	}
	return nil, fmt.Errorf("timed out waiting for the browser sign-in")
}

func postJSON(url string, in, out any, statusOut *int) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if statusOut != nil {
		*statusOut = resp.StatusCode
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// openBrowser tries the platform opener; printing the URL already happened,
// so failure is fine.
//
// It is a variable so that a test can sign in without a browser window
// appearing on the developer's screen, and it honours JOHN_NO_BROWSER for the
// same reason on a machine with no display: over SSH, in a container, or in
// CI, launching a browser is at best useless and at worst a hang. The URL is
// printed either way, which is the part that matters.
var openBrowser = func(url string) {
	if os.Getenv("JOHN_NO_BROWSER") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

// Console is the `john console` entrypoint: it opens the web console.
func Console(args []string) int {
	if s, err := credstore.LoadSetup(); err == nil && s.Mode == credstore.ModeStandalone {
		return standaloneConsole()
	}
	c, err := credstore.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "john console:", err)
		return 1
	}
	fmt.Printf("opening %s\n", c.BackendURL)
	openBrowser(c.BackendURL)
	return 0
}
