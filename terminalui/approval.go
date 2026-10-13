package terminalui

import (
	"fmt"
	"strings"
)

// approvalView is the buffered-request inspector. The proxy has the whole
// request in hand before anything leaves the machine, so this shows the whole
// request: method, URL, every header, and as much body as was captured.
//
// It is drawn as a box with the request scrolling inside it and the choices as
// buttons along the bottom, because this is the one moment in a session where
// the developer is being asked to decide something rather than watch it.
type approvalView struct {
	choiceBox
	req ApprovalRequest
}

func newApprovalView(req ApprovalRequest, t *Theme) *approvalView {
	choices := req.Options
	if len(choices) == 0 {
		choices = DefaultApprovalOptions()
	}
	return &approvalView{choiceBox: newChoiceBox(t, choices), req: req}
}

// layout renders the request into lines of the given (inner) width.
func (v *approvalView) layout(width int) {
	if width == v.width && v.lines != nil {
		return
	}
	v.width = width
	t := v.theme
	w := max(width, 8)
	var out []string

	add := func(s string) { out = append(out, s) }

	if v.req.Verdict != "" || v.req.Policy != "" {
		verdict := v.req.Verdict
		if verdict == "" {
			verdict = "ask developer"
		}
		line := t.Danger.Render(sanitizeLine(verdict))
		if v.req.Policy != "" {
			line += t.Muted.Render(" " + t.Symbols.Dot + " " + sanitizeLine(v.req.Policy))
		}
		add(line)
	}

	if len(v.req.Headers) > 0 {
		add("")
		add(t.PanelHead.Render("headers"))
		for _, h := range v.req.Headers {
			label := "  " + h.Name + ": "
			vlines := wrapPlain(sanitizeLine(h.Value), max(w-len(label), 8))
			if len(vlines) == 0 {
				vlines = []string{""}
			}
			add("  " + t.DiffPath.Render(sanitizeLine(h.Name)+": ") + t.Muted.Render(vlines[0]))
			for _, l := range vlines[1:] {
				add(strings.Repeat(" ", len(label)) + t.Muted.Render(l))
			}
		}
	}

	if v.req.Body != "" {
		add("")
		title := fmt.Sprintf("body (%d bytes)", len(v.req.Body))
		if v.req.BodyTruncated {
			title = fmt.Sprintf("body (first %d bytes)", len(v.req.Body))
		}
		add(t.PanelHead.Render(title))
		for _, raw := range strings.Split(sanitizeBlock(v.req.Body), "\n") {
			for _, l := range wrapPlain(raw, max(w-2, 8)) {
				add("  " + t.Text.Render(l))
			}
		}
		if v.req.BodyTruncated {
			add("  " + t.Muted.Render(t.Symbols.Ellipsis+" truncated"))
		}
	}

	v.setLines(out)
}

// header is what the title row says: what is being asked, and about what.
func (v *approvalView) header(height int) boxHeader {
	return boxHeader{
		Label:  "APPROVAL REQUIRED",
		Detail: v.req.Method + " " + v.req.URL,
		Right:  v.scrollInfo(height),
	}
}
