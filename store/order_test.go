package store

import (
	"encoding/json"
	"testing"
)

// Everything here is one property: rows written in the same clock tick come
// back in the order they were written.
//
// It reads like a test about ties, and it is really a test about what an
// administrator sees. "Newest first" is how the console shows the change
// history and how a request's thread is read, and the ordering used to break
// ties on `id` — eight random bytes, so a tie was not broken but shuffled.
// Nobody saw it on macOS or Linux, where two consecutive inserts almost never
// share a timestamp; on Windows the clock resolves to the system tick and a
// tie is routine. The fix is to break on rowid, which is insertion order.
//
// The tie is forced here rather than waited for, so this fails on every
// platform if the ordering goes back to being random — the flaky Windows
// tests that found it could only ever fail sometimes, and only there.

// sameTick collapses a column to one value across a whole table, which is what
// a coarse clock does on its own.
func sameTick(t *testing.T, s *Store, table, column string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE ` + table + ` SET ` + column +
		` = (SELECT MIN(` + column + `) FROM ` + table + `)`); err != nil {
		t.Fatalf("collapsing %s.%s: %v", table, column, err)
	}
}

func TestRowsWrittenInOneTickComeBackInTheOrderTheyWereWritten(t *testing.T) {
	s := openTestStore(t)
	actor := mkUser(t, s, "admin@example.com")

	t.Run("change history", func(t *testing.T) {
		for _, name := range []string{"first", "second", "third", "fourth", "fifth"} {
			if err := s.RecordChange(actor.ID, "policy", "created", "p-"+name, name,
				"Added "+name+".", nil); err != nil {
				t.Fatal(err)
			}
		}
		sameTick(t, s, "changes", "at")

		got, err := s.Changes("", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"fifth", "fourth", "third", "second", "first"}
		if len(got) != len(want) {
			t.Fatalf("%d changes, want %d", len(got), len(want))
		}
		for i, name := range want {
			if got[i].EntityName != name {
				t.Fatalf("history in %v order, want newest first %v",
					entityNames(got), want)
			}
		}
	})

	t.Run("request thread", func(t *testing.T) {
		req, err := s.CreateAccessRequest(actor.ID, "", "npm registry", "to install",
			"allow npm", nil, json.RawMessage(`{"kind":"network"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"one", "two", "three", "four"} {
			if _, err := s.AddRequestComment(req.ID, actor.ID, body); err != nil {
				t.Fatal(err)
			}
		}
		sameTick(t, s, "request_comments", "at")

		comments, err := s.RequestComments(req.ID)
		if err != nil {
			t.Fatal(err)
		}
		// A thread is read oldest first: it is a conversation, and an answer
		// above the question it answers is not one.
		want := []string{"one", "two", "three", "four"}
		if len(comments) != len(want) {
			t.Fatalf("%d comments, want %d", len(comments), len(want))
		}
		for i, body := range want {
			if comments[i].Body != body {
				t.Fatalf("comment %d is %q, want %q — the thread is out of order",
					i, comments[i].Body, body)
			}
		}
	})

	t.Run("the workspace's live session", func(t *testing.T) {
		// This one had no tiebreaker at all, so which session a machine
		// reattached to came down to which row SQLite reached first. It is
		// the worst of the set: the wrong answer is somebody else's
		// conversation, carried on as if it were yours.
		var last string
		for i := 0; i < 5; i++ {
			sess, err := s.CreateSession(actor.ID, "ws-1", "/home/bob/ws", "laptop", "m1")
			if err != nil {
				t.Fatal(err)
			}
			last = sess.ID
		}
		sameTick(t, s, "sessions", "started_at")

		active, err := s.FindActiveSession(actor.ID, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		if active.ID != last {
			t.Fatalf("reattached to session %s, want the newest %s", active.ID, last)
		}
		list, err := s.SessionsForUser(actor.ID, "ws-1", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 5 || list[0].ID != last {
			t.Fatalf("/resume lists %d sessions, newest %s, want the newest to be %s",
				len(list), list[0].ID, last)
		}
	})
}

func entityNames(changes []*Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.EntityName)
	}
	return out
}
