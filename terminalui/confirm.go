package terminalui

// confirmView asks a plain question, in the same box the proxy uses to ask a
// hard one.
//
// It exists because some of what john does is irreversible in a way no policy
// covers: resetting a sandbox throws away work that was never applied, and
// applying a diff writes to files the developer has open. Those decisions
// deserve the same treatment as a network request, not a line of output that
// scrolls past.
type confirmView struct {
	choiceBox
	req Confirm
}

func newConfirmView(req Confirm, t *Theme) *confirmView {
	choices := req.Choices
	if len(choices) == 0 {
		choices = DefaultConfirmChoices()
	}
	return &confirmView{choiceBox: newChoiceBox(t, choices), req: req}
}

func (v *confirmView) layout(width int) {
	if width == v.width && v.lines != nil {
		return
	}
	v.width = width
	t := v.theme
	w := max(width, 8)

	var out []string
	for _, l := range wrapPlain(sanitizeBlock(v.req.Question), w) {
		out = append(out, t.Text.Render(l))
	}
	if len(v.req.Detail) > 0 && len(out) > 0 {
		out = append(out, "")
	}
	for n, d := range v.req.Detail {
		conn := t.Symbols.TreeMid
		if n == len(v.req.Detail)-1 {
			conn = t.Symbols.TreeEnd
		}
		for i, l := range wrapPlain(sanitizeBlock(d), w-2) {
			prefix := "  "
			if i == 0 {
				prefix = t.Tree.Render(conn + " ")
			}
			out = append(out, prefix+t.Muted.Render(l))
		}
	}
	v.setLines(out)
}

func (v *confirmView) header(height int) boxHeader {
	label := v.req.Title
	if label == "" {
		label = "CONFIRM"
	}
	return boxHeader{Label: label, Right: v.scrollInfo(height)}
}
