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
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// The index and its state table are created outside the goose migration chain
// on purpose.  goose disables out-of-order migrations by default and errors
// when it finds one, so shipping this as a migration would mean an archive
// written by a newer upstream could not be opened by this fork at all.  The
// index is a derived artifact — always rebuildable from MESSAGE — so it needs
// no schema versioning.
const (
	ftsTable = "MESSAGE_FTS"
	ftsState = "MESSAGE_FTS_STATE"
)

// EnsureFTS creates the full-text index if it is absent and rebuilds it when
// MESSAGE has changed since it was last built.  It is idempotent.
//
// Call it lazily, on the first word-mode query of a process: commands that
// never search should not pay for an index they never read.
func EnsureFTS(ctx context.Context, conn sqlx.ExtContext) error {
	const createFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS ` + ftsTable + `
  USING fts5(TXT, content='MESSAGE', content_rowid='ROWID',
             tokenize='unicode61 remove_diacritics 2')`
	if _, err := conn.ExecContext(ctx, createFTS); err != nil {
		return fmt.Errorf("ensureFTS: create %s: %w", ftsTable, err)
	}

	const createState = `
CREATE TABLE IF NOT EXISTS ` + ftsState + ` (ROWS INTEGER NOT NULL, SUM_ID INTEGER NOT NULL, SUM_ROWID INTEGER NOT NULL)`
	if _, err := conn.ExecContext(ctx, createState); err != nil {
		return fmt.Errorf("ensureFTS: create %s: %w", ftsState, err)
	}

	stale, err := ftsStale(ctx, conn)
	if err != nil {
		return fmt.Errorf("ensureFTS: stale check: %w", err)
	}
	if !stale {
		return nil
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO `+ftsTable+`(`+ftsTable+`) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("ensureFTS: rebuild %s: %w", ftsTable, err)
	}
	// Drop rather than DELETE so a state table left behind with an older
	// column set is replaced rather than failing the insert below.
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+ftsState); err != nil {
		return fmt.Errorf("ensureFTS: reset %s: %w", ftsState, err)
	}
	if _, err := conn.ExecContext(ctx, createState); err != nil {
		return fmt.Errorf("ensureFTS: recreate %s: %w", ftsState, err)
	}
	const recordState = `
INSERT INTO ` + ftsState + `(ROWS, SUM_ID, SUM_ROWID)
  SELECT COUNT(*), COALESCE(SUM(ID % 1000000007),0), COALESCE(SUM(ROWID % 1000000007),0) FROM MESSAGE`
	if _, err := conn.ExecContext(ctx, recordState); err != nil {
		return fmt.Errorf("ensureFTS: record %s: %w", ftsState, err)
	}
	return nil
}

// ftsStale reports whether MESSAGE has changed since the index was built.
//
// The signal is the triple (ROWS, SUM_ID, SUM_ROWID).  ROWS tracks ordinary
// churn, because this schema has no upsert (see AGENTS.md quirk 1): every edit
// or re-fetch arrives as a new row, and "tools dedupe" only removes rows.
//
// ROWS alone is not enough.  MESSAGE_FTS is an *external-content* index: it
// stores MESSAGE's rowids, not its text.  SQLite reuses the highest rowid once
// the row holding it is deleted, so deleting the top row and inserting another
// leaves the count unchanged while every FTS entry for that rowid now describes
// a different message.  Summing ID catches that, because the replacement
// carries a different timestamp and so a different ID.
//
// SUM_ID alone is not enough either.  Delete an *interior* row and insert one
// carrying the same ID — a re-fetch of a message "tools dedupe" had removed —
// and both ROWS and SUM_ID come back exactly, while SQLite hands the new row a
// fresh rowid rather than the freed one.  Summing rowids catches that.  It also
// catches interior renumbering that a MAX(ROWID) term would miss, for the same
// single scan, and it covers VACUUM: SQLite was observed to preserve rowids for
// ordinary tables (tested on SQLite 3.51.0 and modernc.org/sqlite 1.54.0), but
// the documentation promises the opposite — "a VACUUM may change the ROWIDs of
// entries in any tables that do not have an explicit INTEGER PRIMARY KEY" — and
// MESSAGE's primary key is the composite (ID, CHUNK_ID), so the documented
// behaviour is the one to code against.
//
// What the triple still misses is any change that leaves all three terms
// untouched.  Three are known.  Deleting the *highest* rowid and inserting a
// row with the same ID but different text: SQLite hands back the rowid it just
// freed, so no term moves and the index keeps serving the old text (measured,
// not reasoned).  Two messages in different channels sharing an ID, since
// MESSAGE.ID comes from the message timestamp alone and is not scoped by
// channel — the fixtures in this package rely on that.  And an UPDATE that
// rewrites TXT in place, which moves nothing at all.  Closing them would mean
// checksumming TXT, i.e. a full text scan on every search; none of them arises
// from a slackdump writer, which only ever INSERTs messages and only ever
// deletes whole rows, so the signal is sized for the writers that exist rather
// than for arbitrary SQL.
//
// Every term is reduced modulo 1000000007 before summing.  MESSAGE.ID is a
// microsecond timestamp of roughly 1e16, and a plain SUM(ID) overflows int64 on
// any real archive; reduced, each term is under 1e9, so overflow needs more
// than 9.2e9 rows.
//
// Any failure to read the stored state — table missing, wrong shape, or empty —
// counts as stale.  That is also what lets the column set change safely: a state
// table written by an older version fails the scan, reads as stale, and is
// dropped and recreated by EnsureFTS.  A needless rebuild is cheap; silently
// serving a stale index is not.
func ftsStale(ctx context.Context, conn sqlx.QueryerContext) (bool, error) {
	const liveQuery = `SELECT COUNT(*), COALESCE(SUM(ID % 1000000007),0), COALESCE(SUM(ROWID % 1000000007),0) FROM MESSAGE`
	var liveRows, liveSumID, liveSumRowID int64
	if err := conn.QueryRowxContext(ctx, liveQuery).Scan(&liveRows, &liveSumID, &liveSumRowID); err != nil {
		return false, fmt.Errorf("ftsStale: live state: %w", err)
	}

	const storedQuery = `SELECT ROWS, SUM_ID, SUM_ROWID FROM ` + ftsState
	var storedRows, storedSumID, storedSumRowID int64
	if err := conn.QueryRowxContext(ctx, storedQuery).Scan(&storedRows, &storedSumID, &storedSumRowID); err != nil {
		// Missing table, wrong shape or no row: the index has never been built,
		// or was built by an incompatible version.  Either way, rebuild.
		return true, nil
	}

	return liveRows != storedRows || liveSumID != storedSumID || liveSumRowID != storedSumRowID, nil
}
