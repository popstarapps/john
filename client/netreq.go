package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/popstarapps/john/proxy"
	"github.com/popstarapps/john/terminalui"
)

// One question per host per session, unless the developer narrows it.
//
// A command that is blocked is rarely blocked once: apt fetches three indexes
// and then a hundred packages, a retry repeats the lot. Asking about each
// would be a dialog per URL, and filing each would hand an administrator a
// pile of requests for one decision. So the first block on a host asks, and
// blocks that arrive while the question is on screen join it.
//
// The answer says how far the request reaches. The whole host — usually what
// a tool needs — and later blocks on the host are added to that request
// without asking. Only this path — for the developer who knows one endpoint
// is all they want — and a block on another path of the host is a different
// question, asked in its turn. A "no" is not asked about again this session,
// and neither is a request that has since been decided — for the methods it
// asked for. A request is for what was seen: a host that was only ever read
// from is asked about for GET, and the POST that comes after the decision is
// a request of its own, asked in its turn. One that comes while the request
// is still open joins it.

type askState int

const (
	askAsking askState = iota
	askFiled
	askDeclined
	askClosed // filed, and since decided: not re-filed unasked
)

const (
	askHost = "host"
	askPath = "path"
	askNo   = "no"
)

type hostAsk struct {
	state   askState
	scope   string          // askHost or askPath, once filed
	paths   map[string]bool // with askPath: the paths asked for
	methods map[string]bool // the methods asked for, all requests together
	pending []proxy.Outcome // blocked while the question was on screen
}

// hostKey is what makes two blocked requests "the same host": scheme, host
// and port, as a network policy matches them.
func hostKey(raw string) (key, host string) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, raw
	}
	return u.Scheme + "://" + u.Host, u.Hostname()
}

func urlPath(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		return u.Path
	}
	return "/"
}

// blockedUnknown is what happens after the proxy refuses a request no policy
// covers.
func (a *app) blockedUnknown(o proxy.Outcome) {
	key, _ := hostKey(o.URL)
	a.mu.Lock()
	if a.hostAsks == nil {
		a.hostAsks = map[string]*hostAsk{}
	}
	h := a.hostAsks[key]
	if h == nil {
		h = &hostAsk{}
		a.hostAsks[key] = h
		h.state = askAsking
		a.mu.Unlock()
		a.askForHost(key, h, o)
		return
	}
	switch {
	case h.state == askAsking:
		h.pending = append(h.pending, o)
		a.mu.Unlock()
	case h.state == askFiled && (h.scope == askHost || h.paths[urlPath(o.URL)]):
		exact := h.scope == askPath
		a.mu.Unlock()
		_, _, err := a.fileAccessRequest(o, exact, true)
		if errors.Is(err, errNothingOpen) {
			// Decided since it was filed. What it asked for stays quiet; a
			// method it never asked for is a question of its own.
			a.mu.Lock()
			h.state = askClosed
			a.mu.Unlock()
			a.blockedUnknown(o)
			return
		}
		if err == nil {
			a.mu.Lock()
			h.asked(o.Method)
			a.mu.Unlock()
		}
	case h.state == askFiled:
		// Another path on a host filed path by path: its own question.
		h.state = askAsking
		a.mu.Unlock()
		a.askForHost(key, h, o)
	case h.state == askClosed && !h.methods[o.Method]:
		// The decided request was for other methods: npm reads the registry
		// with GET and then posts its audit. Asked about once, like any
		// first block on a host.
		h.state = askAsking
		a.mu.Unlock()
		a.askForHost(key, h, o)
	default: // declined, or decided for this method
		a.mu.Unlock()
	}
}

// asked records a method as asked for on this host. The caller holds a.mu.
func (h *hostAsk) asked(method string) {
	if h.methods == nil {
		h.methods = map[string]bool{}
	}
	h.methods[method] = true
}

// askForHost puts the question, files the answer, and deals with what was
// blocked on the host while it waited.
func (a *app) askForHost(key string, h *hostAsk, o proxy.Outcome) {
	_, host := hostKey(o.URL)
	path := urlPath(o.URL)
	id := fmt.Sprintf("ask-policy-%d", a.nextID())
	reply := make(chan terminalui.ConfirmDecision, 1)
	a.mu.Lock()
	a.confirms[id] = reply
	a.mu.Unlock()
	question := "Ask an administrator to allow " + host + "?"
	a.mu.Lock()
	if asked := sortedKeys(h.methods); len(asked) > 0 {
		// Asked about before, for other methods: say so, or it reads as a
		// host nobody has heard of.
		question = "Ask an administrator to allow " + o.Method + " to " + host + " too? " +
			"It was asked for " + strings.Join(asked, ", ") + "."
	}
	a.mu.Unlock()
	a.ui.Send(terminalui.Confirm{
		ID:       id,
		Title:    "REQUEST A POLICY",
		Question: question,
		Detail: []string{
			o.Method + " " + o.URL,
			"This request was blocked. Filing a request does not unblock it — it asks for a policy for next time.",
			"The whole host is what most tools need; anything else blocked on it while the request is open joins it.",
		},
		Choices: []terminalui.Choice{
			// The usual answer, where Enter lands: asking commits nothing.
			{Key: 'h', Label: "the whole host", Decision: askHost, Default: true},
			{Key: 'p', Label: "only " + path, Decision: askPath},
			{Key: 'n', Label: "no", Decision: askNo, Cancel: true},
		},
	})
	dec := <-reply

	a.mu.Lock()
	pending := h.pending
	h.pending = nil
	if dec.Decision != askHost && dec.Decision != askPath {
		h.state = askDeclined
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	exact := dec.Decision == askPath
	ref, merged, err := a.fileAccessRequest(o, exact, false)
	if err != nil {
		a.mu.Lock()
		if h.scope == "" {
			delete(a.hostAsks, key) // never filed: the next block may ask again
		} else {
			h.state = askFiled
		}
		a.mu.Unlock()
		a.notice(terminalui.NoticeError, "could not file the request: "+err.Error())
		return
	}

	a.mu.Lock()
	h.state = askFiled
	h.asked(o.Method)
	if exact && h.scope != askHost {
		h.scope = askPath
		if h.paths == nil {
			h.paths = map[string]bool{}
		}
		h.paths[path] = true
	} else {
		h.scope = askHost
	}
	a.mu.Unlock()

	// What was blocked while the question was up: added to this request when
	// it covers them, asked about in turn when it does not.
	covered := 1
	for _, p := range pending {
		if !exact || urlPath(p.URL) == path {
			a.fileAccessRequest(p, exact, true)
			a.mu.Lock()
			h.asked(p.Method)
			a.mu.Unlock()
			covered++
		} else {
			go a.blockedUnknown(p)
		}
	}

	what := host
	if exact {
		what = host + path
	}
	covers := ""
	if covered > 1 {
		covers = fmt.Sprintf(" · it covers the %d requests blocked so far", covered)
	}
	if merged {
		a.notice(terminalui.NoticeInfo, "added to "+ref+", already waiting for an administrator"+covers)
		return
	}
	a.notice(terminalui.NoticeInfo, "network access request "+ref+" filed for "+what+
		" · an administrator will decide it"+covers)
}

// errNothingOpen is a merge-only filing that found no open request to join.
var errNothingOpen = errors.New("no open request for this host")

// fileAccessRequest posts a network access request built from a blocked
// request: for the whole host, or with exactPath for that path alone. The
// backend adds it to the requester's open request it belongs with when there
// is one; with mergeOnly it does nothing else.
func (a *app) fileAccessRequest(o proxy.Outcome, exactPath, mergeOnly bool) (ref string, merged bool, err error) {
	spec, err := proxy.ProposedPolicy(o, exactPath)
	if err != nil {
		return "", false, err
	}
	title := "Access to " + spec.Host
	if exactPath {
		title += spec.Path
	}
	body, _ := json.Marshal(map[string]any{
		"title":     title,
		"blocked":   o.Method + " " + o.URL,
		"teamId":    "",
		"groupId":   nil,
		"groupName": "",
		"spec":      spec,
		"mergeOnly": mergeOnly,
	})

	req, err := http.NewRequest("POST", a.wire.url+"/api/v1/access-requests", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.wire.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if mergeOnly && resp.StatusCode == http.StatusNotFound {
		return "", false, errNothingOpen
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("backend returned %d", resp.StatusCode)
	}
	var out struct {
		Ref    string `json:"ref"`
		Merged bool   `json:"merged"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Ref, out.Merged, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
