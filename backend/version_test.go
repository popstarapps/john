package backend

import (
	"testing"
	"testing/fstest"

	"github.com/popstarapps/john/protocol"
)

func openFrom(version, osArch string) *protocol.SessionOpen {
	return &protocol.SessionOpen{
		Client:   protocol.ClientInfo{Version: version, OS: osArch},
		Protocol: protocol.Version,
	}
}

// A backend holding the fix refuses a client that is not its own build, and
// says which build to fetch.
func TestABackendRefusesAClientItDidNotShip(t *testing.T) {
	clientsFS = fstest.MapFS{
		"john-darwin-arm64.gz": {Data: gzBytes(make([]byte, 64))},
	}
	t.Cleanup(func() { clientsFS = nil })

	srv := &Server{cfg: Config{Version: "0.5.2"}}

	got := srv.versionMismatch(openFrom("0.4.1", "darwin/arm64").Client)
	if got == nil {
		t.Fatal("an out-of-date client was served a session")
	}
	if got.Required != "0.5.2" || got.Client != "0.4.1" {
		t.Fatalf("mismatch = %+v", got)
	}
	if got.Download != "/releases/john-darwin-arm64" {
		t.Fatalf("download path = %q", got.Download)
	}

	// The matching build is served, and the exactness is the point: a client
	// one patch ahead is as wrong as one behind.
	if srv.versionMismatch(openFrom("0.5.2", "darwin/arm64").Client) != nil {
		t.Fatal("its own build was refused")
	}
	if srv.versionMismatch(openFrom("0.5.3", "darwin/arm64").Client) == nil {
		t.Fatal("a newer client was let through — the rule is exact match")
	}
}

// Refusing a client that cannot be fixed would be a locked door with no key:
// `make build` bundles no client downloads, and a backend built that way must
// keep serving whatever turns up.
func TestABackendWithNoBuildToOfferDoesNotRefuse(t *testing.T) {
	clientsFS = nil
	srv := &Server{cfg: Config{Version: "0.5.2"}}

	if got := srv.versionMismatch(openFrom("0.4.1", "darwin/arm64").Client); got != nil {
		t.Fatalf("refused with nothing to offer: %+v", got)
	}

	// Same when downloads exist but not for this developer's platform.
	clientsFS = fstest.MapFS{"john-linux-amd64.gz": {Data: gzBytes(make([]byte, 64))}}
	t.Cleanup(func() { clientsFS = nil })
	if got := srv.versionMismatch(openFrom("0.4.1", "darwin/arm64").Client); got != nil {
		t.Fatalf("refused a platform it has no build for: %+v", got)
	}
	if got := srv.versionMismatch(openFrom("0.4.1", "linux/amd64").Client); got == nil {
		t.Fatal("did not refuse a platform it does have a build for")
	}
}

// Somebody working on john itself has no release to be sent to, on either
// side of the connection.
func TestDevelopmentBuildsAreLeftAlone(t *testing.T) {
	clientsFS = fstest.MapFS{"john-darwin-arm64.gz": {Data: gzBytes(make([]byte, 64))}}
	t.Cleanup(func() { clientsFS = nil })

	released := &Server{cfg: Config{Version: "0.5.2"}}
	for _, v := range []string{"dev", ""} {
		if got := released.versionMismatch(openFrom(v, "darwin/arm64").Client); got != nil {
			t.Fatalf("refused a %q client build: %+v", v, got)
		}
	}

	dev := &Server{cfg: Config{Version: "dev"}}
	if got := dev.versionMismatch(openFrom("0.4.1", "darwin/arm64").Client); got != nil {
		t.Fatalf("a dev backend refused a released client: %+v", got)
	}
}

func TestAnUnparseablePlatformIsNotRefused(t *testing.T) {
	clientsFS = fstest.MapFS{"john-darwin-arm64.gz": {Data: gzBytes(make([]byte, 64))}}
	t.Cleanup(func() { clientsFS = nil })

	srv := &Server{cfg: Config{Version: "0.5.2"}}
	if got := srv.versionMismatch(openFrom("0.4.1", "something-odd").Client); got != nil {
		t.Fatalf("refused a client whose platform could not be read: %+v", got)
	}
}
