package keychain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

// The one test that talks to the machine's actual keychain.
//
// It is skipped unless JOHN_KEYCHAIN_TEST=1, because a test that wrote to a
// developer's real credential store on every `go test ./...` would be a test
// that prompts, leaves rubbish behind, and — on the wrong day — overwrites the
// credential they are signed in with. So it is a thing you run on purpose:
//
//	JOHN_KEYCHAIN_TEST=1 go test ./keychain -run TheRealStore -v
//
// Run it once on each of macOS, Windows and a Linux desktop. It is the only
// thing that can tell you the three backends work, since no machine has more
// than one of them and the rest of this package's tests deliberately never
// leave the process.
//
// What it writes is named for this run and deleted at the end, so it cannot
// touch the machine-token or registry: items that credstore and registryauth
// keep. To look at those by hand afterwards:
//
//	macOS    security find-generic-password -s john -a machine-token -w
//	Linux    secret-tool lookup service john account machine-token
//	Windows  cmdkey /list | findstr john        (names the item, not the secret)
//
// A Linux box with no desktop can still run it, and no root is needed. Fetch
// the packages, unpack them somewhere, and start a keyring of your own:
//
//	apt-get download libsecret-tools libsecret-1-0 gnome-keyring \
//	                 libgck-1-0 libgcr-base-3-1
//	for d in *.deb; do dpkg-deb -x $d root; done
//	export PATH=$PWD/root/usr/bin:$PATH
//	export LD_LIBRARY_PATH=$PWD/root/usr/lib/$(gcc -dumpmachine)
//	export XDG_DATA_HOME=$PWD/xdg          # keeps the keyring out of ~
//	echo -n pw | gnome-keyring-daemon --unlock --components=secrets --daemonize
//
// XDG_DATA_HOME is the part worth copying: gnome-keyring keeps its keyrings
// under it, so setting it puts the whole thing in a directory you can delete
// rather than in the developer's own. `pkill gnome-keyring-daemon` afterwards.
func TestTheRealStoreOnThisMachine(t *testing.T) {
	if os.Getenv("JOHN_KEYCHAIN_TEST") == "" {
		t.Skip("set JOHN_KEYCHAIN_TEST=1 to exercise this machine's real keychain")
	}
	b := platformBackend()
	if !b.Available() {
		// Asked for on purpose and not there is a failure, not a skip: the
		// point of running this is to find out, and "no keychain" is an
		// answer that needs acting on rather than passing over.
		t.Fatalf("no keychain on this machine — %s", missingKeychainAdvice())
	}
	t.Logf("keychain: %s", b.Name())
	defer Use(b)()

	var suffix [6]byte
	rand.Read(suffix[:])
	name := "selftest-" + hex.EncodeToString(suffix[:])
	t.Cleanup(func() { b.Delete(name) })

	// A name that was never written is not found, rather than an error about
	// the store. Everything that reads a credential depends on telling those
	// two apart: the first means "this machine has not signed in".
	if _, err := Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unwritten item answered %v, want not found", err)
	}

	// The secrets that actually go in here are a machine token and whatever a
	// person typed as a registry password, so the awkward one is the password.
	// Quotes and backslashes are what a shell-quoted write mangles, spaces are
	// what an unquoted one truncates, and the rest are here because a keychain
	// that stores bytes should store bytes.
	for _, tc := range []struct{ what, secret string }{
		{"a machine token", "tok_" + strings.Repeat("aA1", 30)},
		{"quotes and a backslash", `pa"ss'wo\rd`},
		{"spaces and shell metacharacters", "two words $HOME `id` ${x} #comment"},
		{"unicode", "påsswørd·æøå·日本語"},
		{"a newline in the middle", "line one\nline two"},
		{"a long one", strings.Repeat("x", 2000)},
	} {
		t.Run(tc.what, func(t *testing.T) {
			// Set is the package-level one, so this exercises the read-back
			// that the fallback depends on: if it returns an error here, the
			// backend is wrong in a way john would route around rather than
			// lose a credential over — but it is still wrong.
			if err := Set(name, tc.secret); err != nil {
				t.Fatalf("set: %v", err)
			}
			got, err := Get(name)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got != tc.secret {
				t.Fatalf("came back as %q, want %q", got, tc.secret)
			}
		})
	}

	// Writing again replaces rather than adding a second item beside it,
	// which is what every token refresh does.
	if err := Set(name, "second"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got, _ := Get(name); got != "second" {
		t.Fatalf("after an overwrite the store still says %q", got)
	}

	if err := Delete(name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := Get(name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v — the item outlived it", err)
	}
	// And again, because signing out twice is not an error.
	if err := Delete(name); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
