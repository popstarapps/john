package policy

import "testing"

func TestAttachmentsDefaultToGuardedAndFailClosedOnAValueItCannotCarryOut(t *testing.T) {
	cases := []struct {
		field             string
		want              string
		allowedPlain      bool
		allowedControlled bool
	}{
		// A backend that predates the field could not carry an attachment at
		// all, so an organization whose replacements were being kept by that
		// accident keeps them on purpose.
		{"", AttachmentsGuarded, true, false},
		{AttachmentsGuarded, AttachmentsGuarded, true, false},
		// An administrator who has read the trade can make the call.
		{AttachmentsOn, AttachmentsOn, true, true},
		{AttachmentsOff, AttachmentsOff, false, false},
		// An instruction this build cannot carry out is not a licence to send
		// more than we were told to.
		{"whatever-comes-next", AttachmentsOff, false, false},
		{"GUARDED", AttachmentsOff, false, false},
	}
	for _, c := range cases {
		snap := &Snapshot{Attach: c.field}
		if got := snap.Attachments(); got != c.want {
			t.Errorf("Attachments(%q) = %q, want %q", c.field, got, c.want)
		}
		if got := snap.AttachmentsAllowed(false); got != c.allowedPlain {
			t.Errorf("AttachmentsAllowed(%q, no text control) = %v, want %v", c.field, got, c.allowedPlain)
		}
		if got := snap.AttachmentsAllowed(true); got != c.allowedControlled {
			t.Errorf("AttachmentsAllowed(%q, under a text control) = %v, want %v",
				c.field, got, c.allowedControlled)
		}
	}
}
