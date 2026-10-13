//go:build !race

package client

// timeScale stretches the e2e wall-clock waits under the race detector; see
// racescale_test.go. A normal run waits exactly as long as it always did.
const timeScale = 1
