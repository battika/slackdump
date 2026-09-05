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

package dbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/trace"
	"sync"
	"time"

	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase/repository"

	"github.com/jmoiron/sqlx"

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/chunk"
	"github.com/rusq/slackdump/v4/internal/fasttime"
	"github.com/rusq/slackdump/v4/internal/structures"
)

const preallocSz = 100 // preallocate slice size

// DefaultDBFile is the default database filename used when a directory
// is passed instead of a file path.
const DefaultDBFile = "slackdump.sqlite"

type Source struct {
	conn *sqlx.DB
	// canClose set to false when the connection is passed to the source
	// and should not be closed by the source.
	canClose bool

	// ftsMu guards ftsBuilt, which records that this Source's full-text index
	// has been brought up to date: one build per Source, not one per process
	// (a long-lived process opening several archives must index each one) and
	// not one per query (repeated searches must not repeatedly pay for it).
	//
	// Only success is remembered.  A failed build must be retried, because the
	// common failures are transient — the request context being cancelled (the
	// viewer aborts an in-flight search on the next keystroke) or the database
	// being busy while another process writes.  Caching those would disable
	// word search for the life of the Source and report it as a read-only
	// archive, which it is not.
	ftsMu    sync.Mutex
	ftsBuilt bool
}

// SearchModeWords and SearchModeContains select the matching strategy for
// [Source.SearchMessages]: SearchModeWords does FTS5 whole-token matching,
// SearchModeContains does a LIKE substring scan.
//
// mode is a plain string, deliberately: the viewer declares an unexported
// interface that *Source must satisfy, and dbase cannot import the viewer
// package, so a viewer-defined enum type could never appear in this
// signature. Each side keeps its own constants and converts at the boundary.
const (
	SearchModeWords    = "words"    // FTS5 whole-token matching
	SearchModeContains = "contains" // LIKE substring matching
)

// ErrIndexUnavailable is returned when word-mode search is requested but the
// full-text index cannot be created, typically because the archive file is
// read-only.  Callers should surface this rather than quietly running a
// different kind of search: the user chose a mode and is entitled to know it
// was not honoured.
var ErrIndexUnavailable = errors.New("full-text index unavailable")

// ErrIsDirectory is returned when a directory path is passed instead of
// a database file.
var ErrIsDirectory = fmt.Errorf("path is a directory")

// validateDBPath checks if path is a directory and returns a helpful error
// with a suggestion if the expected database file exists inside.
func validateDBPath(path string) error {
	// Check if path is a symlink and warn about it.
	li, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to stat path, continuing", "path", path, "error", err)
	} else if err == nil && li.Mode()&os.ModeSymlink != 0 {
		slog.Warn("database path is a symlink, following it to the target", "path", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Non-existent paths are allowed (for creating new databases).
			return nil
		}
		return fmt.Errorf("stat: %w", err)
	}
	if fi.IsDir() {
		dbFile := filepath.Join(path, DefaultDBFile)
		if _, err := os.Stat(dbFile); err == nil {
			return fmt.Errorf("%w: %s (did you mean %q?)", ErrIsDirectory, path, dbFile)
		}
		return fmt.Errorf("%w: %s (no %s found inside)", ErrIsDirectory, path, DefaultDBFile)
	}
	return nil
}

// Open attempts to open the database at given path for reading.
func Open(ctx context.Context, path string) (*Source, error) {
	if err := validateDBPath(path); err != nil {
		return nil, err
	}
	// migrate to the latest
	if err := migrate(ctx, path); err != nil {
		return nil, err
	}
	conn, err := sqlx.Open(repository.Driver, "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	return &Source{conn: conn, canClose: true}, nil
}

// OpenRW attempts to open the database at given path for reading and writing.
// Use [Open] when only read access is needed.
func OpenRW(ctx context.Context, path string) (*RWSource, error) {
	if err := validateDBPath(path); err != nil {
		return nil, err
	}
	if err := migrate(ctx, path); err != nil {
		return nil, err
	}
	conn, err := sqlx.Open(repository.Driver, "file:"+path+"?mode=rw")
	if err != nil {
		return nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	return &RWSource{Source: &Source{conn: conn, canClose: true}}, nil
}

// RWSource wraps [Source] with alias write operations.  It satisfies the
// viewer Aliaser interface together with the read methods promoted from
// the embedded [Source].
type RWSource struct {
	*Source
}

func (s *RWSource) SetAlias(id, alias string) error {
	ar := repository.NewAliasRepository()
	return ar.Set(context.Background(), s.conn, id, alias)
}

func (s *RWSource) DeleteAlias(id string) error {
	ar := repository.NewAliasRepository()
	return ar.Delete(context.Background(), s.conn, id)
}

func migrate(ctx context.Context, path string) error {
	conn, err := sql.Open(repository.Driver, path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := repository.Migrate(ctx, conn, false); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint"); err != nil {
		return err
	}
	return nil
}

// Close closes the database connection.  It is a noop
// if the [Source] was created with [Connect].
func (s *Source) Close() error {
	if !s.canClose {
		slog.Debug("not closing database connection, it was passed to the source")
		return nil
	}
	slog.Debug("closing database connection")
	if err := s.conn.Close(); err != nil {
		slog.Error("error closing database connection", "error", err)
		return err
	}
	return nil
}

// Channels returns all channels.  If the channel info is not available,
// it will attempt to get all channels.
func (s *Source) Channels(ctx context.Context) ([]slack.Channel, error) {
	cr := repository.NewChannelRepository()
	it, err := cr.AllOfType(ctx, s.conn, chunk.CChannelInfo)
	if err != nil {
		return nil, err
	}
	var chns []slack.Channel
	chns, err = collect(it, preallocSz)
	if err != nil {
		return nil, err
	}
	if len(chns) == 0 {
		// no channel info, try getting all channels
		it, err := cr.AllOfType(ctx, s.conn, chunk.CChannels)
		if err != nil {
			return nil, err
		}
		chns, err = collect(it, preallocSz)
		if err != nil {
			return nil, err
		}
	}
	// Use index-based iteration: range-value loop copies the struct,
	// so assigning Members to the copy would be silently discarded.
	for i := range chns {
		users, err := s.channelUsers(ctx, chns[i].ID, chns[i].NumMembers)
		if err != nil {
			return nil, err
		}
		chns[i].Members = users
	}

	return chns, nil
}

func (s *Source) channelUsers(ctx context.Context, channelID string, prealloc int) ([]string, error) {
	cur := repository.NewChannelUserRepository()
	users, err := cur.GetByChannelID(ctx, s.conn, channelID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []string{}, nil
		}
		return nil, err
	}
	us := make([]string, 0, prealloc)
	for c, err := range users {
		if err != nil {
			return nil, err
		}
		us = append(us, c.UserID)
	}
	return us, nil
}

func (s *Source) Users(ctx context.Context) ([]slack.User, error) {
	ur := repository.NewUserRepository()

	it, err := ur.AllOfType(ctx, s.conn, chunk.CUsers)
	if err != nil {
		return nil, err
	}
	return collect(it, preallocSz)
}

type valuer[T any] interface {
	Val() (T, error)
}

func valueIter[T any, D valuer[T]](it iter.Seq2[D, error]) iter.Seq2[T, error] {
	iterFn := func(yield func(T, error) bool) {
		for c, err := range it {
			if err != nil {
				var t T
				yield(t, err)
				return
			}
			if !yield(c.Val()) {
				return
			}
		}
	}
	return iterFn
}

func collect[T any, D valuer[T]](it iter.Seq2[D, error], sz int) ([]T, error) {
	vs := make([]T, 0, sz)
	for c, err := range it {
		if err != nil {
			return nil, err
		}
		v, err := c.Val()
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	return vs, nil
}

func (s *Source) AllMessages(ctx context.Context, channelID string) (iter.Seq2[slack.Message, error], error) {
	mr := repository.NewMessageRepository()
	it, err := mr.AllForID(ctx, s.conn, channelID)
	if err != nil {
		return nil, err
	}
	return valueIter(it), nil
}

// CountMessages returns the number of messages in the channel timeline.
func (s *Source) CountMessages(ctx context.Context, channelID string) (int64, error) {
	mr := repository.NewMessageRepository()
	return mr.Count(ctx, s.conn, channelID)
}

// MessagesPage returns a window of the channel timeline, oldest first.  A
// limit of 0 means no limit.
func (s *Source) MessagesPage(ctx context.Context, channelID string, limit, offset int) (iter.Seq2[slack.Message, error], error) {
	mr := repository.NewMessageRepository()
	it, err := mr.PageForID(ctx, s.conn, channelID, limit, offset)
	if err != nil {
		return nil, err
	}
	return valueIter(it), nil
}

// MessageOrdinal returns the 0-based position of ts within the channel
// timeline.  When ts is not itself a timeline message, it returns the position
// the message would occupy, which is what page resolution needs.
func (s *Source) MessageOrdinal(ctx context.Context, channelID, ts string) (int64, error) {
	mr := repository.NewMessageRepository()
	n, err := mr.CountBeforeID(ctx, s.conn, channelID, ts)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, nil
	}
	return n - 1, nil
}

// ensureFTS brings this Source's full-text index up to date, at most once per
// Source for as long as it keeps succeeding.
//
// The lock is held across the build on purpose.  It stops two concurrent
// first-searches from both paying for it, and — unlike sync.Once — the waiter
// then re-checks and, if the first attempt failed, proceeds under *its own*
// context instead of inheriting the first caller's cancellation.
//
// The cost of caching only success is that a genuinely unbuildable index (a
// read-only archive) is re-attempted on every word search.  That attempt fails
// on its first write, so it is cheap, and correctness is worth more than the
// saved syscall.
func (s *Source) ensureFTS(ctx context.Context) error {
	s.ftsMu.Lock()
	defer s.ftsMu.Unlock()
	if s.ftsBuilt {
		return nil
	}
	if err := repository.EnsureFTS(ctx, s.conn); err != nil {
		return err
	}
	s.ftsBuilt = true
	return nil
}

// SearchMessages returns up to limit messages matching query, across both
// channel timelines and thread replies.  An empty channelID searches every
// conversation.  truncated reports whether more matches existed than the
// limit allowed.
//
// mode selects the matching strategy.  SearchModeWords does FTS5 whole-token
// matching and lazily builds the full-text index on this Source's first
// word-mode call, retrying on the next call if that build fails; see
// [ErrIndexUnavailable] for the failure that does not go away.  Any other
// value, including an unrecognised one, runs a SearchModeContains LIKE
// substring scan — an unrecognised mode must not error, it degrades to the
// mode that always works. Results are newest first, unless byRelevance is set
// and mode is SearchModeWords, in which case they are ordered by bm25 score;
// byRelevance is ignored in contains mode, since a LIKE scan has no score to
// order by.
//
// Each returned message has Channel set, which is otherwise absent from stored
// conversations.history payloads; that is what lets callers work with plain
// slack.Message values instead of a dedicated hit type.
func (s *Source) SearchMessages(ctx context.Context, query, channelID, mode string, byRelevance bool, limit int) (msgs []slack.Message, truncated bool, err error) {
	if limit <= 0 {
		// A non-positive cap asks for nothing, so return nothing.  Guarding
		// here also keeps the msgs[:limit] trim below in range: the repository
		// treats a Limit of 0 or less as "no LIMIT clause", so a negative limit
		// would otherwise run unbounded and then slice with a negative bound.
		return nil, false, nil
	}
	mr := repository.NewMessageRepository()
	// One row beyond the cap tells us the result was truncated without a
	// second COUNT query.
	var it iter.Seq2[repository.DBMessage, error]
	if mode == SearchModeWords {
		if err := s.ensureFTS(ctx); err != nil {
			if ctx.Err() != nil {
				// The caller went away mid-build.  Reporting that as an
				// unavailable index would have the viewer explain a
				// read-only archive that is nothing of the sort.
				return nil, false, err
			}
			return nil, false, fmt.Errorf("%w: %w", ErrIndexUnavailable, err)
		}
		it, err = mr.SearchMessagesFTS(ctx, s.conn, query, channelID, byRelevance, limit+1)
	} else {
		it, err = mr.SearchMessages(ctx, s.conn, query, channelID, limit+1)
	}
	if err != nil {
		return nil, false, err
	}
	for dbm, err := range it {
		if err != nil {
			return nil, false, err
		}
		m, err := dbm.Val()
		if err != nil {
			return nil, false, err
		}
		m.Channel = dbm.ChannelID
		msgs = append(msgs, m)
	}
	if len(msgs) > limit {
		return msgs[:limit], true, nil
	}
	return msgs, false, nil
}

func (s *Source) AllThreadMessages(ctx context.Context, channelID, threadID string) (iter.Seq2[slack.Message, error], error) {
	mr := repository.NewMessageRepository()
	it, err := mr.AllForThread(ctx, s.conn, channelID, threadID)
	if err != nil {
		return nil, err
	}
	return valueIter(it), nil
}

func (s *Source) Sorted(ctx context.Context, channelID string, desc bool, cb func(ts time.Time, msg *slack.Message) error) error {
	mr := repository.NewMessageRepository()
	it, err := mr.Sorted(ctx, s.conn, channelID, repository.Asc)
	if err != nil {
		return err
	}
	for c, err := range it {
		if err != nil {
			return err
		}
		v, err := c.Val()
		if err != nil {
			return err
		}
		if err := cb(fasttime.Int2Time(c.ID), &v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) ChannelInfo(ctx context.Context, channelID string) (*slack.Channel, error) {
	cr := repository.NewChannelRepository()
	c, err := cr.Get(ctx, s.conn, channelID)
	if err != nil {
		return nil, err
	}
	v, err := c.Val()
	if err != nil {
		return nil, err
	}
	users, err := s.channelUsers(ctx, v.ID, v.NumMembers)
	if err != nil {
		return nil, err
	}
	v.Members = users

	return &v, nil
}

func (s *Source) WorkspaceInfo(ctx context.Context) (*slack.AuthTestResponse, error) {
	cr := repository.NewWorkspaceRepository()
	dbw, err := cr.GetWorkspace(ctx, s.conn)
	if err != nil {
		return nil, err
	}
	w, err := dbw.Val()
	return &w, err
}

func (s *Source) Alias(id string) (string, bool, error) {
	ar := repository.NewAliasRepository()
	a, err := ar.Get(context.Background(), s.conn, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return a.Alias, true, nil
}

func (s *Source) Aliases() (map[string]string, error) {
	ar := repository.NewAliasRepository()
	aa, err := ar.All(context.Background(), s.conn)
	if err != nil {
		return nil, err
	}
	mm := make(map[string]string, len(aa))
	for _, a := range aa {
		mm[a.ChannelID] = a.Alias
	}
	return mm, nil
}

func (s *Source) Latest(ctx context.Context) (map[structures.SlackLink]time.Time, error) {
	ctx, task := trace.NewTask(ctx, "Latest")
	defer task.End()

	r := repository.NewMessageRepository()
	m := make(map[structures.SlackLink]time.Time, preallocSz)
	slog.DebugContext(ctx, "fetching latest messages")
	itm, err := r.LatestMessages(ctx, s.conn)
	if err != nil {
		return nil, err
	}
	for c, err := range itm {
		if err != nil {
			return nil, err
		}
		sl := structures.SlackLink{
			Channel: c.ChannelID,
		}
		m[sl] = fasttime.Int2Time(c.ID)
	}
	slog.DebugContext(ctx, "fetching latest threads")
	ittm, err := r.LatestThreads(ctx, s.conn)
	if err != nil {
		return nil, err
	}
	for c, err := range ittm {
		if err != nil {
			return nil, err
		}
		sl := structures.SlackLink{
			Channel:  c.ChannelID,
			ThreadTS: c.ThreadTS,
		}
		m[sl] = fasttime.Int2Time(c.ID)
	}
	return m, nil
}

func (src *Source) ToChunk(ctx context.Context, e chunk.Encoder, sessID int64) error {
	if sessID < 1 {
		return ErrInvalidSessionID
	}
	sr := repository.NewSessionRepository()
	sess, err := sr.Get(ctx, src.conn, sessID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidSessionID
		}
		return err
	}
	if !sess.Finished {
		return ErrIncomplete
	}

	cr := repository.NewChunkRepository()
	it, err := cr.All(ctx, src.conn, sessID)
	if err != nil {
		return err
	}
	for dbchunk, err := range it {
		if err != nil {
			return err
		}
		fn, ok := assemblers[dbchunk.TypeID]
		if !ok {
			return chunk.ErrUnsupChunkType
		}
		chunk, err := fn(ctx, src.conn, &dbchunk)
		if err != nil {
			return err
		}
		if err := e.Encode(ctx, chunk); err != nil {
			return fmt.Errorf("error converting chunk %d[%s]: %w", dbchunk.ID, dbchunk.TypeID, err)
		}
	}
	return nil
}

func (src *Source) Sessions(ctx context.Context) ([]repository.Session, error) {
	sr := repository.NewSessionRepository()
	return sr.All(ctx, src.conn)
}
