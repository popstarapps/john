//go:build darwin

package keychain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
)

// The macOS Keychain, through /usr/bin/security.
//
// The tool rather than Security.framework, because the framework means cgo or
// a hand-rolled Objective-C runtime call to build CFDictionaries, and this is
// a program every macOS carries. It is also the same shape as the credential
// helpers registryauth already runs: a small program that holds the secret,
// asked for one thing at a time.
//
// The secret never appears in an argument. `security add-generic-password -w
// <secret>` would put a machine token in the process list, where every other
// user on the machine can read it, so the write goes through interactive mode
// and the command text arrives on standard input instead.
//
// Both directions move the secret as hex rather than as text, because
// security's text forms are lossy in ways that only show up on the secrets
// people actually have. Interactive mode reads a line at a time, so a newline
// inside a quoted argument ends the command rather than sitting in the value;
// and `find-generic-password -w` prints hex, unannounced, for anything that is
// not printable ASCII, so a password with an accent in it reads back as its
// own hex digits. -X and -g are the byte-exact forms of the same two calls.
type macKeychain struct{}

func platformBackend() Backend { return macKeychain{} }

const macService = "john"

func (macKeychain) Name() string { return "the macOS Keychain" }

func (macKeychain) Available() bool {
	if !realStoreWanted() {
		return false
	}
	_, err := exec.LookPath(securityTool)
	return err == nil
}

const securityTool = "/usr/bin/security"

func (macKeychain) Get(name string) (string, error) {
	// -g rather than -w: it prints the value as 0x<hex> whenever the bytes are
	// not printable ASCII, and quoted when they are, so the two cases can be
	// told apart. -w prints the same hex digits with nothing to mark them as
	// hex, which is indistinguishable from a password that looks like hex.
	cmd := exec.Command(securityTool, "find-generic-password",
		"-s", macService, "-a", name, "-g")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// 44 is errSecItemNotFound, which is an answer rather than a failure.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", ErrNotFound
		}
		return "", err
	}
	return macPassword(stderr.String())
}

// macPassword pulls the secret out of what `find-generic-password -g` writes
// to standard error, which is one line of the shape
//
//	password: "a printable one"
//	password: 0x6C696E650A74776F  "line\ntwo"
//	password:
//
// The quoted form is the value verbatim between the first quote and the last,
// and cannot span lines, since a newline is one of the things that sends
// security down the hex path. The trailing quoted copy on the hex form is a
// convenience for people reading it and is not escaped — an embedded quote is
// printed as itself — so the hex is what gets decoded.
func macPassword(stderr string) (string, error) {
	const marker = "password:"
	i := strings.Index(stderr, marker)
	if i < 0 {
		return "", errors.New("security: no password in what find-generic-password reported")
	}
	line, _, _ := strings.Cut(stderr[i+len(marker):], "\n")
	line = strings.TrimSuffix(line, "\r")
	line = strings.TrimPrefix(line, " ")

	switch {
	case line == "":
		// An empty secret: security prints the marker and stops.
		return "", nil
	case strings.HasPrefix(line, "0x"):
		digits := line[2:]
		if j := strings.IndexByte(digits, ' '); j >= 0 {
			digits = digits[:j]
		}
		raw, err := hex.DecodeString(digits)
		if err != nil {
			return "", errors.New("security: unreadable password data: " + err.Error())
		}
		return string(raw), nil
	case len(line) >= 2 && strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `"`):
		return line[1 : len(line)-1], nil
	}
	return "", errors.New("security: unreadable password data")
}

func (macKeychain) Set(name, secret string) error {
	// -U updates an existing item rather than refusing to add a second one.
	// -X takes the secret as hex, which is the one form the interactive
	// parser cannot mangle: hex digits hold no quote to close the argument
	// early and no newline to end the command. The quotes around it are for
	// the empty secret, which -X will not accept as a bare empty word.
	cmd := exec.Command(securityTool, "-i")
	cmd.Stdin = strings.NewReader("add-generic-password -U -s " + macQuote(macService) +
		" -a " + macQuote(name) + ` -X "` + hex.EncodeToString([]byte(secret)) + "\"\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errors.New("security: " + strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (macKeychain) Delete(name string) error {
	err := exec.Command(securityTool, "delete-generic-password",
		"-s", macService, "-a", name).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ErrNotFound
	}
	return err
}

// macQuote wraps a value for security's interactive parser, which reads a
// line the way a shell does. The secret goes past it as hex, so what is left
// to quote is the service and the item name — and the name carries a registry
// host, which is someone else's string and can hold a space. Set reads back
// what it wrote, so a parser that disagrees with this is caught rather than
// trusted.
func macQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' || r == '$' || r == '`' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// missingBackendAdvice: /usr/bin/security is part of macOS, so its absence is
// a machine that is not what it says it is.
func missingBackendAdvice() string {
	return "there is no " + securityTool + " on this machine"
}
