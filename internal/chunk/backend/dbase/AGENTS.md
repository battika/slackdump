# Database Package — Agent Reference

This document captures non-obvious behaviours, data quirks, and schema
decisions in the `dbase` package that are relevant to any agent working on or
querying the Slackdump SQLite database.

The schema is managed by [goose](https://github.com/pressly/goose) migrations
located in `repository/migrations/`.

---

## Schema Overview

### Tables

| Table          | Primary Key          | Description |
|----------------|----------------------|-------------|
| `SESSION`      | `ID` (autoincrement) | One row per `slackdump` invocation |
| `CHUNK`        | `ID` (autoincrement) | One row per Slack API response page |
| `TYPES`        | `ID`                 | Chunk type lookup (TYPE_ID → name) |
| `MESSAGE`      | `(ID, CHUNK_ID)`     | Channel and thread messages |
| `CHANNEL`      | `(ID, CHUNK_ID)`     | Slack channels / conversations |
| `FILE`         | `(ID, CHUNK_ID)`     | File attachments |
| `WORKSPACE`    | `ID` (autoincrement) | Workspace metadata |
| `S_USER`       | `(ID, CHUNK_ID)`     | Workspace members |
| `CHANNEL_USER` | `(CHANNEL_ID, USER_ID, CHUNK_ID)` | Channel membership (no DATA blob) |
| `SEARCH_MESSAGE` | `ID` (autoincrement) | Results from `slackdump search` |
| `SEARCH_FILE`  | `ID` (autoincrement) | File results from `slackdump search` |

### Views (all prefixed `V_`)

All `V_*` views are **internal to slackdump** and used to track unprocessed
threads during archiving. Do not rely on them for general analysis. They are:

| View | Purpose |
|------|---------|
| `V_CHANNEL_THREADS` | Thread count per channel/session (finished channels only) |
| `V_CHANNEL_THREAD_COUNT` | Count of actually downloaded thread parents |
| `V_UNFINISHED_CHANNELS` | Channels where thread count doesn't match downloaded |
| `V_ORPHAN_THREADS` | Threads with a parent but no downloaded children |
| `V_EMPTY_THREADS` | Threads where `LATEST_REPLY = '0000000000.000000'` |
| `V_THREAD_ONLY_THREADS` | For thread-only mode: counts parts per thread |
| `V_LATEST_MESSAGE` | Latest channel message per channel (TYPE_ID=0 only) |
| `V_LATEST_THREAD` | Latest thread message per channel+thread_ts (TYPE_ID=1 only) |

---

## Chunk Types (TYPES table)

| ID | NAME              | Stores data in   |
|----|-------------------|------------------|
|  0 | `MESSAGES`        | `MESSAGE`        |
|  1 | `THREAD_MESSAGES` | `MESSAGE`        |
|  2 | `FILES`           | `FILE`           |
|  3 | `USERS`           | `S_USER`         |
|  4 | `CHANNELS`        | `CHANNEL`        |
|  5 | `CHANNEL_INFO`    | `CHANNEL`        |
|  6 | `WORKSPACE_INFO`  | `WORKSPACE`      |
|  7 | `CHANNEL_USERS`   | `CHANNEL_USER`   |
|  8 | `STARRED_ITEMS`   | *(no table)*     |
|  9 | `BOOKMARKS`       | *(no table)*     |
| 10 | `SEARCH_MESSAGES` | `SEARCH_MESSAGE` |
| 11 | `SEARCH_FILES`    | `SEARCH_FILE`    |

**Note:** `STARRED_ITEMS` (8) and `BOOKMARKS` (9) are defined in the enum and
TYPES table but have no corresponding storage table and no assembler — they
would return an error if encountered in `insertPayload`. See `split.go`.

**Note:** `CHANNELS` (4) and `CHANNEL_INFO` (5) both write to the `CHANNEL`
table. `CHANNEL_INFO` (5) contains individually fetched full channel details;
`CHANNELS` (4) is the bulk list. `source.Channels()` prefers type 5, falling
back to type 4 if empty. See `source.go`.

---

## Key Data Quirks

### 1. No upsert — deduplication is at query time

All inserts use plain `INSERT INTO` with no `ON CONFLICT` clause
(`generic.go: stmtInsert`). The same message can appear in multiple `CHUNK`
rows (e.g. from pagination overlap or a `slackdump resume` run).

**Always select the row from the highest `CHUNK_ID` for a given message.**
The internal pattern used is a CTE:

```sql
WITH LATEST AS (
    SELECT CHANNEL_ID, MAX(CHUNK_ID) AS CHUNK_ID
    FROM MESSAGE
    GROUP BY CHANNEL_ID
)
SELECT M.*
FROM MESSAGE M
JOIN LATEST L ON M.CHANNEL_ID = L.CHANNEL_ID AND M.CHUNK_ID = L.CHUNK_ID
```

See `generic.go: stmtLatestRows`.

### 2. MESSAGE.ID is not a Slack timestamp string

`MESSAGE.ID` is the Slack timestamp (`TS`) converted to `int64` microseconds
by stripping the dot:

```
"1648085300.726649"  →  1648085300726649
```

This is done by `fasttime.TS2int()` (`fasttime/fasttime_x64.go`).

The primary key is `(ID, CHUNK_ID)`, so the same logical message appearing in
two chunks has the same `ID` but different `CHUNK_ID` values.

**Use `MESSAGE.TS` for the human-readable Slack timestamp.**

### 3. PARENT_ID is set on both parents and replies

`MESSAGE.PARENT_ID` is set to the `thread_ts` (as int64) for **all** messages
that have a thread timestamp — both the thread-parent and its replies.

- Thread-parent: `PARENT_ID = ID` (points to itself), `IS_PARENT = TRUE`
- Thread reply: `PARENT_ID = <parent's ID>`, `IS_PARENT = FALSE`

Filtering `PARENT_ID IS NOT NULL` alone matches both parents **and** replies.

| Goal | Filter |
|------|--------|
| Thread parents (with replies) | `IS_PARENT = TRUE` |
| Thread replies only | `IS_PARENT = FALSE AND PARENT_ID IS NOT NULL` |
| Non-threaded messages | `PARENT_ID IS NULL` |

### 4. IS_PARENT=TRUE implies the thread has replies

`IS_PARENT` is set by `structures.IsThreadStart()`:

```go
msg.ThreadTimestamp != "" &&
msg.Timestamp == msg.ThreadTimestamp &&
msg.LatestReply != "0000000000.000000"
```

A thread-lead message with `LatestReply = "0000000000.000000"` (deleted/empty
thread) gets `IS_PARENT = FALSE`. Therefore `IS_PARENT = TRUE` already
excludes empty threads — no need to additionally filter on `LATEST_REPLY`.

See `structures/conversation.go`.

### 5. Sentinel value "0000000000.000000"

Stored in `MESSAGE.LATEST_REPLY` and `LATEST_REPLY` column. Means the thread
was started but has no replies (deleted thread). Defined as
`structures.LatestReplyNoReplies`.

Do not attempt to fetch thread messages for rows with this value — there are
none.

### 6. thread_broadcast — messages duplicated across TYPE_ID=0 and TYPE_ID=1

Messages with `subtype = "thread_broadcast"` ("also sent to channel") are
stored in **both** the channel history chunk (TYPE_ID=0) and the thread chunk
(TYPE_ID=1).

When querying channel messages, filter them out to avoid double-counting:

```sql
WHERE JSON_EXTRACT(DATA, '$.subtype') IS NOT 'thread_broadcast'
```

See `repository/dbmessage.go: threadCond()`.

### 7. DATA column — full JSON blob

Every entity table stores the complete Slack API JSON payload in a `DATA`
column (`BLOB`, uncompressed). Explicit columns (`TS`, `PARENT_ID`,
`IS_PARENT`, `MODE`, `NAME`, etc.) are extracted for indexing purposes only.

All other fields — reactions, edited timestamps, message subtypes, user
profiles, blocks, attachments — are only accessible via `JSON_EXTRACT`:

```sql
SELECT TS,
       JSON_EXTRACT(DATA, '$.subtype')            AS subtype,
       JSON_EXTRACT(DATA, '$.edited.ts')          AS edited_ts,
       JSON_ARRAY_LENGTH(DATA, '$.reactions')     AS reaction_count
FROM MESSAGE
WHERE CHUNK_ID = 44;
```

Note: an abandoned `unused.go` file (`//go:build ignore`) contains gzip/flate
compression code that was never activated. Data is always stored uncompressed.

### 8. CHUNK.FINAL — archive completeness

`CHUNK.FINAL = TRUE` marks the last API pagination page for a given
channel/thread. If no `FINAL = TRUE` chunk exists for a channel, or the last
chunk has `FINAL = FALSE`, the archive is incomplete for that channel.

The index `CHUNK_I1 ON CHUNK (CHANNEL_ID, SESSION_ID, TYPE_ID, FINAL)` was
added in migration `20250809050908` to make completeness checks efficient.

### 9. SESSION.FINISHED — interrupted archives

`SESSION.FINISHED = TRUE` is set only when `DBP.Close()` completes
successfully. A session interrupted mid-run (crash, network failure) leaves
`FINISHED = FALSE`. Data from such a session may be incomplete.

See `repository/session.go: Finalise()` and `dbase.go: Close()`.

### 10. Resume creates a new SESSION

`slackdump resume` inserts a new `SESSION` row with `PAR_SESSION_ID` pointing
to the previous session. It does **not** update existing rows. Data from
multiple sessions coexists in the same tables, distinguished by the
`CHUNK_ID → SESSION_ID` chain.

The resume logic uses `OptInclusive(false)` (exclusive lower bound) so the
last known message is not re-fetched, plus a configurable lookback window
(default 7 days) to catch new replies on older messages.

See `cmd/slackdump/internal/resume/resume.go`.

### 11. S_USER — not USER

The users table is named `S_USER`, not `USER`. `USER` is a reserved word in
SQLite. Querying `.schema USER` will return nothing.

The `USERNAME` column is derived via `structures.Username()` which returns
`COALESCE(u.Name, u.ID)` — never empty.

### 12. FILE.MESSAGE_ID is NULL for canvas files

`FILE.MESSAGE_ID` is nullable. It is `NULL` for channel canvas files (Slack
Spaces), which are not attached to a specific message. `FILE.THREAD_ID` is
also nullable (only set when the file belongs to a thread message).

### 13. File modes

`FILE.MODE` comes directly from Slack's API. Known values:

| Mode | Downloadable | Notes |
|------|-------------|-------|
| `hosted` | Yes | Normal Slack-hosted file |
| `snippet` | Yes | Code snippet |
| `space` | Yes | Slack canvas / huddle space |
| `external` | No | Externally hosted; `is_external = true` in DATA |
| `tombstone` | No | File was deleted |
| `hidden_by_limit` | No | Hidden on free workspaces after 90 days |

See `convert/transform/fileproc/fileproc.go: invalidModes`.

### 14. CHUNK.NUM_REC is informational only

`CHUNK.NUM_REC` stores the record count at insert time from the chunk struct.
The actual number of `MESSAGE` rows for a given `CHUNK_ID` may differ. Do not
rely on `NUM_REC` for exact message counts — always `COUNT(*)` the target
table.

### 15. SEARCH_MESSAGE.ID is autoincrement

Unlike `MESSAGE` whose `ID` is derived from the Slack timestamp,
`SEARCH_MESSAGE.ID` is a SQLite autoincrement integer. Search results have no
deduplication key equivalent to `MESSAGE.TS`; the latest-chunk pattern
(MAX CHUNK_ID per CHANNEL_ID) is used instead.

### 16. Database pragmas

The database is initialised with:

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA foreign_keys = ON;
```

External tools querying the database should be aware of WAL mode (the
`slackdump.sqlite-wal` and `slackdump.sqlite-shm` sidecar files must be
present for a consistent read while slackdump is running).

See `dbase.go: dbInitCommands`.

### 17. LIMIT/OFFSET are emitted as literals, not binds

`queryParams.Limit` and `.Offset` are formatted into the statement with `fmt.Fprintf` rather
than bound. `allOfTypeWhere` appends `qp.Binds` a **second** time on top of the binds
`stmtLatestRows` already returned — the outer statement ends in `WHERE 1=1` and has no
placeholders of its own, and the driver silently tolerates the surplus arguments. (Verified:
`PageForID` sends 4 binds against 3 placeholders and works.) Adding new placeholders to that path
would misalign the positional arguments. Both fields are `int`, so there is no injection surface.

`OFFSET` is emitted only alongside a `LIMIT`; an offset with no limit is ignored, because SQLite
rejects a bare `OFFSET` and no caller needs one.

Note that `countTypeWhere` does **not** have this quirk — it calls `stmtLatestWhere` once and uses
its binds directly, which is why `CountBeforeID` can safely take a second bind.

### 18. Paging queries rely on ID being a total order

`PageForID` paginates with `ORDER BY T.ID`. `DBMessage.userkey()` is `ID` alone and the dedup CTE
groups by it with `MAX(CHUNK_ID)`, so every row in the result has a distinct `ID` — the ordering
is total and pages cannot repeat or drop rows across `slackdump resume` overlap.

Beware when writing tests: `fasttime.TS2int` concatenates the digits either side of the dot with
**no decimal alignment**, so `"1.000000"` becomes `1000000`, which is *larger* than a fixture using
short timestamps like `"123.456"` (→ `123456`). Ordering is only preserved between timestamps of
equal digit width. Real Slack timestamps are always 10-digit seconds plus 6-digit microseconds, so
production data is safe; synthetic fixtures must use the same shape or comparisons invert.

### 19. Search deliberately omits `channelTimelineCondition`

`SearchMessages` is the one message query that does **not** apply
`channelTimelineCondition`. That condition exists to keep thread replies out of
a channel *timeline*; search must reach them, since a large share of real
content lives in threads. Adding it back silently drops every thread reply from
results — the `reaches thread replies` test row is the guard, and it fails if
you do.

Truncation is detected by asking for `limit+1` rows and trimming: one row beyond
the cap means "there were more", with no second `COUNT` query. `Source.
SearchMessages` also rejects a non-positive limit up front, because the
repository reads `Limit <= 0` as "no LIMIT clause" — a negative limit would
otherwise run unbounded and then slice with a negative bound.

### 20. `slackdump_lower` exists because SQLite folds ASCII only

SQLite's `LIKE` and `LOWER()` are ASCII-only, so accented text never matches
across case: an upper-case accented term returns **zero** rows against a corpus
that plainly contains it in lower case. Verify with
`SELECT 'áéíóú' LIKE '%ÁÉÍÓÚ%'` — it is 0.

`repository/searchfn.go` therefore registers a deterministic scalar function
`slackdump_lower`, backed by Go's Unicode-aware `strings.ToLower`, and search
compares `slackdump_lower(T.TXT)` against a query lowered the same way. It
roughly doubles the cost of the scan, which is immaterial next to the rendering
it feeds. No index is lost — `LIKE '%…%'` cannot use one anyway.

Two caveats:

- Registration is **process-global** and happens in `init()`. The name is
  namespaced to make a collision with another `modernc.org/sqlite` consumer
  implausible, and a collision panics rather than silently degrading to
  unfolded search.
- `strings.ToLower` is simple case mapping, not full case folding, so German
  `ß`/`SS` still will not match. Fixing that needs `x/text/cases`; FTS5's
  `unicode61` tokenizer would also solve it, along with diacritic stripping.

### 21. User input must be escaped before it reaches `LIKE`

`escapeLike` backslash-escapes `\`, `%` and `_`, and the statement must declare
`ESCAPE '\'`. Both halves are load-bearing and were verified by mutation
testing:

- drop `escapeLike` and a query of `%` matches the entire archive;
- drop the `ESCAPE` clause while keeping `escapeLike` and the inserted
  backslash becomes a literal to match, so searching for a literal `%` silently
  stops working. The `a literal percent is still findable` test row is the only
  thing that catches this — it fails alone when `ESCAPE` is removed.

Note that a literal `%` is common in real data — percentages appear routinely
in ordinary conversation — so this is not a theoretical concern.

### 22. The full-text index is deliberately not a goose migration

`MESSAGE_FTS` and `MESSAGE_FTS_STATE` are created by `repository.EnsureFTS`,
outside the migration chain. This is the most important note in the FTS work
and it is not a style preference.

goose disables out-of-order migrations by default and **errors** when it finds
one. Ship the index as a migration here and any archive written by a newer
upstream slackdump — which numbers its migrations without knowing about this
fork's — becomes unopenable by this build. Not "unsearchable": unopenable, at
`Open`.

The index is a derived artifact. It can always be rebuilt from `MESSAGE`, so it
needs no schema versioning, and creating it must never touch
`goose_db_version`. `TestEnsureFTS/does not touch the goose version` is the
guard. If that test ever fails, the change is unshippable regardless of
anything else, because it breaks archives written by other people's slackdump.

The same reasoning rules out triggers: an `AFTER INSERT` trigger on `MESSAGE`
would be schema an upstream writer never agreed to, and would make every
archiving run pay for an index most users never query.

### 23. The staleness signal, and what it does not catch

`MESSAGE_FTS` is an FTS5 **external-content** table: it stores `MESSAGE`
rowids, not text. If a rowid comes to mean a different message, the index
silently serves wrong results — so `EnsureFTS` records
`(COUNT(*), SUM(ID % 1000000007), SUM(ROWID % 1000000007))` at build time and
rebuilds whenever the live triple differs.

Why each term:

- `COUNT(*)` catches ordinary churn. Sound here because the schema has no
  upsert (quirk 1): every edit or re-fetch arrives as a new row, and dedupe
  only removes rows.
- `SUM(ID % p)` catches a delete plus an insert of a *different* message, which
  leaves the count unchanged.
- `SUM(ROWID % p)` catches a delete plus a re-insert of the **same** ID, which
  leaves both other terms identical while SQLite hands the freed rowid to a
  different row. This term was removed once, on the mistaken reasoning that it
  only guarded against `VACUUM` renumbering. It was restored after the failure
  above was reproduced: the message became permanently invisible to word
  search.

The moduli are load-bearing. `MESSAGE.ID` values are ~1e16 and an unbounded
`SUM(ID)` overflows int64 on any archive past a few thousand messages — that
shipped once and broke `EnsureFTS` outright.

**Known blind spot, accepted.** Deleting the row holding `MAX(ROWID)` and
inserting a replacement with the same ID but different text leaves all three
terms unchanged, because SQLite reuses the freed rowid. So do an in-place
`UPDATE` of `TXT`, and a cross-channel ID collision. Closing these means
checksumming `TXT`, i.e. a full text scan on every search, which is not worth
it for writes slackdump itself never performs. Any failure to read the stored
state — table missing, wrong shape, no row — counts as stale, so an older
state table is replaced rather than trusted.

### 24. Only a *successful* index build is remembered

`Source.ensureFTS` guards `ftsBuilt` with a mutex and sets it only on success.
Caching the failure looks like the obvious optimisation and is a bug: the
viewer's search box fires on `keyup` and htmx aborts the in-flight request on
the next keystroke, so the most likely first-ever outcome is
`context.Canceled`. Latching that disabled word search for the life of the
`Source` and reported it to the user as a read-only archive, which it was not.
`SQLITE_BUSY` from a concurrent `resume` had the same effect.

The lock is held across the build so two concurrent first-searches do not both
pay for it, and unlike `sync.Once` the waiter proceeds under **its own**
context rather than inheriting the first caller's cancellation.

A cancelled build returns the context error unwrapped; only a genuine failure
is wrapped in `ErrIndexUnavailable`. On a truly read-only archive every word
search re-attempts the build, which fails immediately on the first write — a
cheap price for not lying about why search is unavailable.

`EnsureFTS` has exactly one non-test caller and is unreachable from `Open`,
`OpenRW`, `migrate` and every constructor. Commands that never search must
never pay for the index.

---

## Relevant Source Files

| File | What it covers |
|------|---------------|
| `repository/migrations/*.sql` | Full schema and all views |
| `repository/generic.go` | Insert pattern (no upsert), latest-chunk CTE |
| `repository/dbmessage.go` | `DBMessage`, `threadCond()`, `IS_PARENT` logic, `JSON_EXTRACT` usage |
| `repository/dbfile.go` | `DBFile`, file mode stored directly |
| `repository/dbchannel.go` | `DBChannel`, channel type priority (INFO vs bulk) |
| `repository/dbuser.go` | `DBUser`, `S_USER` table, `Username()` |
| `repository/session.go` | `Session`, `PAR_SESSION_ID`, `Finalise()` |
| `repository/unused.go` | Abandoned gzip compression (`//go:build ignore`) |
| `dbase.go` | `DBP`, DB init pragmas, `Close()` finalises session |
| `split.go` | `InsertChunk()`, `insertPayload()` dispatch |
| `source.go` | `Source`, `Channels()` fallback, `Latest()` for resume |
| `assemble.go` | Chunk reassembly, parent message lookup |
| `../chunk.go` | `ChunkType` enum (0–11) |
| `../../structures/conversation.go` | `IsThreadStart()`, `LatestReplyNoReplies` sentinel |
| `../../fasttime/fasttime_x64.go` | `TS2int()` — timestamp → int64 encoding |
| `../../../../convert/transform/fileproc/fileproc.go` | `invalidModes` |
| `../../../../cmd/slackdump/internal/resume/resume.go` | Resume session logic |
