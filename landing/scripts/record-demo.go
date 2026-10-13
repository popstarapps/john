//go:build ignore

// Command record-demo captures a `john` terminal session for the landing page
// hero.
//
//	go run landing/scripts/record-demo.go
//
// It builds ./terminalui/cmd/demo, runs it on a pseudo-terminal, types at it
// the way a developer would, and records every byte the program wrote with the
// time it wrote it. The demo is the CLI's own terminal UI with a script where
// the backend would be, so what it says has to be kept to what the client
// says: the same notices, the same verdicts, an approval box only for an
// ask-developer match, and a model choice only where a routing policy acts. Nothing on the page is hand-written markup: what the hero
// plays back is the bytes the CLI emitted, rendered by the small terminal
// emulator below.
//
// The terminal it records is a dark one, and the panel on the page is dark in
// both of the page's themes for that reason: the CLI's colours are adaptive, so
// a recording carries the palette of whichever background it was made against,
// and a light-page/dark-recording mix would be somebody else's terminal. The
// page already inverts a card rather than tinting it (see --note-bg), so this
// is the shape it already had.
//
// Output is landing/src/session.json, imported by the hero's player.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	// The terminal is the size of the window the page shows it in: the panel
	// takes its height from rows (see .cast-screen), so a dialog that fits the
	// terminal fits the panel and nothing scrolls that would not have scrolled.
	cols = 80
	rows = 26

	// Output written closer together than this belongs to one frame. A
	// streaming answer arrives a word every 32ms, which nothing can see as
	// separate paints; coalescing to roughly 30fps costs nothing visible and
	// takes most of an order of magnitude off the frame count.
	frameGap = 34 * time.Millisecond
	// Dead air longer than this is compressed to it — but only where the
	// script was waiting on the program, which is most of what the program
	// does: thinking, a command running. Long enough to see that it took a
	// moment, not so long that it is dead air. Time the script spent reading
	// something on purpose is a hold (see dwell), and holds are never
	// compressed: they are the moments a viewer has to read too.
	maxIdle = 900 * time.Millisecond

	// pace stretches every hold: the session is read by somebody seeing it for
	// the first time, at the speed of the page rather than the speed of the
	// person who wrote the script.
	pace = 1.35
)

func main() {
	root, err := os.Getwd()
	check(err)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		die("run this from the repository root: go run landing/scripts/record-demo.go")
	}

	bin := filepath.Join(os.TempDir(), fmt.Sprintf("john-demo-%d", os.Getpid()))
	build := exec.Command("go", "build", "-o", bin, "./terminalui/cmd/demo")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	check(build.Run())
	defer os.Remove(bin)

	chunks, holds, title := record(bin)
	captured, styles := frames(chunks, holds)

	out := recording{
		Cols:     cols,
		Rows:     rows,
		Title:    title,
		Duration: captured[len(captured)-1].T,
		Styles:   styles,
		Frames:   captured,
	}
	blob, err := json.Marshal(out)
	check(err)
	dest := filepath.Join(root, "landing", "src", "session.json")
	check(os.WriteFile(dest, blob, 0o644))
	fmt.Printf("recorded %s · %d frames · %.1fs · %d kB\n",
		dest, len(out.Frames), out.Duration, len(blob)/1024)
}

// ---------------------------------------------------------------- the session

// chunk is one write the program made, and when it made it.
type chunk struct {
	at   time.Duration
	data []byte
}

// record runs one session end to end and returns what the program wrote.
//
// The script waits on what is actually on screen rather than on a clock, so a
// slower machine records the same session, just with different pauses in it —
// and a wait that never arrives fails the recording instead of typing into the
// wrong view.
func record(bin string) ([]chunk, []hold, string) {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"CLICOLOR_FORCE=1",
		// termenv reads this when it cannot ask the terminal itself, which is
		// the case here: nothing on this pty answers an OSC 11 query. Light
		// text on a dark ground, which is what the page draws the panel as.
		"COLORFGBG=15;0",
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	check(err)
	defer ptmx.Close()

	s := &session{ptmx: ptmx, screen: newScreen(cols), start: time.Now()}
	go s.read()

	s.wait("ask john to do something")
	title := s.title()
	s.pause(600 * time.Millisecond)
	s.type_("the retry in billing/client.go gives up on 429s. fix it and prove it.")
	s.pause(500 * time.Millisecond)
	s.send("\r")

	// The first decision, and not the developer's to dodge: the file the agent
	// just read carries the keyword a routing policy watches for, so the
	// backend holds it back from the external model and asks which of the
	// models the policy leaves to carry on with. Read why, then carry on — the
	// conversation goes with the session.
	s.wait("CONTINUE ON")
	s.dwell(2600 * time.Millisecond)
	s.send("\r")
	s.dwell(700 * time.Millisecond)

	// The decision the whole page is an argument about: a request an
	// ask-developer policy matched, held with the entire body in hand before a
	// byte of it leaves. Somebody watching this has to be able to read the
	// body — the user and the git remote it would have carried out — so the
	// script takes as long over it as a person would.
	//
	// The whole request fits in the box, so there is nothing to scroll; what
	// moves instead is the choice, because that is what is actually happening.
	// The cursor opens on "reject" — the safe way out — so this looks at
	// "allow once", thinks better of it, and comes back.
	s.wait("telemetry.vendor.io")
	s.dwell(2900 * time.Millisecond)
	s.send("\x1b[D") // left, onto "allow once"
	s.dwell(1500 * time.Millisecond)
	s.send("\x1b[C") // right, back to "reject"
	s.dwell(1400 * time.Millisecond)
	s.send("\r")

	// The red run: the test written first fails, and the line says why.
	s.wait("exit 1")
	s.dwell(1800 * time.Millisecond)

	// The third decision, and the other half of the network story: a host no
	// policy mentions is not asked about, it is blocked — and what john asks is
	// whether to put it to an administrator. Read the box, take the default,
	// which is the whole host, and read the notice that says it was filed.
	s.wait("REQUEST A POLICY")
	s.dwell(3400 * time.Millisecond)
	s.send("\r")
	s.wait("filed for sum.golang.org")
	s.dwell(1600 * time.Millisecond)

	s.wait("to review")
	s.pause(900 * time.Millisecond)
	s.type_("/apply")
	s.pause(400 * time.Millisecond)
	s.send("\r")

	// Nothing has touched the working directory yet, and this is where that
	// changes. Same reasoning as the approval — read the file list (the
	// coverage profile the test run wrote is there, unticked, because
	// .gitignore ignores it), open one, read the hunk, then apply and say yes.
	s.wait("apply to ~/src/payments")
	s.dwell(3200 * time.Millisecond)
	s.send("\r") // expand the first file and read the hunks
	s.dwell(2400 * time.Millisecond)
	for range 8 {
		s.send("\x1b[B")
		s.dwell(420 * time.Millisecond)
	}
	s.dwell(1800 * time.Millisecond)
	s.send("a") // apply the selected files
	s.wait("APPLY TO WORKING DIRECTORY")
	s.dwell(1500 * time.Millisecond)
	s.send("y")
	s.wait("files \u2192 payments")
	// And the loop closes: an administrator approved the request meanwhile.
	s.wait("approved \u00b7 sum.golang.org")
	s.dwell(2000 * time.Millisecond)

	s.type_("/quit")
	s.pause(300 * time.Millisecond)
	s.send("\r")
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
	}
	s.pause(150 * time.Millisecond)

	s.mu()
	defer s.unmu()
	return s.chunks, s.holds, title
}

// ------------------------------------------------------------------- plumbing

// hold is a stretch of the recording the script spent deliberately reading
// something rather than waiting for the program. It survives idle compression
// whole, because it is the part a viewer has to keep up with.
type hold struct{ from, to time.Duration }

type session struct {
	ptmx   *os.File
	screen *screen
	start  time.Time
	lock   sync.Mutex
	chunks []chunk
	holds  []hold
}

// The reader goroutine writes the screen while the script reads it, so both go
// through here.
func (s *session) mu()   { s.lock.Lock() }
func (s *session) unmu() { s.lock.Unlock() }

func (s *session) read() {
	buf := make([]byte, 8192)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			b := append([]byte(nil), buf[:n]...)
			s.mu()
			s.chunks = append(s.chunks, chunk{at: time.Since(s.start), data: b})
			s.screen.write(b)
			s.unmu()
		}
		if err != nil {
			return
		}
	}
}

// wait blocks until text is on screen, and fails the recording if it never is.
func (s *session) wait(text string) {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		s.mu()
		found := strings.Contains(s.screen.plain(), text)
		s.unmu()
		if found {
			fmt.Fprintf(os.Stderr, "  reached %q\n", text)
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.mu()
	dump := s.screen.plain()
	s.unmu()
	fmt.Fprintln(os.Stderr, dump)
	die("timed out waiting for %q on screen", text)
}

// title reads the working directory out of the status line the CLI keeps under
// its input box — "~/src/payments · claude-sonnet-5-5 · sandbox ready" — and
// names the window after it, the way a terminal names itself after what is
// running in it.
func (s *session) title() string {
	s.mu()
	defer s.unmu()
	for _, line := range strings.Split(s.screen.plain(), "\n") {
		field, _, ok := strings.Cut(strings.TrimSpace(line), " · ")
		if ok && (strings.HasPrefix(field, "~/") || strings.HasPrefix(field, "/")) {
			return "john — " + field
		}
	}
	return "john"
}

// pause waits on the program: dead air, and compressible.
func (s *session) pause(d time.Duration) { time.Sleep(d) }

// dwell waits the way a developer reads: this is time the recording keeps.
func (s *session) dwell(d time.Duration) {
	d = time.Duration(float64(d) * pace)
	from := time.Since(s.start)
	time.Sleep(d)
	s.mu()
	s.holds = append(s.holds, hold{from: from, to: time.Since(s.start)})
	s.unmu()
}

func (s *session) send(keys string) {
	_, err := s.ptmx.WriteString(keys)
	check(err)
}

// type_ types a line at a human rate, slower after punctuation.
func (s *session) type_(text string) {
	for _, r := range text {
		s.send(string(r))
		d := 55 * time.Millisecond
		switch r {
		case ' ':
			d = 80 * time.Millisecond
		case '.', ',':
			d = 180 * time.Millisecond
		}
		time.Sleep(d)
	}
}

// ------------------------------------------------------------------- encoding

type recording struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
	// Title is what the window is called on the page, taken from the session
	// rather than written next to it, so it cannot describe a session that is
	// no longer the one being played.
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
	Styles   []style `json:"styles"`
	Frames   []frame `json:"frames"`
}

// frame is a screen state: the rows that changed since the frame before it, and
// how many rows the screen has now.
type frame struct {
	T    float64 `json:"t"`
	N    int     `json:"n"`
	Rows []row   `json:"r"`
}

type row struct {
	Index int
	Runs  []run
}

func (r row) MarshalJSON() ([]byte, error) {
	runs := r.Runs
	if runs == nil {
		runs = []run{} // a blanked row, which is not the same as no change
	}
	return json.Marshal([]any{r.Index, runs})
}

// run is a stretch of text sharing one style, as [styleIndex, text].
type run struct {
	Style int
	Text  string
}

func (r run) MarshalJSON() ([]byte, error) { return json.Marshal([]any{r.Style, r.Text}) }

// frames replays the recording through the emulator and snapshots it whenever
// the program stopped writing for a moment.
func frames(chunks []chunk, holds []hold) ([]frame, []style) {
	sc := newScreen(cols)
	var (
		out      []frame
		prev     [][]run
		clock    time.Duration
		lastReal time.Duration
	)
	flush := func() {
		snap := sc.snapshot()
		f := frame{T: clock.Seconds(), N: len(snap)}
		for i, line := range snap {
			if i < len(prev) && sameRuns(prev[i], line) {
				continue
			}
			f.Rows = append(f.Rows, row{Index: i, Runs: line})
		}
		if len(out) > 0 && len(f.Rows) == 0 && f.N == out[len(out)-1].N {
			return
		}
		out = append(out, f)
		prev = snap
	}

	for i, c := range chunks {
		if i > 0 {
			// Whatever of this gap the script spent reading is kept; only the
			// rest of it — the program taking its time — is compressed.
			held := overlap(lastReal, c.at, holds)
			if gap := held + min(c.at-lastReal-held, maxIdle); gap > 0 {
				clock += gap
			}
		}
		lastReal = c.at
		sc.write(c.data)
		// One frame per burst: hold the write until the next one turns out to
		// be far enough away to have been visible on its own.
		if i+1 < len(chunks) && chunks[i+1].at-c.at < frameGap {
			continue
		}
		flush()
	}
	flush()

	// The program sets the terminal up before it draws anything. That is real
	// output, but it is a blank screen, and a hero that opens on one for a
	// second and a half reads as broken rather than as starting.
	for len(out) > 1 && out[0].N == 0 {
		out = out[1:]
	}
	if len(out) > 0 {
		start := out[0].T
		for i := range out {
			out[i].T -= start
		}
	}
	return out, sc.styles
}

// overlap is how much of [from, to) falls inside a hold.
func overlap(from, to time.Duration, holds []hold) time.Duration {
	var total time.Duration
	for _, h := range holds {
		if lo, hi := max(from, h.from), min(to, h.to); hi > lo {
			total += hi - lo
		}
	}
	return total
}

func sameRuns(a, b []run) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------- the emulator

// style is one rendition: a foreground, a background and the attributes that
// survive being turned into CSS.
type style struct {
	Fg    string `json:"f,omitempty"`
	Bg    string `json:"g,omitempty"`
	Bold  bool   `json:"b,omitempty"`
	Dim   bool   `json:"d,omitempty"`
	Ital  bool   `json:"i,omitempty"`
	Uline bool   `json:"u,omitempty"`
}

type cell struct {
	r rune
	s int // index into screen.styles
}

// screen is a terminal, enough of one to render this program's output: colour,
// the cursor moves an inline Bubble Tea renderer makes, and a grid that grows
// instead of scrolling — so the whole session is one transcript, the way it is
// in the scrollback of a real terminal.
//
// The diff review takes the alternate screen, so that is here too: a separate
// grid the height of the terminal, and on the way out the transcript comes back
// as it was, with nothing of the review left in it.
type screen struct {
	width   int
	grid    [][]cell
	cy, cx  int
	main    [][]cell // the transcript, while the alternate screen is up
	mainCy  int
	mainCx  int
	alt     bool
	cur     style
	styles  []style
	byStyle map[style]int
	pending []byte
}

func newScreen(width int) *screen {
	s := &screen{width: width, byStyle: map[style]int{}}
	s.styles = []style{{}}
	s.byStyle[style{}] = 0
	s.grid = [][]cell{s.blank()}
	return s
}

func (s *screen) blank() []cell {
	line := make([]cell, s.width)
	for i := range line {
		line[i] = cell{r: ' '}
	}
	return line
}

func (s *screen) styleIndex() int {
	if i, ok := s.byStyle[s.cur]; ok {
		return i
	}
	s.styles = append(s.styles, s.cur)
	s.byStyle[s.cur] = len(s.styles) - 1
	return len(s.styles) - 1
}

func (s *screen) grow(y int) {
	for len(s.grid) <= y {
		s.grid = append(s.grid, s.blank())
	}
}

var (
	csiRe = regexp.MustCompile(`^\x1b\[([0-9;:?]*)([@-~])`)
	oscRe = regexp.MustCompile(`^\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
)

func (s *screen) write(p []byte) {
	data := append(s.pending, p...)
	s.pending = nil
	for len(data) > 0 {
		switch {
		case data[0] == 0x1b:
			if m := csiRe.FindSubmatch(data); m != nil {
				s.csi(string(m[1]), rune(m[2][0]))
				data = data[len(m[0]):]
				continue
			}
			if m := oscRe.Find(data); m != nil {
				data = data[len(m):]
				continue
			}
			if len(data) < 32 {
				s.pending = append(s.pending[:0], data...)
				return
			}
			data = data[1:]
		case data[0] == '\r':
			s.cx = 0
			data = data[1:]
		case data[0] == '\n':
			s.cy++
			s.grow(s.cy)
			data = data[1:]
		case data[0] == '\b':
			if s.cx > 0 {
				s.cx--
			}
			data = data[1:]
		case data[0] == 0x07:
			data = data[1:]
		default:
			r, size := decodeRune(data)
			if size == 0 {
				s.pending = append(s.pending[:0], data...)
				return
			}
			s.put(r)
			data = data[size:]
		}
	}
}

func (s *screen) put(r rune) {
	if s.cx >= s.width {
		s.cx = 0
		s.cy++
	}
	s.grow(s.cy)
	s.grid[s.cy][s.cx] = cell{r: r, s: s.styleIndex()}
	s.cx++
}

func (s *screen) csi(params string, final rune) {
	if strings.HasPrefix(params, "?") {
		if final == 'h' || final == 'l' {
			for _, mode := range strings.Split(params[1:], ";") {
				if mode == "1049" || mode == "1047" || mode == "47" {
					s.altScreen(final == 'h')
				}
			}
		}
		return // other private modes: cursor visibility, bracketed paste, and so on
	}
	nums := func(def int) []int {
		if params == "" {
			return []int{def}
		}
		var out []int
		for _, f := range strings.Split(params, ";") {
			n, err := strconv.Atoi(strings.SplitN(f, ":", 2)[0])
			if err != nil {
				n = def
			}
			out = append(out, n)
		}
		return out
	}
	switch final {
	case 'A':
		s.cy = max(s.cy-nums(1)[0], 0)
	case 'B':
		s.cy += nums(1)[0]
		s.grow(s.cy)
	case 'C':
		s.cx = min(s.cx+nums(1)[0], s.width-1)
	case 'D':
		s.cx = max(s.cx-nums(1)[0], 0)
	case 'G':
		s.cx = clamp(nums(1)[0]-1, 0, s.width-1)
	case 'H', 'f':
		p := nums(1)
		for len(p) < 2 {
			p = append(p, 1)
		}
		s.cy, s.cx = max(p[0]-1, 0), clamp(p[1]-1, 0, s.width-1)
		s.grow(s.cy)
	case 'J':
		s.grow(s.cy)
		switch nums(0)[0] {
		case 0:
			s.eraseLine(s.cy, s.cx, s.width)
			s.grid = s.grid[:s.cy+1]
		case 1:
			s.eraseLine(s.cy, 0, s.cx+1)
		case 2:
			for y := range s.grid {
				s.eraseLine(y, 0, s.width)
			}
		}
	case 'K':
		s.grow(s.cy)
		switch nums(0)[0] {
		case 0:
			s.eraseLine(s.cy, s.cx, s.width)
		case 1:
			s.eraseLine(s.cy, 0, s.cx+1)
		case 2:
			s.eraseLine(s.cy, 0, s.width)
		}
	case 'm':
		s.sgr(params)
	}
}

// altScreen switches to a blank screen-sized grid and back. 1049 also saves
// and restores the cursor, which the inline renderer relies on: it repaints on
// return by moving up from where it left the cursor.
func (s *screen) altScreen(on bool) {
	if on == s.alt {
		return
	}
	s.alt = on
	if on {
		s.main, s.mainCy, s.mainCx = s.grid, s.cy, s.cx
		s.grid, s.cy, s.cx = nil, 0, 0
		s.grow(rows - 1)
		return
	}
	s.grid, s.cy, s.cx = s.main, s.mainCy, s.mainCx
	s.main = nil
}

func (s *screen) eraseLine(y, from, to int) {
	if y >= len(s.grid) {
		return
	}
	for x := max(from, 0); x < min(to, s.width); x++ {
		s.grid[y][x] = cell{r: ' '}
	}
}

func (s *screen) sgr(params string) {
	if params == "" {
		s.cur = style{}
		return
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			s.cur = style{}
		case n == 1:
			s.cur.Bold = true
		case n == 2:
			s.cur.Dim = true
		case n == 3:
			s.cur.Ital = true
		case n == 4:
			s.cur.Uline = true
		case n == 22:
			s.cur.Bold, s.cur.Dim = false, false
		case n == 23:
			s.cur.Ital = false
		case n == 24:
			s.cur.Uline = false
		case n >= 30 && n <= 37:
			s.cur.Fg = ansi16[n-30]
		case n == 39:
			s.cur.Fg = ""
		case n >= 40 && n <= 47:
			s.cur.Bg = ansi16[n-40]
		case n == 49:
			s.cur.Bg = ""
		case n >= 90 && n <= 97:
			s.cur.Fg = ansi16[8+n-90]
		case n >= 100 && n <= 107:
			s.cur.Bg = ansi16[8+n-100]
		case n == 38 || n == 48:
			colour, used := extended(fields[i+1:])
			if n == 38 {
				s.cur.Fg = colour
			} else {
				s.cur.Bg = colour
			}
			i += used
		}
	}
}

// extended reads a 38/48 colour argument and returns it with how many fields it
// consumed.
func extended(rest []string) (string, int) {
	if len(rest) == 0 {
		return "", 0
	}
	switch rest[0] {
	case "2":
		if len(rest) < 4 {
			return "", len(rest)
		}
		r, g, b := atoi(rest[1]), atoi(rest[2]), atoi(rest[3])
		return fmt.Sprintf("#%02x%02x%02x", r, g, b), 4
	case "5":
		if len(rest) < 2 {
			return "", len(rest)
		}
		return xterm256(atoi(rest[1])), 2
	}
	return "", 1
}

// snapshot renders the grid as rows of styled runs, blank tail trimmed.
func (s *screen) snapshot() [][]run {
	out := make([][]run, 0, len(s.grid))
	for _, line := range s.grid {
		end := len(line)
		for end > 0 && line[end-1].r == ' ' && line[end-1].s == 0 {
			end--
		}
		var runs []run
		for x := 0; x < end; x++ {
			if len(runs) > 0 && runs[len(runs)-1].Style == line[x].s {
				runs[len(runs)-1].Text += string(line[x].r)
				continue
			}
			runs = append(runs, run{Style: line[x].s, Text: string(line[x].r)})
		}
		out = append(out, runs)
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	return out
}

func (s *screen) plain() string {
	var b strings.Builder
	for _, line := range s.grid {
		for _, c := range line {
			b.WriteRune(c.r)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ---------------------------------------------------------------- odds & ends

// ansi16 is the xterm palette for the sixteen named colours. The UI asks for
// hex almost everywhere, so these are mostly a fallback.
var ansi16 = [16]string{
	"#000000", "#cd0000", "#00cd00", "#cdcd00", "#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
	"#7f7f7f", "#ff0000", "#00ff00", "#ffff00", "#5c5cff", "#ff00ff", "#00ffff", "#ffffff",
}

func xterm256(n int) string {
	switch {
	case n < 16:
		return ansi16[n]
	case n < 232:
		n -= 16
		lv := [6]int{0, 95, 135, 175, 215, 255}
		return fmt.Sprintf("#%02x%02x%02x", lv[n/36], lv[(n/6)%6], lv[n%6])
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

func decodeRune(p []byte) (rune, int) {
	r, size := rune(p[0]), 1
	if p[0] >= 0x80 {
		r, size = decodeUTF8(p)
	}
	return r, size
}

func decodeUTF8(p []byte) (rune, int) {
	var want int
	switch {
	case p[0]&0xe0 == 0xc0:
		want = 2
	case p[0]&0xf0 == 0xe0:
		want = 3
	case p[0]&0xf8 == 0xf0:
		want = 4
	default:
		return '?', 1
	}
	if len(p) < want {
		return 0, 0
	}
	r := []rune(string(p[:want]))
	if len(r) == 0 {
		return '?', want
	}
	return r[0], want
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func check(err error) {
	if err != nil {
		die("%v", err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "record-demo: "+format+"\n", args...)
	os.Exit(1)
}
