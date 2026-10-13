package policy

// What a prompt may carry besides text.
//
// A developer can hand a model a screenshot or a PDF, and the useful cases
// are ordinary ones: a rendering that is wrong, a stack trace photographed
// off another machine, the specification a change is meant to implement.
//
// The reason it is a policy decision at all is that john's two content
// controls read text. Replacements rewrite what is on its way to a model and
// keyword routing refuses to send at all until the session is narrowed —
// both by looking at the words. Neither can see inside a PNG. An organization
// that redacts customer names from prompts is not redacting them from a
// screenshot of the same screen, and nothing about the way that fails is
// visible: the attachment goes, the alias table is untouched, and the trace
// records a file rather than a leak.
//
// So the question an administrator answers here is not "do we trust our
// developers" — it is "does the control we wrote still mean what we think it
// means when the content is pixels". Three answers, and the default is the
// one that keeps the promise the controls make.
const (
	// AttachmentsGuarded allows attachments except in a session whose content
	// is under a text control — a replacement policy or a routing keyword. It
	// is the default, and the only value that changes behaviour depending on
	// what else an organization has written: an organization with no such
	// policy has nothing for an image to walk around, and one that has them
	// gets them kept rather than quietly bypassed.
	AttachmentsGuarded = "guarded"
	// AttachmentsOn allows attachments always. An administrator has read the
	// trade above and made the call; the controls still run on the text.
	AttachmentsOn = "on"
	// AttachmentsOff refuses them everywhere. Prompts are text.
	AttachmentsOff = "off"
)

// Attachments is what the snapshot says about files on a prompt.
//
// Unset means guarded, which is also what a backend that predates the field
// meant: it had no way to send an attachment at all, so refusing one under a
// text control is the behaviour its administrators already had. A value this
// build does not recognise means off — an instruction we cannot carry out is
// not a licence to send more than we were told to, the same direction the
// rest of the policy engine fails in.
func (s *Snapshot) Attachments() string {
	switch s.Attach {
	case "":
		return AttachmentsGuarded
	case AttachmentsGuarded, AttachmentsOn, AttachmentsOff:
		return s.Attach
	default:
		return AttachmentsOff
	}
}

// AttachmentsAllowed answers the question a client asks before it reads a
// file off the developer's disk: may this prompt carry one at all.
//
// underTextControl is what the caller knows about the session's content
// controls. The client knows about replacements, because the snapshot says
// so; the backend also knows about routing keywords, which never leave it.
// That asymmetry is deliberate and it is why both sides ask: the client asks
// so a developer is told before the bytes are read, and the backend asks
// because it is the side that can answer completely.
func (s *Snapshot) AttachmentsAllowed(underTextControl bool) bool {
	switch s.Attachments() {
	case AttachmentsOn:
		return true
	case AttachmentsOff:
		return false
	default:
		return !underTextControl
	}
}
