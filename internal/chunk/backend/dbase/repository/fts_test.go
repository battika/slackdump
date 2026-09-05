// Copyright (c) 2021-2026 Rustam Gilyazov and Contributors.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package repository

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/rusq/slack"
)

func TestEnsureFTS(t *testing.T) {
	newConn := func(t *testing.T) *sqlx.DB {
		t.Helper()
		conn := testConn(t)
		messagePrepFn(t, conn)
		return conn
	}
	indexed := func(t *testing.T, conn *sqlx.DB) int {
		t.Helper()
		var n int
		if err := conn.QueryRowxContext(t.Context(),
			`SELECT COUNT(*) FROM MESSAGE_FTS WHERE MESSAGE_FTS MATCH '"thread"'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("creates and populates", func(t *testing.T) {
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatalf("EnsureFTS() error = %v", err)
		}
		if got := indexed(t, conn); got == 0 {
			t.Error("index should contain the thread messages")
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		conn := newConn(t)
		for i := range 3 {
			if err := EnsureFTS(t.Context(), conn); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if got := indexed(t, conn); got == 0 {
			t.Error("index should still be populated after repeated calls")
		}
	})

	t.Run("does not touch the goose version", func(t *testing.T) {
		// The guarantee that an archive indexed by this fork still opens in
		// upstream slackdump.  If this fails the change is unshippable.
		conn := newConn(t)
		q := `SELECT COALESCE(MAX(version_id),0) FROM goose_db_version`
		var before, after int64
		if err := conn.QueryRowxContext(t.Context(), q).Scan(&before); err != nil {
			t.Fatalf("before: %v", err)
		}
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowxContext(t.Context(), q).Scan(&after); err != nil {
			t.Fatalf("after: %v", err)
		}
		if before != after {
			t.Errorf("goose version moved %d -> %d; the index must never be a migration", before, after)
		}
	})

	t.Run("rebuilds after a foreign write", func(t *testing.T) {
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		before := indexed(t, conn)

		// A writer that knows nothing about the index — i.e. upstream slackdump.
		msg := slack.Message{Msg: slack.Msg{Timestamp: "126.999", Text: "another thread message"}}
		if err := NewMessageRepository().Insert(t.Context(), conn, must(NewDBMessage(1, 9, "C123", &msg))); err != nil {
			t.Fatalf("foreign insert: %v", err)
		}

		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		if got := indexed(t, conn); got <= before {
			t.Errorf("index did not pick up the foreign write: %d then %d", before, got)
		}
	})

	t.Run("skips the rebuild when nothing changed", func(t *testing.T) {
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		stale, err := ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() error = %v", err)
		}
		if stale {
			t.Error("index should not be stale immediately after building")
		}
	})

	t.Run("large ids do not overflow the checksum", func(t *testing.T) {
		// MESSAGE.ID is a microsecond timestamp of roughly 1e16, so summing it
		// directly overflows int64 after a few thousand messages — that is, on
		// essentially every real archive.  Two rows near the ceiling reproduce
		// it deterministically without inserting thousands.
		conn := newConn(t)
		const huge = `
INSERT INTO MESSAGE (ID, CHUNK_ID, CHANNEL_ID, TS, IS_PARENT, IDX, NUM_FILES, TXT, DATA)
VALUES (?, 1, 'C123', ?, FALSE, 99, 0, 'huge id message', X'7B7D')`
		for i, id := range []int64{9_000_000_000_000_000_000, 8_000_000_000_000_000_000} {
			if _, err := conn.ExecContext(t.Context(), huge, id, "999.00"+string(rune('0'+i))); err != nil {
				t.Fatalf("insert huge id %d: %v", id, err)
			}
		}

		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatalf("EnsureFTS() with large ids: %v", err)
		}
		if _, err := ftsStale(t.Context(), conn); err != nil {
			t.Fatalf("ftsStale() with large ids: %v", err)
		}
	})

	t.Run("rowid reuse after delete plus insert is caught", func(t *testing.T) {
		// SQLite reuses the highest rowid once the row holding it is deleted,
		// so deleting the top row and inserting another leaves both the row
		// count and MAX(ROWID) exactly as they were.  Summing ID is what
		// notices; without it the index keeps serving the deleted message and
		// never sees the new one.
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}

		var topRowID, rowsBefore int64
		if err := conn.QueryRowxContext(t.Context(),
			`SELECT COALESCE(MAX(ROWID),0), COUNT(*) FROM MESSAGE`).Scan(&topRowID, &rowsBefore); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if _, err := conn.ExecContext(t.Context(), `DELETE FROM MESSAGE WHERE ROWID = ?`, topRowID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		msg := slack.Message{Msg: slack.Msg{Timestamp: "199.001", Text: "replacement message"}}
		if err := NewMessageRepository().Insert(t.Context(), conn, must(NewDBMessage(1, 11, "C123", &msg))); err != nil {
			t.Fatalf("insert: %v", err)
		}

		var rowsAfter, maxAfter int64
		if err := conn.QueryRowxContext(t.Context(),
			`SELECT COUNT(*), COALESCE(MAX(ROWID),0) FROM MESSAGE`).Scan(&rowsAfter, &maxAfter); err != nil {
			t.Fatalf("probe after: %v", err)
		}
		if rowsAfter != rowsBefore || maxAfter != topRowID {
			t.Skipf("this SQLite did not reuse the rowid (rows %d->%d, max %d->%d); "+
				"the scenario under test did not occur", rowsBefore, rowsAfter, topRowID, maxAfter)
		}

		stale, err := ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() error = %v", err)
		}
		if !stale {
			t.Error("a swapped row left the count and max rowid unchanged and went undetected")
		}
	})

	t.Run("a state table from an older version is replaced, not trusted", func(t *testing.T) {
		// The state's column set is free to grow only because a table of the
		// wrong shape fails the scan, reads as stale, and is dropped and
		// recreated.  The legacy row below describes MESSAGE exactly, so its
		// shape is the only thing that can make it read as stale.
		conn := newConn(t)
		if _, err := conn.ExecContext(t.Context(),
			`CREATE TABLE `+ftsState+` (ROWS INTEGER NOT NULL, SUM_ID INTEGER NOT NULL)`); err != nil {
			t.Fatalf("create legacy state: %v", err)
		}
		if _, err := conn.ExecContext(t.Context(),
			`INSERT INTO `+ftsState+`(ROWS, SUM_ID) SELECT COUNT(*), COALESCE(SUM(ID % 1000000007),0) FROM MESSAGE`); err != nil {
			t.Fatalf("record legacy state: %v", err)
		}

		stale, err := ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() error = %v", err)
		}
		if !stale {
			t.Fatal("a state table of an older shape must read as stale")
		}

		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatalf("EnsureFTS() over a legacy state: %v", err)
		}
		if got := indexed(t, conn); got == 0 {
			t.Error("index should be populated once the legacy state was replaced")
		}
		stale, err = ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() after replacement: %v", err)
		}
		if stale {
			t.Error("the replacement state should read as fresh")
		}
	})

	t.Run("delete and reinsert of the same message ID is caught", func(t *testing.T) {
		// The nastier sibling of the rowid-reuse case above, and the one that
		// (ROWS, SUM_ID) alone cannot see: delete an *interior* row and insert
		// a row carrying the same ID — a re-fetch of a message that "tools
		// dedupe" had removed.  COUNT comes back to where it was and
		// SUM(ID % p) comes back *exactly*, because the ID never changed.
		// SQLite hands the new row a fresh rowid, though, so the
		// external-content index now points at a rowid that no longer exists
		// and misses the one that does: word search finds neither the old text
		// nor the new one.
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}

		const liveState = `SELECT COUNT(*), COALESCE(SUM(ID % 1000000007),0), COALESCE(MAX(ROWID),0) FROM MESSAGE`
		var rowsBefore, sumBefore, maxBefore int64
		if err := conn.QueryRowxContext(t.Context(), liveState).Scan(&rowsBefore, &sumBefore, &maxBefore); err != nil {
			t.Fatalf("probe: %v", err)
		}

		// Message A is the oldest row of the fixture, comfortably below
		// MAX(ROWID): the point is that the rowid it frees is *not* the one
		// the insert below will be given.
		var rowID int64
		if err := conn.QueryRowxContext(t.Context(),
			`SELECT ROWID FROM MESSAGE WHERE CHANNEL_ID = 'C123' AND CHUNK_ID = 1 AND TXT = 'A'`).Scan(&rowID); err != nil {
			t.Fatalf("locate A: %v", err)
		}
		if rowID >= maxBefore {
			t.Fatalf("A is at ROWID %d, the top of the table (%d); this test needs an interior row", rowID, maxBefore)
		}
		if _, err := conn.ExecContext(t.Context(), `DELETE FROM MESSAGE WHERE ROWID = ?`, rowID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		// Same timestamp, therefore the same MESSAGE.ID, and the same chunk.
		msg := slack.Message{Msg: slack.Msg{Timestamp: "123.456", Text: "refetched needle"}}
		if err := NewMessageRepository().Insert(t.Context(), conn, must(NewDBMessage(1, 0, "C123", &msg))); err != nil {
			t.Fatalf("reinsert: %v", err)
		}

		var rowsAfter, sumAfter, maxAfter int64
		if err := conn.QueryRowxContext(t.Context(), liveState).Scan(&rowsAfter, &sumAfter, &maxAfter); err != nil {
			t.Fatalf("probe after: %v", err)
		}
		if rowsAfter != rowsBefore || sumAfter != sumBefore {
			t.Fatalf("the scenario under test did not occur: (rows, sum) moved (%d, %d) -> (%d, %d); "+
				"it is only interesting while both are restored exactly", rowsBefore, sumBefore, rowsAfter, sumAfter)
		}
		if maxAfter == maxBefore {
			t.Fatalf("this SQLite reused ROWID %d instead of allocating a new one; the scenario did not occur", rowID)
		}

		stale, err := ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() error = %v", err)
		}
		if !stale {
			t.Error("a row swapped for one with the same ID left every counted term unchanged and went undetected")
		}

		// The consequence, which is what actually matters: after EnsureFTS the
		// reinserted message must be findable by word search.
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		it, err := NewMessageRepository().SearchMessagesFTS(t.Context(), conn, "needle", "", false, 10)
		if err != nil {
			t.Fatalf("SearchMessagesFTS() error = %v", err)
		}
		var got []string
		for dbm, err := range it {
			if err != nil {
				t.Fatalf("iteration error = %v", err)
			}
			got = append(got, dbm.Text)
		}
		if len(got) != 1 || got[0] != "refetched needle" {
			t.Errorf("word search after the swap = %v, want [refetched needle]", got)
		}
	})

	t.Run("a foreign write makes it stale", func(t *testing.T) {
		conn := newConn(t)
		if err := EnsureFTS(t.Context(), conn); err != nil {
			t.Fatal(err)
		}
		msg := slack.Message{Msg: slack.Msg{Timestamp: "127.111", Text: "yet another"}}
		if err := NewMessageRepository().Insert(t.Context(), conn, must(NewDBMessage(1, 10, "C123", &msg))); err != nil {
			t.Fatalf("foreign insert: %v", err)
		}
		stale, err := ftsStale(t.Context(), conn)
		if err != nil {
			t.Fatalf("ftsStale() error = %v", err)
		}
		if !stale {
			t.Error("a row added behind the index's back should read as stale")
		}
	})
}
