// Package backend is the control plane: the HTTP server every john client
// talks to, the console's host, and — in later stages — the agent loop, the
// policy engine, and the model router. It runs on one machine, stores
// everything in SQLite, and has no configuration file.
package backend

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/oliverkofoed/gokit/agentkit/llm"
	"golang.org/x/term"

	"github.com/popstarapps/john/auth"
	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/console"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// Config assembles a backend. Standalone mode builds one of these in-process;
// `john backend` builds one from flags.
type Config struct {
	DBPath  string
	Listen  string
	Version string

	// Setup runs the bootstrap flow: a one-time URL is minted and only the
	// setup endpoints are served.
	Setup bool

	// Standalone is the backend `john` runs in its own process for one
	// person on one machine. Its identity provider signs in whoever asks, so
	// it answers only requests from this machine that are addressed to it by
	// a loopback name — which also shuts out a web page that points its own
	// domain at 127.0.0.1 (DNS rebinding) to reach it through the browser.
	// The console is told, and its identity settings are fixed.
	Standalone bool

	// DevIdP, when set, mounts the embedded identity provider under /devidp
	// and every sign-in is this identity. Standalone mode and tests only.
	DevIdP *devidp.Identity

	// Streamer overrides how the agent loop reaches models. Nil means the
	// real providers (see Server.streamer); tests inject llmtest fakes here.
	Streamer llm.Streamer
}

// Server is a running backend.
type Server struct {
	cfg     Config
	store   *store.Store
	auth    *auth.Service
	hub     *hub
	idp     *devidp.Provider
	http    *http.Server
	ln      net.Listener
	started time.Time

	// The one trace writer for this backend. Built on first use because the
	// organization — and with it the trace key — may not exist yet when the
	// server is constructed.
	tracesMu  sync.Mutex
	traces    *trace.Writer
	tracesFor string

	// SetupURL is the one-time bootstrap URL, set only in setup mode.
	SetupURL string

	// SetupTraceIdentity is a freshly minted organization trace key, printed
	// once by Run and then cleared. It is generated here so that an operator
	// without age-keygen still gets a real key, and it goes to the terminal
	// rather than through the browser: the private half must never reach the
	// database, an HTTP response, or a log.
	SetupTraceIdentity string

	// setupTraceKey is the public half of the above, used when the setup form
	// leaves the field empty.
	setupTraceKey string
}

// Run is the `john backend` entrypoint. It returns the process exit code.
func Run(args []string, version string) int {
	fs := flag.NewFlagSet("john backend", flag.ExitOnError)
	var (
		dbPath = fs.String("db", "john.db", "path to the SQLite database (created if missing)")
		listen = fs.String("listen", "127.0.0.1:8080", "address to listen on")
		setup  = fs.Bool("setup", false, "bootstrap: print a one-time setup URL and serve the setup flow")
	)
	fs.Parse(args)

	srv, err := New(Config{DBPath: *dbPath, Listen: *listen, Version: version, Setup: *setup})
	if err != nil {
		fmt.Fprintln(os.Stderr, "john backend:", err)
		return 1
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *setup {
		fmt.Printf("john backend %s in setup mode on http://%s\n\n", version, srv.Addr())

		// The one thing printed here that cannot be recovered later. It is
		// written to this terminal and nowhere else — not to the database, not
		// through the browser — and whoever can read this output is already
		// trusted to bootstrap the organization.
		fmt.Println("This organization's TRACE READ KEY. Save it now, somewhere safe:")
		fmt.Printf("\n    %s\n\n", srv.SetupTraceIdentity)
		fmt.Println("It is the only thing that can read session traces. It is not stored")
		fmt.Println("anywhere, it is not recoverable, and traces written without it are lost")
		fmt.Println("for good. To use a key you generated yourself instead, paste its public")
		fmt.Println("half into the setup form and this one is discarded.")
		srv.SetupTraceIdentity = ""

		fmt.Println("\nOpen this one-time URL to configure the organization:")
		fmt.Printf("\n    %s\n\n", srv.SetupURL)
		srv.OfferRemoteSetup(os.Stdin, os.Stdout,
			term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())),
			OtherAddresses())
		fmt.Println("When setup completes, stop this process and start the backend without --setup.")
	} else {
		if _, err := srv.Store().Org(); errors.Is(err, store.ErrNoOrg) {
			fmt.Println("No organization is configured yet — run `john backend --setup` first.")
		}
		fmt.Printf("john backend %s listening on http://%s (db %s)\n", version, srv.Addr(), *dbPath)
	}
	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "john backend:", err)
		return 1
	}
	return 0
}

// New opens the database, applies migrations, and binds the listener. It does
// not begin serving; Serve does.
func New(cfg Config) (*Server, error) {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	s := &Server{cfg: cfg, store: st, ln: ln, auth: auth.New(st), started: time.Now()}
	s.hub = newHub(s)

	if cfg.DevIdP != nil {
		s.idp, err = devidp.New("http://"+ln.Addr().String()+"/devidp", *cfg.DevIdP)
		if err != nil {
			s.Close()
			return nil, err
		}
	}

	if cfg.Setup {
		org, err := st.Org()
		if err != nil && !errors.Is(err, store.ErrNoOrg) {
			s.Close()
			return nil, err
		}
		if org != nil && org.SetupComplete {
			s.Close()
			return nil, errors.New("setup already completed — refusing to run --setup again on a configured backend")
		}
		token := store.NewSecret()
		if err := st.CreateSetupToken(store.HashSecret(token)); err != nil {
			s.Close()
			return nil, err
		}
		s.SetupURL = "http://" + loopbackFor(ln.Addr()) + "/setup?token=" + token

		// The organization's trace key. An operator who would rather hold one
		// they generated elsewhere pastes it into the form and this is
		// discarded; leaving the field empty uses this one.
		identity, err := age.GenerateX25519Identity()
		if err != nil {
			s.Close()
			return nil, err
		}
		s.SetupTraceIdentity = identity.String()
		s.setupTraceKey = identity.Recipient().String()
	}

	h := s.handler()
	if cfg.Standalone {
		h = localOnly(h)
	}
	s.http = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// loopbackFor is addr, with a wildcard host (0.0.0.0, ::) replaced by the
// loopback one: a URL for "every address" is not one a browser can open. Go
// binds a wildcard dual-stack, so 127.0.0.1 reaches it either way.
func loopbackFor(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Addr is the address the server is bound to, useful when Listen was :0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Store exposes the database to the packages assembled around the server.
func (s *Server) Store() *store.Store { return s.store }

// DevIdP returns the embedded identity provider, or nil.
func (s *Server) DevIdP() *devidp.Provider { return s.idp }

// Auth exposes the identity layer, for standalone mode's direct token mint.
func (s *Server) Auth() *auth.Service { return s.auth }

// traceWriter returns the backend's single trace writer, or nil when the
// organization has no trace store configured. It is built once and rebuilt
// only if setup changed the key or the path.
//
// There is exactly one so that there is exactly one sequence counter per
// file. A writer per session would restart the count every time a session was
// reopened, and append a second run of numbers into a file that already had
// them — which a reader cannot tell apart from a line having been removed.
func (s *Server) traceWriter() *trace.Writer {
	org, err := s.store.Org()
	if err != nil || org.TraceKey == "" || org.TracePath == "" {
		return nil
	}
	want := org.TracePath + "\x00" + org.TraceKey

	s.tracesMu.Lock()
	defer s.tracesMu.Unlock()
	if s.traces != nil && s.tracesFor == want {
		return s.traces
	}
	w, err := trace.NewWriter(org.TracePath, org.TraceKey)
	if err != nil {
		return nil
	}
	s.traces, s.tracesFor = w, want
	return w
}

// Serve blocks until the context ends or the server fails.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Close releases everything. Safe after Serve returns.
func (s *Server) Close() {
	if s.hub != nil {
		s.hub.shutdownAll()
	}
	if s.http != nil {
		s.http.Close()
	}
	if s.ln != nil {
		s.ln.Close()
	}
	s.store.Close()
}

// localOnly refuses a request that did not come from this machine, or that
// names a host other than a loopback one. The second is what stops DNS
// rebinding: the browser sends the attacker's domain as the Host.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !isLoopback(from) || !isLoopback(hostOnly(r.Host)) {
			http.Error(w, "john standalone answers only on this machine, at 127.0.0.1 or localhost",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// streamer reaches the model providers. A model's key is the one stored with
// it, and only that: the client library would otherwise fall back to
// ANTHROPIC_API_KEY and the like, and a backend quietly spending a key from
// its own environment is not something anyone configured. With no stored key
// the request goes without one, and the provider's refusal says so.
func (s *Server) streamer() llm.Streamer {
	if s.cfg.Streamer != nil {
		return s.cfg.Streamer
	}
	return llm.New(llm.WithKeyFunc(func(context.Context, string) (string, error) {
		return llm.NoAuth, nil
	}))
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	if s.cfg.Setup {
		// Setup mode serves the wizard and nothing else: no sign-in exists
		// yet, so every other endpoint would be an unauthenticated surface.
		mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
		mux.HandleFunc("GET /api/v1/models/catalog", s.handleModelCatalog)
		mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable, "setup_required")
		})
		mux.Handle("/", consoleHandler(console.FS()))
		return mux
	}

	if s.idp != nil {
		mux.Handle("/devidp/", http.StripPrefix("/devidp", s.idp.Handler()))
	}

	mux.HandleFunc("GET /auth/login", s.auth.HandleLogin)
	mux.HandleFunc("GET /auth/callback", s.auth.HandleCallback)
	mux.HandleFunc("GET /auth/logout", s.auth.HandleLogout)
	mux.HandleFunc("POST /api/v1/auth/cli/start", s.handleCLIStart)
	mux.HandleFunc("POST /api/v1/auth/cli/poll", s.handleCLIPoll)
	mux.HandleFunc("POST /api/v1/auth/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/v1/viewer", s.handleViewer)

	mux.HandleFunc("GET /api/v1/users", s.handleListUsers)
	mux.HandleFunc("GET /api/v1/users/{id}", s.handleGetUser)
	mux.HandleFunc("PUT /api/v1/users/{id}/teams", s.handleSetUserTeams)
	mux.HandleFunc("PUT /api/v1/users/{id}/role", s.handleSetUserRole)
	mux.HandleFunc("PUT /api/v1/users/{id}/status", s.handleSetUserStatus)
	mux.HandleFunc("GET /api/v1/users/{id}/groups", s.handleUserGroups)
	mux.HandleFunc("GET /api/v1/users/{id}/policies", s.handleUserPolicies)
	mux.HandleFunc("GET /api/v1/users/{id}/models", s.handleUserModels)

	mux.HandleFunc("GET /api/v1/teams", s.handleListTeams)
	mux.HandleFunc("POST /api/v1/teams", s.handleCreateTeam)
	mux.HandleFunc("GET /api/v1/teams/{id}", s.handleGetTeam)
	mux.HandleFunc("PUT /api/v1/teams/{id}/groups", s.handleSetTeamGroups)
	mux.HandleFunc("DELETE /api/v1/teams/{id}", s.handleDeleteTeam)

	mux.HandleFunc("GET /api/v1/groups", s.handleListGroups)
	mux.HandleFunc("POST /api/v1/groups", s.handleCreateGroup)
	mux.HandleFunc("GET /api/v1/groups/{id}", s.handleGetGroup)

	mux.HandleFunc("GET /api/v1/policies", s.handleListPolicies)
	mux.HandleFunc("POST /api/v1/policies", s.handleCreatePolicy)
	mux.HandleFunc("GET /api/v1/policies/{id}", s.handleGetPolicy)
	mux.HandleFunc("PUT /api/v1/policies/{id}/enabled", s.handleSetPolicyEnabled)

	mux.HandleFunc("GET /api/v1/models", s.handleListModels)
	mux.HandleFunc("POST /api/v1/models", s.handleCreateModel)
	mux.HandleFunc("POST /api/v1/models/test", s.handleTestModel)
	mux.HandleFunc("GET /api/v1/models/catalog", s.handleModelCatalog)
	mux.HandleFunc("PUT /api/v1/models/{id}/status", s.handleSetModelStatus)
	mux.HandleFunc("PUT /api/v1/models/{id}", s.handleUpdateModel)
	mux.HandleFunc("DELETE /api/v1/models/{id}", s.handleDeleteModel)

	mux.HandleFunc("GET /api/v1/access-requests", s.handleListAccessRequests)
	mux.HandleFunc("POST /api/v1/access-requests", s.handleCreateAccessRequest)
	mux.HandleFunc("GET /api/v1/access-requests/{id}", s.handleGetAccessRequest)
	mux.HandleFunc("POST /api/v1/access-requests/{id}/decision", s.handleDecideAccessRequest)
	mux.HandleFunc("POST /api/v1/access-requests/{id}/comments", s.handleCommentAccessRequest)

	mux.HandleFunc("GET /api/v1/network-requests", s.handleListNetworkRequests)
	mux.HandleFunc("GET /api/v1/network-requests/{id}", s.handleGetNetworkRequest)

	mux.HandleFunc("GET /api/v1/changes", s.handleListChanges)

	mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/v1/traces", s.handleTraceSessions)
	mux.HandleFunc("GET /api/v1/traces/recipients", s.handleTraceRecipients)
	mux.HandleFunc("GET /api/v1/traces/{session}/file", s.handleTraceFile)
	mux.HandleFunc("POST /api/v1/traces/{session}/grant", s.handleTraceGrant)

	mux.HandleFunc("GET /api/v1/usage", s.handleGetUsage)

	mux.HandleFunc("GET /api/v1/org", s.handleGetOrg)
	mux.HandleFunc("PATCH /api/v1/org", s.handleUpdateOrg)
	mux.HandleFunc("GET /api/v1/org/public", s.handleOrgPublic)

	mux.HandleFunc("GET /api/v1/policy/snapshot", s.handlePolicySnapshot)
	mux.HandleFunc("GET /api/v1/policy/signing-key", s.handleSigningPublicKey)

	mux.HandleFunc("GET /api/v1/releases", s.handleListReleases)
	mux.HandleFunc("GET /releases/{file}", s.handleDownloadRelease)

	mux.HandleFunc("GET /api/v1/session", s.handleSessionWS)

	// The API namespace never falls back to the SPA: an unknown endpoint is
	// a JSON 404, not a 200 with HTML a client would try to parse.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/", consoleHandler(console.FS()))
	return mux
}
