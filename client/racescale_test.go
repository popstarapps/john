//go:build race

package client

// The race detector slows everything the e2e sessions drive — PTY, backend,
// sandbox, agent loop — by several times over. The waits in those tests are
// wall-clock, so they are stretched to match rather than failing on a machine
// that is doing the extra work. Without this the client package cannot be run
// under -race at all, which is how the wire.conn race went unseen.
const timeScale = 6
