package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/terminalui"
)

// A backend that is briefly gone — a deploy, a restart — can outlast the token
// this machine holds, and then there is no session to start until it answers
// again. Most of those gaps last seconds, so on a terminal john waits them out
// where the developer can see it and starts the moment the backend is back.
// Past a couple of minutes it is not a blip, and it says so and stops.

// Variables so tests can wait in milliseconds.
var (
	backendWaitFirst = time.Second
	backendWaitMax   = 15 * time.Second
	backendWaitLimit = 2 * time.Minute
	backendWaitFrame = 100 * time.Millisecond
)

// waitForBackend calls try until it stops reporting the backend unavailable,
// the limit runs out, or ctx ends. What try last said is what comes back, so
// a wait that runs out reports the outage exactly as a run without a terminal
// would.
func waitForBackend(ctx context.Context, out io.Writer, theme *terminalui.Theme,
	try func() (*credstore.Credentials, error)) (*credstore.Credentials, error) {
	c, err := try()
	var down *BackendUnavailableError
	if !errors.As(err, &down) {
		return c, err
	}

	host := down.URL
	if u, perr := url.Parse(down.URL); perr == nil && u.Host != "" {
		host = u.Host
	}
	defer fmt.Fprint(out, "\r\x1b[K")

	deadline := time.Now().Add(backendWaitLimit)
	wait, frame := backendWaitFirst, 0
	for time.Now().Before(deadline) {
		next := time.Now().Add(wait)
		if next.After(deadline) {
			next = deadline
		}
		for now := time.Now(); now.Before(next); now = time.Now() {
			secs := int(next.Sub(now).Round(time.Second) / time.Second)
			spin := terminalui.BusyPreview(theme, terminalui.SpinnerBraille, terminalui.EffectNone, "", frame)
			fmt.Fprintf(out, "\r\x1b[K%sBackend at %s is unavailable (%s) — retrying in %ds · Ctrl-C to quit",
				spin, host, down.Reason(), secs)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backendWaitFrame):
			}
			frame++
		}

		fmt.Fprintf(out, "\r\x1b[K%sBackend at %s is unavailable (%s) — retrying now",
			terminalui.BusyPreview(theme, terminalui.SpinnerBraille, terminalui.EffectNone, "", frame),
			host, down.Reason())
		c, err = try()
		if !errors.As(err, &down) {
			return c, err
		}
		wait = min(wait*2, backendWaitMax)
	}
	return nil, err
}
