package client

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

// The client and the backend are one program split across two machines. The
// wire between them changes, and running two versions of it against each
// other produces failures that look like anything except a version skew, so
// john does not do it: a backend serves only its own build.
//
// Keeping that promise cannot mean "and the developer figures it out". The
// client fetches the build the backend is holding, replaces itself with it,
// and starts again where it left off — but it asks first, every time, because
// silently rewriting the binary somebody just ran is not a thing to do behind
// their back.

// update is a version difference that has to be closed before john can run.
type update struct {
	Required string // the version the backend serves
	Current  string // what this binary is
	URL      string // absolute URL of the build to fetch
	Token    string // machine token; the download endpoint is behind sign-in
}

// checkVersion asks the backend what it runs, over the endpoint that answers
// before anyone is signed in. It returns nil when there is nothing to do —
// including every case where the answer cannot be had, because a version
// check that cannot complete is not a reason to refuse to work.
func checkVersion(backendURL, current, token string) *update {
	if isDevBuild(current) {
		return nil
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(strings.TrimSuffix(backendURL, "/") + "/api/v1/org/public")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Backend struct {
			Version string `json:"version"`
		} `json:"backend"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	remote := out.Backend.Version
	if remote == current || isDevBuild(remote) {
		return nil
	}
	return &update{
		Required: remote,
		Current:  current,
		URL:      strings.TrimSuffix(backendURL, "/") + "/releases/" + releaseFileName(),
		Token:    token,
	}
}

// isDevBuild reports a binary with no version stamped into it — somebody
// working on john itself, who has no release to be sent to.
func isDevBuild(v string) bool { return v == "" || v == "dev" }

// releaseFileName is what this platform's build is called on the backend,
// matching what `make clients` produces.
func releaseFileName() string {
	name := "john-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// confirm puts the update to the developer and waits for an answer. Enter
// accepts, because saying yes is what almost everyone will do and the
// alternative is not useful for long; anything starting with "n" declines.
func (u *update) confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprintf(out, `
This backend runs john %s. You have %s.

john will download %s from the backend, replace this binary, and start again.
Nothing else on your machine is touched, and the session you asked for runs
straight after.

`, u.Required, u.Current, u.Required)

	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Update now? [Y/n]: ")
		line, err := r.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if err != nil && answer == "" {
			return false
		}
		switch answer {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			fmt.Fprintln(out, "Please answer y or n.")
		}
	}
}

// apply downloads the build and puts it where this binary is. It does not
// return on success on Unix, where the new binary replaces this process
// outright; on Windows it runs the new binary and returns its exit code.
func (u *update) apply(out io.Writer) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("finding this binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return 1, fmt.Errorf("resolving this binary: %w", err)
	}

	tmp, err := u.download(exe, out)
	if err != nil {
		return 1, err
	}
	// From here the downloaded file is either installed or removed; a half
	// finished update must not leave a stray binary beside the real one.
	defer os.Remove(tmp)

	if err := replaceBinary(exe, tmp); err != nil {
		return 1, fmt.Errorf("replacing %s: %w", exe, err)
	}
	fmt.Fprintf(out, "Updated to john %s. Starting again…\n\n", u.Required)
	return relaunch(exe, os.Args)
}

// download fetches the build to a temporary file beside the binary it will
// replace — beside, because a rename across filesystems is not atomic and
// /tmp is very often a different filesystem. It says how far along it is on
// out as it goes.
//
// It asks for the build gzipped, which a backend from before v0.0.2 ignores
// and sends it plain; either is installed the same.
func (u *update) download(exe string, out io.Writer) (string, error) {
	req, err := http.NewRequest("GET", u.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/gzip, application/octet-stream")
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", u.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: status %d", u.URL, resp.StatusCode)
	}

	f, err := os.CreateTemp(filepath.Dir(exe), ".john-update-*")
	if err != nil {
		return "", fmt.Errorf("writing beside %s: %w", exe, err)
	}
	tmp := f.Name()

	prog := newProgress(resp.Body, out, "Downloading john "+u.Required+"…", resp.ContentLength)
	var body io.Reader = prog
	if resp.Header.Get("Content-Type") == "application/gzip" {
		zr, err := gzip.NewReader(prog)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return "", fmt.Errorf("downloading %s: %w", u.URL, err)
		}
		body = zr
	}
	n, err := io.Copy(f, body)
	prog.finish()
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("downloading %s: %w", u.URL, err)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", closeErr
	}
	// A truncated download that still parses as a file is the worst outcome
	// here: it would replace a working binary with a broken one and there
	// would be nothing left to run. An empty or absurdly small body is the
	// cheap version of that check.
	if n < 1<<20 {
		os.Remove(tmp)
		return "", fmt.Errorf("downloaded john %s is only %d bytes — refusing to install it", u.Required, n)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// progress counts a download as it arrives. On a terminal it redraws one line
// with how much is done, how fast, and how long is left; anywhere else it says
// what it is fetching and how big it is, once, because a log does not want a
// line per tenth of a second.
type progress struct {
	r     io.Reader
	out   io.Writer
	label string
	total int64 // -1 when the backend did not say
	live  bool

	n     int64
	start time.Time
	drawn time.Time
}

func newProgress(r io.Reader, out io.Writer, label string, total int64) *progress {
	p := &progress{r: r, out: out, label: label, total: total, start: time.Now()}
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.live = true
		p.draw()
	} else if total > 0 {
		fmt.Fprintf(out, "%s (%s)\n", label, megabytes(total))
	} else {
		fmt.Fprintln(out, label)
	}
	return p
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.live && time.Since(p.drawn) >= 100*time.Millisecond {
		p.draw()
	}
	return n, err
}

func (p *progress) draw() {
	p.drawn = time.Now()
	fmt.Fprintf(p.out, "\r\033[K%s", p.line())
}

// finish leaves the line as it ended: complete, or where it stopped.
func (p *progress) finish() {
	if !p.live {
		return
	}
	p.draw()
	fmt.Fprintln(p.out)
}

func (p *progress) line() string {
	elapsed := time.Since(p.start)
	rate := 0.0
	if s := elapsed.Seconds(); s > 0 {
		rate = float64(p.n) / s
	}
	if p.total <= 0 {
		return fmt.Sprintf("%s %s · %s/s", p.label, megabytes(p.n), megabytes(int64(rate)))
	}
	line := fmt.Sprintf("%s %s of %s (%d%%)", p.label,
		strings.TrimSuffix(megabytes(p.n), " MB"), megabytes(p.total), p.n*100/p.total)
	switch {
	case p.n >= p.total:
		line += " · done in " + duration(elapsed)
	case elapsed >= time.Second && rate > 0:
		left := time.Duration(float64(p.total-p.n) / rate * float64(time.Second))
		line += fmt.Sprintf(" · %s/s · %s left", megabytes(int64(rate)), duration(left))
	}
	return line
}

func megabytes(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func duration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
