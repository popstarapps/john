package client

import "testing"

// No test opens a browser window on the developer's screen.
//
// The sign-in flow launches one, which is right for a developer and wrong for
// everything else: a test run that opens tabs is one nobody will run twice,
// and on a build machine it is at best useless and at worst a hang. The URL is
// still printed, which is the part the sign-in tests actually check.
func TestMain(m *testing.M) {
	openBrowser = func(string) {}
	m.Run()
}
