package sandbox

import "strings"

// normalizeArch maps what a kernel or an environment variable calls a
// processor onto what Go calls it, so the two can be compared. A name this
// build does not recognise comes back empty, which is read as "cannot tell"
// rather than as a mismatch — refusing to start over an unfamiliar spelling
// would be worse than the problem it is guarding against.
func normalizeArch(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "arm64", "aarch64", "armv8b", "armv8l":
		return "arm64"
	case "amd64", "x86_64", "x64":
		return "amd64"
	default:
		return ""
	}
}

// nullTerminated reads a C string out of a fixed-size kernel buffer.
func nullTerminated(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
