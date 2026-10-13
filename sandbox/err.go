package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/popstarapps/john/policy"
)

// Errors cross the guest link as text, which loses their type — and one type
// must not be lost. A *policy.ProtectedError is a decision, not a failure:
// the client turns it into a blocked tool result that names the rule, so the
// model learns the protection instead of retrying it, and client/protect_test
// asserts exactly that. A refusal that arrived on the host as a plain string
// would be reported to the model as a broken sandbox.
//
// fs.ErrNotExist travels for the same reason at a smaller scale: the union
// asks the lower layer about paths that are simply not there, on nearly every
// call, and "not there" is an answer rather than a problem.

// wireError is the encoded form. Kind is the discriminator; a kind this build
// does not know decodes to a plain error carrying the message, which is the
// safe direction — an unrecognised refusal stays a refusal.
type wireError struct {
	Kind     string `json:"kind"`
	Msg      string `json:"msg"`
	Path     string `json:"path,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	PolicyID string `json:"policy_id,omitempty"`
}

func encodeRemoteError(err error) string {
	we := wireError{Kind: "error", Msg: err.Error()}
	var pe *policy.ProtectedError
	switch {
	case errors.As(err, &pe):
		we = wireError{Kind: "protected", Msg: pe.Error(), Path: pe.Path,
			Mode: pe.Mode, Pattern: pe.Pattern, PolicyID: pe.PolicyID}
	case errors.Is(err, fs.ErrNotExist):
		we.Kind = "notexist"
	}
	raw, merr := json.Marshal(we)
	if merr != nil {
		// The error must still reach the caller even if it will not encode.
		return `{"kind":"error","msg":` + quoteJSON(err.Error()) + `}`
	}
	return string(raw)
}

func decodeRemoteError(s string) error {
	var we wireError
	if err := json.Unmarshal([]byte(s), &we); err != nil || we.Msg == "" {
		// Not something this side encoded: keep it whole rather than
		// inventing structure for it.
		return errors.New(s)
	}
	switch we.Kind {
	case "protected":
		return &policy.ProtectedError{Path: we.Path, Mode: we.Mode,
			Pattern: we.Pattern, PolicyID: we.PolicyID}
	case "notexist":
		return fmt.Errorf("%s: %w", we.Msg, fs.ErrNotExist)
	default:
		return errors.New(we.Msg)
	}
}

// quoteJSON quotes a string for the hand-built JSON fallback above.
func quoteJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(raw)
}
