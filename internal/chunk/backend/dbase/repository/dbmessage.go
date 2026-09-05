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
	"iter"
	"log/slog"
	"runtime/trace"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/chunk"
	"github.com/rusq/slackdump/v4/internal/fasttime"
	"github.com/rusq/slackdump/v4/internal/structures"
)

type DBMessage struct {
	ID          int64   `db:"ID,omitempty"`
	ChunkID     int64   `db:"CHUNK_ID,omitempty"`
	ChannelID   string  `db:"CHANNEL_ID"`
	TS          string  `db:"TS"`
	ParentID    *int64  `db:"PARENT_ID,omitempty"`
	ThreadTS    *string `db:"THREAD_TS,omitempty"`
	LatestReply *string `db:"LATEST_REPLY,omitempty"`
	IsParent    bool    `db:"IS_PARENT"`
	Index       int     `db:"IDX"`
	NumFiles    int     `db:"NUM_FILES"`
	Text        string  `db:"TXT"`
	Data        []byte  `db:"DATA"`
}

func NewDBMessage(dbchunkID int64, idx int, channelID string, msg *slack.Message) (*DBMessage, error) {
	ts, err := fasttime.TS2int(msg.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("insertMessages fasttime: %w", err)
	}
	data, err := marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("insertMessages marshal: %w", err)
	}
	var parentID *int64
	if msg.ThreadTimestamp != "" {
		if parID, err := fasttime.TS2int(msg.ThreadTimestamp); err != nil {
			return nil, fmt.Errorf("insertMessages fasttime thread: %w", err)
		} else {
			parentID = &parID
		}
	}

	dbm := DBMessage{
		ID:          ts,
		ChunkID:     dbchunkID,
		ChannelID:   channelID,
		TS:          msg.Timestamp,
		ParentID:    parentID,
		ThreadTS:    orNull(msg.ThreadTimestamp != "", msg.ThreadTimestamp),
		LatestReply: orNull(msg.LatestReply != "", msg.LatestReply),
		IsParent:    structures.IsThreadStart(msg),
		Index:       idx,
		NumFiles:    len(msg.Files),
		Text:        msg.Text,
		Data:        data,
	}
	return &dbm, nil
}

func (dbm DBMessage) tablename() string {
	return "MESSAGE"
}

func (dbm DBMessage) userkey() []string {
	return slice("ID")
}

func (dbm DBMessage) columns() []string {
	return []string{
		"ID",
		"CHUNK_ID",
		"CHANNEL_ID",
		"TS",
		"PARENT_ID",
		"THREAD_TS",
		"IS_PARENT",
		"IDX",
		"NUM_FILES",
		"TXT",
		"DATA",
		"LATEST_REPLY",
	}
}

func (dbm DBMessage) values() []any {
	return []any{
		dbm.ID,
		dbm.ChunkID,
		dbm.ChannelID,
		dbm.TS,
		dbm.ParentID,
		dbm.ThreadTS,
		dbm.IsParent,
		dbm.Index,
		dbm.NumFiles,
		dbm.Text,
		dbm.Data,
		dbm.LatestReply,
	}
}

func (dbm DBMessage) Val() (slack.Message, error) {
	return unmarshalt[slack.Message](dbm.Data)
}

// MessageRepository provides an interface for working with messages in the
// database.
//
//go:generate mockgen -destination=mock_repository/mock_message.go . MessageRepository
type MessageRepository interface {
	Inserter[DBMessage]
	Chunker[DBMessage]
	Getter[DBMessage]
	// Count returns the number of messages in a channel.
	Count(ctx context.Context, conn sqlx.QueryerContext, channelID string) (int64, error)
	// CountBeforeID returns the number of channel-timeline messages with a
	// timestamp at or before ts.  The result is 1-based for an existing
	// message: the oldest message in the channel returns 1.
	CountBeforeID(ctx context.Context, conn sqlx.QueryerContext, channelID, ts string) (int64, error)
	// SearchMessages returns messages whose text contains query, newest first,
	// across the channel timeline AND thread replies.  An empty channelID
	// searches every conversation.  Matching is case-insensitive including
	// accented characters, and query is matched literally — LIKE
	// metacharacters in it carry no special meaning.
	SearchMessages(ctx context.Context, conn sqlx.QueryerContext, query, channelID string, limit int) (iter.Seq2[DBMessage, error], error)
	// SearchMessagesFTS returns messages whose text matches query via the
	// FTS5 full-text index, across the channel timeline AND thread replies,
	// deduplicated the way every other query in this file dedupes, with one
	// wrinkle worth stating precisely: of the rows that *match*, only the one
	// with the highest CHUNK_ID for a given message ID is returned. Matching
	// happens before the grouping, so a message later edited into text that no
	// longer matches is still found, under its older text. LIKE-mode
	// SearchMessages behaves identically, so the two modes agree. Unlike
	// SearchMessages, it matches whole tokens rather than substrings — a term
	// that is only part of a fixture word will not match. An empty channelID
	// searches every conversation. When byRelevance is false, results are
	// ordered newest first; when true, they are ordered by bm25 relevance
	// (best match first) instead of recency. The caller must have already
	// called EnsureFTS — SearchMessagesFTS assumes the index exists and does
	// not build or refresh it.
	SearchMessagesFTS(ctx context.Context, conn sqlx.QueryerContext, query, channelID string, byRelevance bool, limit int) (iter.Seq2[DBMessage, error], error)
	// AllForID returns all messages in a channel.
	AllForID(ctx context.Context, conn sqlx.QueryerContext, channelID string) (iter.Seq2[DBMessage, error], error)
	// PageForID returns a window of the channel timeline, ordered oldest
	// first.  A limit of 0 means no limit; an offset of 0 starts at the
	// beginning.
	PageForID(ctx context.Context, conn sqlx.QueryerContext, channelID string, limit, offset int) (iter.Seq2[DBMessage, error], error)
	// CountThread returns the number of messages in a thread.
	CountThread(ctx context.Context, conn sqlx.QueryerContext, channelID, threadID string) (int64, error)
	// AllForThread returns all messages in a thread, including parent message.
	AllForThread(ctx context.Context, conn sqlx.QueryerContext, channelID, threadID string) (iter.Seq2[DBMessage, error], error)
	// Sorted returns all thread and channel messages in ascending or descending
	// time order.
	Sorted(ctx context.Context, conn sqlx.QueryerContext, channelID string, order Order) (iter.Seq2[DBMessage, error], error)
	// CountUnfinished returns the number of unfinished threads in a channel.
	CountUnfinished(ctx context.Context, conn sqlx.QueryerContext, sessionID int64, channelID string) (int64, error)
	// CountThreadOnlyParts should return the number of parts in a complete
	// thread-only thread. If an unfinished or non-existent thread is
	// requested, it should return the sql.ErrNoRows error.
	CountThreadOnlyParts(ctx context.Context, conn sqlx.QueryerContext, sessionID int64, channelID, threadID string) (int64, error)
	// LatestMessages returns the latest message in each channel.
	LatestMessages(ctx context.Context, conn sqlx.QueryerContext) (iter.Seq2[LatestMessage, error], error)
	// LatestThreads returns the latest thread message in each channel.
	LatestThreads(ctx context.Context, conn sqlx.QueryerContext) (iter.Seq2[LatestThread, error], error)
}

var _ MessageRepository = messageRepository{}

type messageRepository struct {
	genericRepository[DBMessage]
}

func NewMessageRepository() MessageRepository {
	return messageRepository{newGenericRepository(DBMessage{})}
}

// channelTimelineCondition keeps thread replies out of a channel timeline while
// retaining parents from both thread-only and non-thread-only chunks. A
// self-referencing PARENT_ID identifies a parent even after its replies are deleted.
const channelTimelineCondition = " AND ((CH.TYPE_ID=0 AND (CH.THREAD_ONLY=FALSE OR CH.THREAD_ONLY IS NULL)) OR (CH.TYPE_ID=1 AND T.PARENT_ID=T.ID))"

func (r messageRepository) Count(ctx context.Context, conn sqlx.QueryerContext, channelID string) (int64, error) {
	return r.countTypeWhere(
		ctx,
		conn,
		queryParams{
			Where: "T.CHANNEL_ID = ?" + channelTimelineCondition,
			Binds: []any{channelID}},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

func (r messageRepository) CountBeforeID(ctx context.Context, conn sqlx.QueryerContext, channelID, ts string) (int64, error) {
	id, err := fasttime.TS2int(ts)
	if err != nil {
		return 0, fmt.Errorf("countBeforeID fasttime: %w", err)
	}
	return r.countTypeWhere(
		ctx,
		conn,
		queryParams{
			Where: "T.CHANNEL_ID = ? AND T.ID <= ?" + channelTimelineCondition,
			Binds: []any{channelID, id}},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

func (r messageRepository) SearchMessages(ctx context.Context, conn sqlx.QueryerContext, query, channelID string, limit int) (iter.Seq2[DBMessage, error], error) {
	// Deliberately no channelTimelineCondition: that condition keeps thread
	// replies out of a channel timeline, and search must reach them.
	where := lowerFn + "(T.TXT) LIKE ? ESCAPE '\\'"
	binds := []any{"%" + escapeLike(strings.ToLower(query)) + "%"}
	if channelID != "" {
		where += " AND T.CHANNEL_ID = ?"
		binds = append(binds, channelID)
	}
	return r.allOfTypeWhere(
		ctx,
		conn,
		queryParams{
			Where:   where,
			Binds:   binds,
			OrderBy: []string{"T.ID DESC"},
			Limit:   limit,
		},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

func (r messageRepository) SearchMessagesFTS(ctx context.Context, conn sqlx.QueryerContext, query, channelID string, byRelevance bool, limit int) (iter.Seq2[DBMessage, error], error) {
	// MATCH '' is a syntax error, so an empty ftsQuery result must never reach
	// the database: return an empty iterator instead of running anything.
	match := ftsQuery(query)
	if match == "" {
		return func(yield func(DBMessage, error) bool) {}, nil
	}

	if byRelevance {
		return r.searchMessagesFTSByRelevance(ctx, conn, match, channelID, limit)
	}

	// Deliberately no channelTimelineCondition, for the same reason as
	// SearchMessages: it would drop thread replies from the results.
	where := "T.ROWID IN (SELECT rowid FROM " + ftsTable + " WHERE " + ftsTable + " MATCH ?)"
	binds := []any{match}
	if channelID != "" {
		where += " AND T.CHANNEL_ID = ?"
		binds = append(binds, channelID)
	}
	return r.allOfTypeWhere(
		ctx,
		conn,
		queryParams{
			Where:   where,
			Binds:   binds,
			OrderBy: []string{"T.ID DESC"},
			Limit:   limit,
		},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

// searchMessagesFTSByRelevance is the bm25-ordered sibling of the plain "T.ROWID
// IN (...)" search above. That IN-form discards the rank FTS5 computes, so
// relevance order needs a statement that keeps it: a HITS CTE carries
// (rowid, bm25 rank) for every matching row.
//
// The dedup must run over the *matching* rows, not over every row: the LATEST
// select's WHERE carries the same "T.ROWID IN (...)" condition as the newest-
// first path above, so MAX(CHUNK_ID) is computed only among a message's
// versions that satisfy the query — exactly mirroring the newest-first path
// and SearchMessages. Deduping first and filtering by HITS afterwards would be
// worse: MESSAGE.ID is derived from the message timestamp alone, not scoped by
// channel, so an unrelated message in another channel that happens to share a
// timestamp — and therefore an ID — could win the MAX(CHUNK_ID) race and mask
// a real match while matching nothing itself. Filtering first keeps such a row
// out of the grouping entirely.
//
// It does not make the masking impossible, only rarer. When two rows sharing
// an ID *both* match, GROUP BY T.ID still collapses them to the higher
// CHUNK_ID and the other channel's hit is lost — an unscoped search over
// "needle in channel one" (C1, chunk 1) and "needle in channel two" (C2, chunk
// 2) returns one hit, not two. That is pre-existing behaviour of
// stmtLatestWhere, shared with the newest-first path and with LIKE-mode
// SearchMessages, and cross-channel ID collisions have not been observed in a
// real archive, so it is documented here rather than worked around.
//
// HITS is joined only to attach the rank of the row the dedup already chose,
// and T.ROWID is unique, so every output row is guaranteed exactly one HITS
// match.
//
// bm25() returns a more-negative score for a better match, so ORDER BY rank
// ascending is "best first".
func (r messageRepository) searchMessagesFTSByRelevance(ctx context.Context, conn sqlx.QueryerContext, match, channelID string, limit int) (iter.Seq2[DBMessage, error], error) {
	ctx, task := trace.NewTask(ctx, "searchMessagesFTSByRelevance")

	where := "T.ROWID IN (SELECT rowid FROM " + ftsTable + " WHERE " + ftsTable + " MATCH ?)"
	dedupBinds := []any{match}
	if channelID != "" {
		where += " AND T.CHANNEL_ID = ?"
		dedupBinds = append(dedupBinds, channelID)
	}
	latest, latestBinds := r.stmtLatestWhere(queryParams{Where: where, Binds: dedupBinds}, chunk.CMessages, chunk.CThreadMessages)

	var buf strings.Builder
	buf.WriteString("WITH HITS AS (\n")
	buf.WriteString("SELECT rowid AS rid, bm25(" + ftsTable + ") AS rank FROM " + ftsTable + " WHERE " + ftsTable + " MATCH ?\n")
	buf.WriteString(")\n")
	buf.WriteString("SELECT T.")
	buf.WriteString(strings.Join(r.t.columns(), ",T."))
	buf.WriteString(" FROM ")
	buf.WriteString(r.t.tablename())
	buf.WriteString(" AS T\n")
	buf.WriteString("JOIN (\n")
	buf.WriteString(latest)
	buf.WriteString("\n) AS L ON T.ID = L.ID AND T.CHUNK_ID = L.CHUNK_ID\n")
	buf.WriteString("JOIN HITS H ON H.rid = T.ROWID\n")
	buf.WriteString("ORDER BY H.rank")
	if limit > 0 {
		fmt.Fprintf(&buf, " LIMIT %d", limit)
	}

	stmt := buf.String()
	// match binds the top-level HITS CTE; latestBinds binds the identical
	// condition embedded in the LATEST select (see stmtLatestWhere).
	binds := append([]any{match}, latestBinds...)

	slog.DebugContext(ctx, "searchMessagesFTSByRelevance", "stmt", stmt, "binds", binds)

	rgn := trace.StartRegion(ctx, "searchMessagesFTSByRelevance.query")
	rows, err := conn.QueryxContext(ctx, rebind(conn, stmt), binds...)
	rgn.End()
	if err != nil {
		return nil, fmt.Errorf("searchMessagesFTSByRelevance: %w", err)
	}
	it := func(yield func(DBMessage, error) bool) {
		defer task.End()
		defer rows.Close()
		var t DBMessage
		for rows.Next() {
			if err := rows.StructScan(&t); err != nil {
				yield(t, fmt.Errorf("searchMessagesFTSByRelevance: %w", err))
				return
			}
			if !yield(t, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(t, fmt.Errorf("searchMessagesFTSByRelevance: %w", err))
			return
		}
	}
	return it, nil
}

func (r messageRepository) AllForID(ctx context.Context, conn sqlx.QueryerContext, channelID string) (iter.Seq2[DBMessage, error], error) {
	return r.allOfTypeWhere(
		ctx,
		conn,
		queryParams{
			Where:        "T.CHANNEL_ID = ?" + channelTimelineCondition,
			Binds:        []any{channelID},
			UserKeyOrder: true,
		},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

func (r messageRepository) PageForID(ctx context.Context, conn sqlx.QueryerContext, channelID string, limit, offset int) (iter.Seq2[DBMessage, error], error) {
	return r.allOfTypeWhere(
		ctx,
		conn,
		queryParams{
			Where:        "T.CHANNEL_ID = ?" + channelTimelineCondition,
			Binds:        []any{channelID},
			UserKeyOrder: true,
			Limit:        limit,
			Offset:       offset,
		},
		chunk.CMessages, chunk.CThreadMessages,
	)
}

// threadCond returns a condition for selecting messages that are part of a
// thread with additional filtering of thread_broadcast subtype.
func (r messageRepository) threadCond() string {
	var buf strings.Builder
	buf.WriteString("T.CHANNEL_ID = ? AND T.PARENT_ID = ? ")
	buf.WriteString("AND ( JSON_EXTRACT(T.DATA, '$.subtype') IS NULL ")
	buf.WriteString("OR (JSON_EXTRACT(T.DATA, '$.subtype') = 'thread_broadcast' AND CH.TYPE_ID = 1 )")
	buf.WriteString("   ) ")
	return buf.String()
}

func (r messageRepository) CountThread(ctx context.Context, conn sqlx.QueryerContext, channelID, threadID string) (int64, error) {
	parentID, err := fasttime.TS2int(threadID)
	if err != nil {
		return 0, fmt.Errorf("countThread fasttime: %w", err)
	}
	return r.countTypeWhere(ctx, conn, queryParams{Where: r.threadCond(), Binds: []any{channelID, parentID}}, chunk.CMessages, chunk.CThreadMessages)
}

func (r messageRepository) AllForThread(ctx context.Context, conn sqlx.QueryerContext, channelID, threadID string) (iter.Seq2[DBMessage, error], error) {
	parentID, err := fasttime.TS2int(threadID)
	if err != nil {
		return nil, fmt.Errorf("allForThread fasttime: %w", err)
	}
	return r.allOfTypeWhere(ctx, conn, queryParams{Where: r.threadCond(), Binds: []any{channelID, parentID}, UserKeyOrder: true}, chunk.CMessages, chunk.CThreadMessages)
}

func (r messageRepository) Sorted(ctx context.Context, conn sqlx.QueryerContext, channelID string, order Order) (iter.Seq2[DBMessage, error], error) {
	return r.allOfTypeWhere(ctx, conn, queryParams{Where: "T.CHANNEL_ID = ?", Binds: []any{channelID}, OrderBy: []string{"T.ID" + order.String()}}, chunk.CMessages, chunk.CThreadMessages)
}

func (r messageRepository) CountUnfinished(ctx context.Context, conn sqlx.QueryerContext, sessionID int64, channelID string) (int64, error) {
	ctx, task := trace.NewTask(ctx, "CountUnfinished")
	defer task.End()
	const stmt = "SELECT REF_COUNT FROM V_UNFINISHED_CHANNELS WHERE SESSION_ID = ? AND CHANNEL_ID = ?"
	var count int64
	if err := conn.QueryRowxContext(ctx, rebind(conn, stmt), sessionID, channelID).Scan(&count); err != nil {
		return 0, fmt.Errorf("countUnfinished query: %w", err)
	}
	return count, nil
}

func (r messageRepository) CountThreadOnlyParts(ctx context.Context, conn sqlx.QueryerContext, sessionID int64, channelID, threadID string) (int64, error) {
	ctx, task := trace.NewTask(ctx, "CountUnfinishedThreads")
	defer task.End()
	const stmt = "SELECT PARTS FROM V_THREAD_ONLY_THREADS WHERE SESSION_ID = ? AND CHANNEL_ID = ? AND THREAD_TS = ?"
	var count int64
	if err := conn.QueryRowxContext(ctx, rebind(conn, stmt), sessionID, channelID, threadID).Scan(&count); err != nil {
		return 0, fmt.Errorf("CountThreadOnlyParts query: %w", err)
	}
	return count, nil
}

type LatestMessage struct {
	ChannelID string `db:"CHANNEL_ID"`
	TS        string `db:"TS"`
	ID        int64  `db:"ID"`
}

type LatestThread struct {
	LatestMessage
	ThreadTS string `db:"THREAD_TS"`
	ParentID int64  `db:"PARENT_ID"`
}

func (r messageRepository) LatestMessages(ctx context.Context, conn sqlx.QueryerContext) (iter.Seq2[LatestMessage, error], error) {
	const stmt = "SELECT CHANNEL_ID, TS, ID FROM V_LATEST_MESSAGE"
	return query[LatestMessage](ctx, conn, stmt)
}

func (r messageRepository) LatestThreads(ctx context.Context, conn sqlx.QueryerContext) (iter.Seq2[LatestThread, error], error) {
	const stmt = "SELECT CHANNEL_ID, TS, ID, THREAD_TS, PARENT_ID FROM V_LATEST_THREAD"
	return query[LatestThread](ctx, conn, stmt)
}

func query[T any](ctx context.Context, conn sqlx.QueryerContext, stmt string, binds ...any) (iter.Seq2[T, error], error) {
	rows, err := conn.QueryxContext(ctx, stmt, binds...)
	if err != nil {
		return nil, err
	}
	iterFn := func(yield func(T, error) bool) {
		defer rows.Close()
		var t T
		for rows.Next() {
			if err := rows.StructScan(&t); err != nil {
				yield(t, err)
				return
			}
			if !yield(t, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(t, err)
			return
		}
	}
	return iterFn, nil
}
