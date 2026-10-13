package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/popstarapps/john/policy"
)

// policyStore holds the signed policy snapshot the proxy enforces, pulls a
// fresh one on a timer, and blocks nothing on the backend during a request —
// the proxy reads whatever is current and decides alone.
//
// Fail-closed is expressed as expiry: past the snapshot's ExpiresAt, Current
// returns nil and the proxy blocks every decision-needing request, which
// holds even when the backend is up and the client's clock drifted.
type policyStore struct {
	url    string
	token  string
	pubKey ed25519.PublicKey

	// onSnapshot, when set, is handed every snapshot that verifies — the
	// client's way of pushing a policy change into the sandbox without
	// waiting for the next session.
	onSnapshot func(*policy.Snapshot)

	mu   sync.Mutex
	snap *policy.Snapshot

	// refreshing is held for the length of a pull, so the proxy and the
	// timer never fetch the same snapshot twice at once.
	refreshing sync.Mutex
}

func newPolicyStore(backendURL, token string) *policyStore {
	return &policyStore{url: backendURL, token: token}
}

// Current returns the snapshot if one is held and unexpired, else nil.
func (ps *policyStore) Current() *policy.Snapshot {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.snap == nil || ps.snap.Expired(time.Now()) {
		return nil
	}
	return ps.snap
}

// Usable is what the proxy asks before deciding a request: the snapshot, and
// when the one held has expired, one quick attempt at a fresh one first.
//
// Expiry is wall-clock time, and a laptop asleep for an hour wakes with its
// snapshot long expired while the timer that would have renewed it has not
// run — Go's timers stand still while a Mac sleeps. Without this, the first
// requests after waking were refused as "no policy snapshot" with the backend
// up the whole time, and a refusal with no policy behind it offers no access
// request either. It still fails closed: when the backend cannot answer in a
// few seconds, there is no snapshot, and the request is blocked.
func (ps *policyStore) Usable() *policy.Snapshot {
	if snap := ps.Current(); snap != nil {
		return snap
	}
	ps.refreshing.Lock()
	defer ps.refreshing.Unlock()
	if snap := ps.Current(); snap != nil {
		return snap // another request refreshed it while this one waited
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ps.pull(ctx)
	return ps.Current()
}

// due reports whether the snapshot should be renewed now: there is none, or
// it is past 60% of its life by the clock on the wall.
func (ps *policyStore) due(now time.Time) bool {
	ps.mu.Lock()
	snap := ps.snap
	ps.mu.Unlock()
	if snap == nil {
		return true
	}
	life := snap.ExpiresAt.Sub(snap.IssuedAt)
	return !now.Before(snap.IssuedAt.Add(life * 6 / 10))
}

// pinKey fetches and pins the backend's snapshot-signing public key. Pinned
// once, so a later snapshot signed with a different key is not honoured.
func (ps *policyStore) pinKey(ctx context.Context) error {
	var out struct {
		PublicKey string `json:"public_key"`
	}
	if err := ps.get(ctx, "/api/v1/policy/signing-key", &out); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	ps.pubKey = ed25519.PublicKey(raw)
	ps.mu.Unlock()
	return nil
}

// refresh pulls a snapshot, verifies its signature against the pinned key, and
// installs it. A snapshot that does not verify is rejected — a snapshot
// loosened on disk is not one the proxy will honour.
func (ps *policyStore) refresh(ctx context.Context) error {
	ps.refreshing.Lock()
	defer ps.refreshing.Unlock()
	return ps.pull(ctx)
}

// pull is refresh for a caller already holding refreshing.
func (ps *policyStore) pull(ctx context.Context) error {
	ps.mu.Lock()
	pub := ps.pubKey
	ps.mu.Unlock()
	if pub == nil {
		if err := ps.pinKey(ctx); err != nil {
			return err
		}
		ps.mu.Lock()
		pub = ps.pubKey
		ps.mu.Unlock()
	}

	var signed policy.SignedSnapshot
	if err := ps.get(ctx, "/api/v1/policy/snapshot", &signed); err != nil {
		return err
	}
	snap, err := policy.Verify(&signed, pub)
	if err != nil {
		return fmt.Errorf("policy snapshot rejected: %w", err)
	}
	ps.mu.Lock()
	ps.snap = snap
	hook := ps.onSnapshot
	ps.mu.Unlock()
	if hook != nil {
		hook(snap)
	}
	return nil
}

// run keeps the snapshot fresh: renewed at 60% of its life, and retried every
// tick while there is nothing usable held.
//
// It looks at the wall clock every few seconds rather than sleeping until the
// renewal is due. A long timer does not advance while the machine sleeps, so
// it woke past an expiry it had slept through; a short tick notices the clock
// has moved within seconds of waking. Each tick that is not due costs a lock
// and a comparison.
func (ps *policyStore) run(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case now := <-tick.C:
			if !ps.due(now) {
				continue
			}
			ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
			ps.refresh(ctx2)
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

func (ps *policyStore) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", ps.url+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ps.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
