package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// console is the standalone backend's admin API, used the way the console in
// a browser uses it: signed in through /auth/login, which standalone's
// identity provider completes without asking, and then the same /api/v1
// calls. It is the harness standing in for the developer's browser — adding
// the model, approving what the agent asked for.
type console struct {
	base string
	http *http.Client
}

func signIn(base string) (*console, error) {
	jar, _ := cookiejar.New(nil)
	c := &console{base: base, http: &http.Client{Jar: jar, Timeout: 2 * time.Minute}}
	resp, err := c.http.Get(base + "/auth/login?next=/")
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	var viewer map[string]any
	if err := c.call("GET", "/viewer", nil, &viewer); err != nil {
		return nil, fmt.Errorf("signed in, but the console does not know who: %w", err)
	}
	return c, nil
}

func (c *console) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type catalogModel struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

// addModel adds a model from the catalog the way the console's dialog does:
// tested first, saved only once it answers. It returns the model's name and
// the id the console gave it, which is what a routing policy names.
func (c *console) addModel(want, apiKey string) (name, id string, err error) {
	var catalog []catalogModel
	if err := c.call("GET", "/models/catalog", nil, &catalog); err != nil {
		return "", "", err
	}
	var chosen *catalogModel
	var ids []string
	for i, m := range catalog {
		if m.Provider != "anthropic" {
			continue
		}
		ids = append(ids, m.ID)
		if chosen == nil && strings.Contains(m.ID, want) {
			chosen = &catalog[i]
		}
	}
	if chosen == nil {
		return "", "", fmt.Errorf("no Anthropic model in the catalog matches %q; it has %s", want, strings.Join(ids, ", "))
	}
	input := map[string]any{
		"name": chosen.Name, "provider": "anthropic", "hosting": "external", "endpoint": "",
		"modelId": chosen.ID, "contextWindow": 0, "costPerMTokIn": 0, "costPerMTokCachedIn": 0,
		"costPerMTokOut": 0, "apiKey": apiKey, "extra": map[string]string{},
	}
	var test struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.call("POST", "/models/test", input, &test); err != nil {
		return "", "", err
	}
	if !test.OK {
		return "", "", fmt.Errorf("the model test failed: %s", test.Error)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := c.call("POST", "/models", input, &created); err != nil {
		return "", "", err
	}
	return chosen.Name, created.ID, nil
}

type accessRequest struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Proposed struct {
		GroupID *string `json:"groupId"`
		Spec    struct {
			Host string `json:"host"`
		} `json:"spec"`
	} `json:"proposed"`
}

func (c *console) openRequests() ([]accessRequest, error) {
	var reqs []accessRequest
	err := c.call("GET", "/access-requests?status=open", nil, &reqs)
	return reqs, err
}

// approve approves a request as proposed, into the group it proposes or, when
// it proposes none, the baseline group — which is what an administrator on a
// standalone machine, the only person there, would pick.
func (c *console) approve(r accessRequest) error {
	group := ""
	if r.Proposed.GroupID != nil {
		group = *r.Proposed.GroupID
	}
	if group == "" {
		var groups []struct {
			ID       string `json:"id"`
			Baseline bool   `json:"baseline"`
		}
		if err := c.call("GET", "/groups", nil, &groups); err != nil {
			return err
		}
		for _, g := range groups {
			if g.Baseline {
				group = g.ID
				break
			}
		}
		if group == "" && len(groups) > 0 {
			group = groups[0].ID
		}
	}
	body := map[string]any{"status": "approved"}
	if group != "" {
		body["groupId"] = group
	}
	return c.call("POST", "/access-requests/"+r.ID+"/decision", body, nil)
}

// requests lists access requests in one status: open, approved or rejected.
func (c *console) requests(status string) ([]accessRequest, error) {
	var reqs []accessRequest
	err := c.call("GET", "/access-requests?status="+status, nil, &reqs)
	return reqs, err
}

// reject turns a request down, the other answer an administrator has.
func (c *console) reject(r accessRequest) error {
	return c.call("POST", "/access-requests/"+r.ID+"/decision", map[string]any{"status": "rejected"}, nil)
}

// baselineGroup is the policy group everyone is in — on a standalone machine,
// the developer.
func (c *console) baselineGroup() (string, error) {
	var groups []struct {
		ID       string `json:"id"`
		Baseline bool   `json:"baseline"`
	}
	if err := c.call("GET", "/groups", nil, &groups); err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Baseline {
			return g.ID, nil
		}
	}
	if len(groups) > 0 {
		return groups[0].ID, nil
	}
	return "", fmt.Errorf("the console has no policy group")
}

// addPolicy writes a policy into the baseline group, as an administrator does
// in the console's policy editor. spec is the policy's spec, kind and all.
func (c *console) addPolicy(name string, spec map[string]any) error {
	group, err := c.baselineGroup()
	if err != nil {
		return err
	}
	return c.call("POST", "/policies", map[string]any{
		"groupId": group, "name": name, "description": "made by the end-to-end run", "spec": spec,
	}, nil)
}
