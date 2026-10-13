package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/terminalui"
)

// Attachments: the files a prompt carries.
//
// An `@` mention has always been a way to point the model at a file, and for
// a file made of text that is all it needs — the model reads it with the read
// tool, in the sandbox, under every policy that governs a read. A screenshot
// is not that. There is nothing useful for a read tool to return for a PNG,
// and the developer asking about one is not asking the agent to go and look:
// they are showing it something.
//
// So a mention that names an image or a PDF is read here and travels with the
// prompt. Everything else about `@` is unchanged.
//
// Two rules make this something other than a hole in the design:
//
//   - A file inside the workspace is read THROUGH THE SANDBOX, not off the
//     disk. That is the same path the read tool takes, so an excluded path is
//     absent, a protected one is protected, and a symbolic link out of the
//     workspace is refused — none of which would be true of an os.ReadFile
//     here, and all of which an organization has already said it wants.
//   - A file outside the workspace is read off the disk, because a screenshot
//     lives in ~/Desktop and refusing the common case would be refusing the
//     feature. This is the developer handing over their own file by name, not
//     the agent reaching for it: nothing in the loop can ask for one, and the
//     path has to be typed.
//
// Whether any of it is allowed at all is the organization's, in the signed
// snapshot — see policy/attachments.go. The client asks so that a developer
// is told before their file is read; the backend asks again, and its answer
// is the one that decides.

// attachable maps an extension to what a model is told the bytes are. The
// list is short on purpose: these are the types the providers accept, and a
// mention of anything else stays what it has always been — a path, for the
// model to read with a tool.
var attachable = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".pdf":  "application/pdf",
}

// attachableType reports what an attachment of this path would be, or "".
func attachableType(path string) string {
	return attachable[strings.ToLower(filepath.Ext(path))]
}

// mentions returns the `@` mentions in a prompt, in the order written and
// without repeats. The syntax is the one the input box completes: an `@` at
// the start of a word, running to whitespace.
func mentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if !strings.HasPrefix(field, "@") || len(field) == 1 {
			continue
		}
		// Trailing punctuation is sentence, not path: "look at @a.png." names
		// a.png. A trailing dot cannot be part of a name we would attach
		// anyway, because the extension is what makes it one.
		name := strings.TrimRight(field[1:], ".,;:!?)")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// attach reads the files a prompt mentions that a model can look at, and says
// out loud what it did with each one. Anything it cannot attach is reported
// and left as a mention: the prompt still says the file's name, so the model
// can still ask for it or read it with a tool.
func (a *app) attach(ctx context.Context, text string) []protocol.Attachment {
	var named []string
	for _, m := range mentions(text) {
		if attachableType(m) != "" {
			named = append(named, m)
		}
	}
	if len(named) == 0 {
		return nil
	}

	snap := a.snapshot()
	if snap == nil {
		// The proxy blocks every decision-needing request without a snapshot
		// and this is the same rule: an attachment is content on its way to a
		// model, and we cannot say what this organization allows.
		a.notice(terminalui.NoticeWarn,
			"no policy snapshot · "+strings.Join(named, ", ")+" not attached (the prompt still names it)")
		return nil
	}
	if !snap.AttachmentsAllowed(snap.ReplacementsActive) {
		a.notice(terminalui.NoticeWarn, attachmentsRefused(snap)+
			" · "+strings.Join(named, ", ")+" not attached (the prompt still names it)")
		return nil
	}

	limit := a.attachmentLimit()
	var out []protocol.Attachment
	total := 0
	for _, name := range named {
		data, err := a.readAttachment(ctx, name, limit)
		if err != nil {
			a.notice(terminalui.NoticeWarn, "could not attach "+name+": "+err.Error())
			continue
		}
		if total+len(data) > limit {
			a.notice(terminalui.NoticeWarn, fmt.Sprintf(
				"could not attach %s: a prompt carries at most %s of files, and this one is over it",
				name, byteSize(limit)))
			continue
		}
		total += len(data)
		out = append(out, protocol.Attachment{
			Name:     filepath.Base(name),
			MimeType: attachableType(name),
			Data:     base64.StdEncoding.EncodeToString(data),
		})
		a.notice(terminalui.NoticeInfo, fmt.Sprintf("attached %s (%s) · the model sees the file, not the path",
			filepath.Base(name), byteSize(len(data))))
	}
	return out
}

// attachmentsRefused says which half of the policy refused, because "not
// allowed" is not something a developer can do anything with.
func attachmentsRefused(snap *policy.Snapshot) string {
	if snap.Attachments() == policy.AttachmentsOff {
		return "attachments are switched off for this organization"
	}
	return "this session redacts what reaches the model, and that cannot be done inside an image"
}

// readAttachment reads one named file, through the sandbox when it is in the
// workspace and off the disk when it is not. See the note at the top.
func (a *app) readAttachment(ctx context.Context, name string, limit int) ([]byte, error) {
	path := name
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.workdir, path)
	}
	path = filepath.Clean(path)

	if rel, err := filepath.Rel(a.workdir, path); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		content, err := a.sb.Read(ctx, filepath.ToSlash(rel), limit+1)
		if err != nil {
			return nil, err
		}
		if content.Truncated || len(content.Data) > limit {
			return nil, fmt.Errorf("it is larger than the %s a prompt may carry", byteSize(limit))
		}
		return content.Data, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("it is larger than the %s a prompt may carry", byteSize(limit))
	}
	return os.ReadFile(path)
}

// attachmentLimit is the number in force for this session: what the two ends
// agreed on, or this client's own statement before a session is open.
func (a *app) attachmentLimit() int {
	a.mu.Lock()
	limit := a.limits.MaxAttachmentBytes
	a.mu.Unlock()
	if limit <= 0 {
		limit = clientLimits.MaxAttachmentBytes
	}
	return limit
}

// snapshot is the policy in force, or nil when this client is holding none.
func (a *app) snapshot() *policy.Snapshot {
	if a.np == nil {
		return nil
	}
	return a.np.store.Current()
}

func byteSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
