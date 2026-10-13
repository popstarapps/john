package client

import (
	"fmt"
	"time"
)

// plural counts n of a thing: "1 file", "3 files". The things counted here
// all take an s.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// toolDuration is how long a command took, the same way every time. Rounded
// to the millisecond, a time that came out whole lost its decimals ("7s"
// beside "6.784s"); a tenth of a second is as much as anyone reads there.
func toolDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

// unappliedDetail warns a reset about the files it would discard.
func unappliedDetail(n int) string {
	if n == 1 {
		return "1 file has not been applied to your working directory. Resetting discards it."
	}
	return fmt.Sprintf("%d files have not been applied to your working directory. Resetting discards them.", n)
}
