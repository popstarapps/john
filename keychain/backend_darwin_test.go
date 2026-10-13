//go:build darwin

package keychain

import "testing"

// The samples are what /usr/bin/security actually printed for each of these
// secrets, kept here so the parser can be changed without a machine to try it
// on. The awkward ones are the reason it exists: security switches to hex for
// anything that is not printable ASCII — which includes a backslash — and the
// human-readable copy it prints beside the hex does not escape a quote, so the
// hex is the only part that can be believed.
func TestReadingWhatSecurityPrints(t *testing.T) {
	for _, tc := range []struct {
		what, stderr, want string
	}{
		{"a printable one", `password: "tok_abc123"` + "\n", "tok_abc123"},
		{"empty", "password: \n", ""},
		{"one that looks like hex", `password: "6e6f"` + "\n", "6e6f"},
		{"a quote only", `password: """` + "\n", `"`},
		{"unicode", "password: 0x70C3A5737377  \"p\\303\\245ssw\"\n", "påssw"},
		{
			"a newline, whose readable copy runs onto the next line",
			"password: 0x6C696E65206F6E650A6C696E652074776F  \"line one\nline two\"\n",
			"line one\nline two",
		},
		{
			"a quote inside the hex form, printed unescaped beside it",
			"password: 0x706122735C  \"pa\"s\\134\"\n",
			`pa"s\`,
		},
		{"attribute lines before it", "keychain: \"/x\"\nclass: \"genp\"\npassword: \"tok\"\n", "tok"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := macPassword(tc.stderr)
			if err != nil {
				t.Fatalf("macPassword: %v", err)
			}
			if got != tc.want {
				t.Fatalf("read %q, want %q", got, tc.want)
			}
		})
	}
}

// A backend that cannot say what the secret was must say so, rather than hand
// back something shorter than what was stored.
func TestSecurityPrintingSomethingElse(t *testing.T) {
	for _, tc := range []struct{ what, stderr string }{
		{"no password at all", "keychain: \"/x\"\nclass: \"genp\"\n"},
		{"hex that is not hex", "password: 0xZZ\n"},
		{"neither quoted nor hex", "password: bare\n"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if got, err := macPassword(tc.stderr); err == nil {
				t.Fatalf("read %q, want an error", got)
			}
		})
	}
}
