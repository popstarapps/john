package trace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// fixture is the shared trace file both sides of the wire read: this package
// writes the format, and the console decrypts it in a browser. Keeping one
// blob that both must open is what stops the two implementations drifting
// apart quietly — a change to the format here fails there, and the reverse.
//
// The console reads the same file from src/lib/__fixtures__.
type fixture struct {
	OrgIdentity      string   `json:"orgIdentity"`
	UserIdentity     string   `json:"userIdentity"`
	StrangerIdentity string   `json:"strangerIdentity"`
	File             string   `json:"file"`
	ExpectedKinds    []string `json:"expectedKinds"`
	ExpectedPrompt   string   `json:"expectedPrompt"`
}

func loadFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join("..", "console", "src", "lib", "__fixtures__", "trace-fixture.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shared fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return &f
}

// TestSharedFixtureStillOpens fails when the format changes under the console.
// If it does, the console's decryption needs the same change, and the fixture
// needs regenerating for both.
func TestSharedFixtureStillOpens(t *testing.T) {
	f := loadFixture(t)

	for name, key := range map[string]string{
		"organization": f.OrgIdentity,
		"developer":    f.UserIdentity,
	} {
		identity, err := age.ParseX25519Identity(key)
		if err != nil {
			t.Fatal(err)
		}
		records, err := Records(strings.NewReader(f.File), identity)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(records) != len(f.ExpectedKinds) {
			t.Fatalf("%s read %d records, want %d", name, len(records), len(f.ExpectedKinds))
		}
		for i, want := range f.ExpectedKinds {
			if records[i].Kind != want {
				t.Fatalf("%s record %d = %q, want %q", name, i, records[i].Kind, want)
			}
		}
		if !bytes.Contains(records[1].Payload, []byte(f.ExpectedPrompt)) {
			t.Fatalf("%s: prompt payload = %s", name, records[1].Payload)
		}
	}

	stranger, err := age.ParseX25519Identity(f.StrangerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Records(strings.NewReader(f.File), stranger); err == nil {
		t.Fatal("an unrelated identity opened the fixture")
	}
}
