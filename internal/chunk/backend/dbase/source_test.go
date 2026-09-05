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
	"iter"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/rusq/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/rusq/slackdump/v4/internal/fixtures"

	"github.com/rusq/slackdump/v4/internal/chunk"
	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase/repository"
	"github.com/rusq/slackdump/v4/internal/chunk/mock_chunk"
	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/internal/testutil"
)

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	type args struct {
		ctx  context.Context
		path string
	}
	tests := []struct {
		name    string
		args    args
		checkFn utilityFunc
		fn      any
		wantErr bool
	}{
		{
			name: "opens and migrates the database",
			args: args{
				ctx:  t.Context(),
				path: filepath.Join(dir, t.Name()+".db"),
			},
			checkFn: checkGooseTable,
			wantErr: false,
		},
		{
			name: "rejects directory",
			args: args{
				ctx:  context.Background(),
				path: t.TempDir(),
			},
			wantErr: true,
		},
		{
			name: "rejects directory with OpenRW",
			args: args{
				ctx:  context.Background(),
				path: t.TempDir(),
			},
			wantErr: true,
			fn:      OpenRW,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *Source
			var err error
			if tt.fn != nil {
				switch fn := tt.fn.(type) {
				case func(context.Context, string) (*Source, error):
					got, err = fn(tt.args.ctx, tt.args.path)
				case func(context.Context, string) (*RWSource, error):
					rw, err2 := fn(tt.args.ctx, tt.args.path)
					if err2 == nil {
						rw.Close()
					}
					err = err2
				default:
					t.Fatalf("unsupported fn type %T", tt.fn)
				}
			} else {
				got, err = Open(tt.args.ctx, tt.args.path)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("Open() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != nil {
				defer got.Close()
			}
			if tt.checkFn != nil {
				tt.checkFn(t, testutil.TestDBDSN(t, tt.args.path))
			}
		})
	}
}

func Test_validateDBPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-denied stat behavior differs on windows")
	}

	dir := t.TempDir()

	regularFile := filepath.Join(dir, "regular.db")
	require.NoError(t, os.WriteFile(regularFile, nil, 0644))

	dbDir := t.TempDir()
	dbFile := filepath.Join(dbDir, "slackdump.sqlite")
	require.NoError(t, os.WriteFile(dbFile, nil, 0644))

	emptyDir := t.TempDir()

	// Create a symlink to a regular file
	symlinkFile := filepath.Join(dir, "symlink.db")
	require.NoError(t, os.Symlink(regularFile, symlinkFile))

	// Create a symlink to a directory
	symlinkDir := filepath.Join(dir, "symlink_dir")
	require.NoError(t, os.Symlink(dbDir, symlinkDir))

	lockedDir := filepath.Join(dir, "locked")
	require.NoError(t, os.Mkdir(lockedDir, 0755))
	permissionDeniedPath := filepath.Join(lockedDir, "db.sqlite")
	require.NoError(t, os.Chmod(lockedDir, 0000))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(lockedDir, 0755))
	})
	_, permissionProbeErr := os.Stat(permissionDeniedPath)
	permissionDeniedSupported := errors.Is(permissionProbeErr, os.ErrPermission)

	tests := []struct {
		name       string
		path       string
		wantErr    bool
		wantErrIs  error
		errContain string
	}{
		{
			name:    "non-existent path is allowed",
			path:    filepath.Join(dir, "nonexistent.db"),
			wantErr: false,
		},
		{
			name:       "directory without slackdump.sqlite",
			path:       emptyDir,
			wantErr:    true,
			wantErrIs:  ErrIsDirectory,
			errContain: "no slackdump.sqlite found inside",
		},
		{
			name:       "directory with slackdump.sqlite suggests correct path",
			path:       dbDir,
			wantErr:    true,
			wantErrIs:  ErrIsDirectory,
			errContain: "did you mean",
		},
		{
			name:    "regular file is allowed",
			path:    regularFile,
			wantErr: false,
		},
		{
			name:    "symlink to file is allowed (logs warning)",
			path:    symlinkFile,
			wantErr: false,
		},
		{
			name:       "symlink to directory follows target and rejects",
			path:       symlinkDir,
			wantErr:    true,
			wantErrIs:  ErrIsDirectory,
			errContain: "did you mean",
		},
		{
			name:       "permission denied while stating path is returned",
			path:       permissionDeniedPath,
			wantErr:    true,
			wantErrIs:  os.ErrPermission,
			errContain: "stat:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErrIs == os.ErrPermission && !permissionDeniedSupported {
				t.Skipf("skipping permission-denied case: os.Stat(%q) returned %v", permissionDeniedPath, permissionProbeErr)
			}
			err := validateDBPath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, tt.wantErrIs)
				if tt.errContain != "" {
					require.Contains(t, err.Error(), tt.errContain)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSource_Close(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{
			name: "closes the connection",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			wantErr: false,
		},
		{
			name: "does not close the connection",
			fields: fields{
				conn:     testDB(t),
				canClose: false,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			t.Cleanup(func() {
				if err := s.conn.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := s.Close(); (err != nil) != tt.wantErr {
				t.Errorf("Source.Close() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSource_Channels(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []slack.Channel
		wantErr bool
	}{
		{
			name: "returns channels",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx: t.Context(),
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				t.Helper()
				ctx := t.Context()
				dbp, err := New(ctx, conn.(*sqlx.DB), SessionInfo{})
				if err != nil {
					t.Fatal(err)
				}
				channels := fixtures.Load[[]slack.Channel](fixtures.TestChannelsJSON)
				for _, ch := range channels {
					if err := dbp.Encode(ctx, &chunk.Chunk{Type: chunk.CChannelInfo, Channel: &ch}); err != nil {
						t.Error(err)
					}
					if len(ch.Members) > 0 {
						if err := dbp.Encode(ctx, &chunk.Chunk{Type: chunk.CChannelUsers, ChannelID: ch.ID, ChannelUsers: ch.Members}); err != nil {
							t.Error(err)
						}
					}
				}
			},
			want:    fixtures.Load[[]slack.Channel](fixtures.TestChannelsJSON),
			wantErr: false,
		},
		{
			name: "should return chunk.Channel if no ChannelInfo chunks are present",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx: t.Context(),
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				t.Helper()
				ctx := t.Context()
				dbp, err := New(ctx, conn.(*sqlx.DB), SessionInfo{})
				if err != nil {
					t.Fatal(err)
				}
				channels := fixtures.Load[[]slack.Channel](fixtures.TestChannelsJSON)
				if err := dbp.Encode(ctx, &chunk.Chunk{Type: chunk.CChannels, Channels: channels}); err != nil {
					t.Error(err)
				}
			},
			want:    fixtures.Load[[]slack.Channel](fixtures.TestChannelsJSON),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.Channels(tt.args.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.Channels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			// channels are sorted by name
			sort.Slice(tt.want, func(i, j int) bool {
				return tt.want[i].Name < tt.want[j].Name
			})
			// Channels() always populates Members (nil → []string{})
			for i := range tt.want {
				if tt.want[i].Members == nil {
					tt.want[i].Members = []string{}
				}
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func checkGooseTable(t *testing.T, conn repository.PrepareExtContext) {
	t.Helper()
	var n int
	if err := conn.QueryRowxContext(t.Context(), "SELECT COUNT(*) FROM goose_db_version").Scan(&n); err != nil {
		t.Error(err)
	}
	if n == 0 {
		t.Error("database not migrated")
	}
}

func Test_migrate(t *testing.T) {
	dir := t.TempDir()
	type args struct {
		ctx  context.Context
		path string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		checkFn utilityFunc
	}{
		{
			name: "migrates the database",
			args: args{
				ctx:  t.Context(),
				path: filepath.Join(dir, t.Name()+".db"),
			},
			wantErr: false,
			checkFn: checkGooseTable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := migrate(tt.args.ctx, tt.args.path); (err != nil) != tt.wantErr {
				t.Errorf("migrate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkFn != nil {
				tt.checkFn(t, testutil.TestDBDSN(t, tt.args.path))
			}
		})
	}
}

func prepTestChunk(c ...*chunk.Chunk) utilityFunc {
	return func(t *testing.T, conn repository.PrepareExtContext) {
		t.Helper()
		ctx := t.Context()
		dbp, err := New(ctx, conn.(*sqlx.DB), SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range c {
			if err := dbp.Encode(ctx, ch); err != nil {
				t.Error(err)
			}
		}
	}
}

func TestSource_channelUsers(t *testing.T) {
	testUsers := []string{"U01", "U02", "U03"}
	prepTestChannelUsers := func(ch string) func(t *testing.T, conn repository.PrepareExtContext) {
		return prepTestChunk(&chunk.Chunk{Type: chunk.CChannelUsers, ChannelID: ch, ChannelUsers: testUsers})
	}
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx       context.Context
		channelID string
		prealloc  int
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []string
		wantErr bool
	}{
		{
			name: "returns channel users",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx:       t.Context(),
				channelID: "C01",
				prealloc:  10,
			},
			prepFn: prepTestChannelUsers("C01"),
			want:   testUsers,
		},
		{
			name: "returns empty slice if no users",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx:       t.Context(),
				channelID: "C02",
				prealloc:  10,
			},
			prepFn: prepTestChannelUsers("C01"),
			want:   []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.channelUsers(tt.args.ctx, tt.args.channelID, tt.args.prealloc)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.channelUsers() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Source.channelUsers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSource_Users(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []slack.User
		wantErr bool
	}{
		{
			name: "returns users",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx: t.Context(),
			},
			prepFn: prepTestChunk(&chunk.Chunk{Type: chunk.CUsers, Users: fixtures.Load[[]slack.User](fixtures.UsersJSON)}),
			want:   testutil.RoundTripJSON(t, fixtures.Load[[]slack.User](fixtures.UsersJSON)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.Users(tt.args.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.Users() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			sort.Slice(tt.want, func(i, j int) bool { // users are sorted by ID.
				return tt.want[i].ID < tt.want[j].ID
			})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSource_AllMessages(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx       context.Context
		channelID string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []testutil.TestResult[slack.Message]
		wantErr bool
	}{
		{
			name: "returns messages",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx:       t.Context(),
				channelID: "C01",
			},
			prepFn: prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: fixtures.Load[[]slack.Message](fixtures.TestChannelEveryoneMessagesNativeExport)}),
			want:   testutil.SliceToTestResult(fixtures.Load[[]slack.Message](fixtures.TestChannelEveryoneMessagesNativeExport)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.AllMessages(tt.args.ctx, tt.args.channelID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.AllMessages() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			sort.Slice(tt.want, func(i, j int) bool { // messages are sorted by timestamp.
				return tt.want[i].V.Timestamp < tt.want[j].V.Timestamp
			})
			testutil.AssertIterResult(t, tt.want, got)
		})
	}
}

func TestSource_AllThreadMessages(t *testing.T) {
	threadMsg := []slack.Message{
		{Msg: slack.Msg{Timestamp: "1234567890.000001", ThreadTimestamp: "1234567890.000001"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000002", ThreadTimestamp: "1234567890.000001"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000003", ThreadTimestamp: "1234567890.000001"}},
	}
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx       context.Context
		channelID string
		threadID  string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []testutil.TestResult[slack.Message]
		wantErr bool
	}{
		{
			name: "returns thread messages",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx:       t.Context(),
				channelID: "C01",
				threadID:  threadMsg[0].ThreadTimestamp,
			},
			prepFn: prepTestChunk(&chunk.Chunk{Type: chunk.CThreadMessages, ChannelID: "C01", Parent: &threadMsg[0], Messages: threadMsg}),
			want:   testutil.SliceToTestResult(threadMsg),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.AllThreadMessages(tt.args.ctx, tt.args.channelID, tt.args.threadID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.AllThreadMessages() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			sort.Slice(tt.want, func(i, j int) bool { // messages are sorted by timestamp.
				return tt.want[i].V.Timestamp < tt.want[j].V.Timestamp
			})
			testutil.AssertIterResult(t, tt.want, got)
		})
	}
}

func TestSource_Sorted(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx       context.Context
		channelID string
		desc      bool
		cb        func(ts time.Time, msg *slack.Message) error
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			if err := s.Sorted(tt.args.ctx, tt.args.channelID, tt.args.desc, tt.args.cb); (err != nil) != tt.wantErr {
				t.Errorf("Source.Sorted() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSource_ChannelInfo(t *testing.T) {
	testChannel := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{
				ID:         "C1234567890",
				Created:    1580000000,
				IsOpen:     false,
				NumMembers: 3,
			},
			Name:       "test-channel",
			Creator:    "",
			IsArchived: false,
			Members:    []string{"U01", "U02", "U03"},
		},
		IsChannel: true,
		IsGeneral: true,
		IsMember:  true,
	}

	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx       context.Context
		channelID string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    *slack.Channel
		wantErr bool
	}{
		{
			name: "returns channel info",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx:       t.Context(),
				channelID: testChannel.ID,
			},
			prepFn: prepTestChunk(
				&chunk.Chunk{Type: chunk.CChannelInfo, Channel: &testChannel},
				&chunk.Chunk{Type: chunk.CChannelUsers, ChannelID: testChannel.ID, ChannelUsers: testChannel.Members},
			),
			want: &testChannel,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.ChannelInfo(tt.args.ctx, tt.args.channelID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.ChannelInfo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Source.ChannelInfo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSource_WorkspaceInfo(t *testing.T) {
	testAuthTest := &slack.AuthTestResponse{
		URL:          "https://test.slack.com/",
		Team:         "Test Team",
		User:         "Test User",
		TeamID:       "T1234567890",
		UserID:       "U1234567890",
		EnterpriseID: "E1234567890",
		BotID:        "B1234567890",
	}
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    *slack.AuthTestResponse
		wantErr bool
	}{
		{
			name: "returns workspace info",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx: t.Context(),
			},
			prepFn: prepTestChunk(&chunk.Chunk{Type: chunk.CWorkspaceInfo, WorkspaceInfo: testAuthTest}),
			want:   testAuthTest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.WorkspaceInfo(tt.args.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.WorkspaceInfo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Source.WorkspaceInfo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSource_AliasLifecycle(t *testing.T) {
	conn := testDB(t)
	s := &RWSource{
		Source: &Source{
			conn:     conn,
			canClose: true,
		},
	}

	alias, ok, err := s.Alias("C123")
	if err != nil {
		t.Fatalf("Source.Alias() initial error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("Source.Alias() initial ok = %v, want false", ok)
	}
	if alias != "" {
		t.Fatalf("Source.Alias() initial alias = %q, want empty", alias)
	}

	if err := s.SetAlias("C123", "alpha"); err != nil {
		t.Fatalf("Source.SetAlias() error = %v, want nil", err)
	}

	alias, ok, err = s.Alias("C123")
	if err != nil {
		t.Fatalf("Source.Alias() after set error = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("Source.Alias() after set ok = %v, want true", ok)
	}
	if alias != "alpha" {
		t.Fatalf("Source.Alias() after set alias = %q, want %q", alias, "alpha")
	}

	if err := s.SetAlias("C123", "beta"); err != nil {
		t.Fatalf("Source.SetAlias() update error = %v, want nil", err)
	}

	aliases, err := s.Aliases()
	if err != nil {
		t.Fatalf("Source.Aliases() error = %v, want nil", err)
	}
	if got := aliases["C123"]; got != "beta" {
		t.Fatalf("Source.Aliases()[C123] = %q, want %q", got, "beta")
	}

	if err := s.DeleteAlias("C123"); err != nil {
		t.Fatalf("Source.DeleteAlias() error = %v, want nil", err)
	}

	alias, ok, err = s.Alias("C123")
	if err != nil {
		t.Fatalf("Source.Alias() after delete error = %v, want nil", err)
	}
	if ok {
		t.Fatalf("Source.Alias() after delete ok = %v, want false", ok)
	}
	if alias != "" {
		t.Fatalf("Source.Alias() after delete alias = %q, want empty", alias)
	}

	repo := repository.NewAliasRepository()
	if _, err := repo.Get(t.Context(), conn, "C123"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("alias row still present, err = %v; want sql.ErrNoRows", err)
	}
}

func TestSource_Latest(t *testing.T) {
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    map[structures.SlackLink]time.Time
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := s.Latest(tt.args.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.Latest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Source.Latest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSource_ToChunk(t *testing.T) {
	type fields struct {
		// conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
		// e      chunk.Encoder
		sessID int64
	}
	tests := []struct {
		name     string
		fields   fields
		args     args
		prepFn   utilityFunc
		expectFn func(me *mock_chunk.MockEncoder)
		wantErr  bool
	}{
		{
			name: "encodes chunk",
			fields: fields{
				canClose: true,
			},
			args: args{
				ctx:    t.Context(),
				sessID: 1,
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				prepChunk(chunk.CMessages)(t, conn)
				mr := repository.NewMessageRepository()
				dbm1, _ := repository.NewDBMessage(1, 0, "C12345", testMsg1)
				dbm2, _ := repository.NewDBMessage(1, 1, "C12345", testMsg2)
				if err := mr.Insert(t.Context(), conn, dbm1, dbm2); err != nil {
					t.Error(err)
				}
			},
			expectFn: func(me *mock_chunk.MockEncoder) {
				me.EXPECT().Encode(gomock.Any(), gomock.Any()).Return(nil).Times(1)
			},
		},
		{
			name:   "encode error",
			fields: fields{},
			args: args{
				ctx:    t.Context(),
				sessID: 1,
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				prepChunk(chunk.CMessages)(t, conn)
				mr := repository.NewMessageRepository()
				dbm1, _ := repository.NewDBMessage(1, 0, "C12345", testMsg1)
				dbm2, _ := repository.NewDBMessage(1, 1, "C12345", testMsg2)
				if err := mr.Insert(t.Context(), conn, dbm1, dbm2); err != nil {
					t.Error(err)
				}
			},
			expectFn: func(me *mock_chunk.MockEncoder) {
				me.EXPECT().Encode(gomock.Any(), gomock.Any()).Return(assert.AnError).Times(1)
			},
			wantErr: true,
		},
		{
			name:   "no such session",
			fields: fields{},
			args: args{
				ctx:    t.Context(),
				sessID: 2,
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				prepChunk(chunk.CMessages)(t, conn)
				mr := repository.NewMessageRepository()
				dbm1, _ := repository.NewDBMessage(1, 0, "C12345", testMsg1)
				dbm2, _ := repository.NewDBMessage(1, 1, "C12345", testMsg2)
				if err := mr.Insert(t.Context(), conn, dbm1, dbm2); err != nil {
					t.Error(err)
				}
			},
			wantErr: true,
		},
		{
			name: "invalid session id",
			fields: fields{
				canClose: true,
			},
			args: args{
				ctx:    t.Context(),
				sessID: 0,
			},
			prepFn: func(t *testing.T, conn repository.PrepareExtContext) {
				prepChunk(chunk.CMessages)(t, conn)
				mr := repository.NewMessageRepository()
				dbm1, _ := repository.NewDBMessage(1, 0, "C12345", testMsg1)
				dbm2, _ := repository.NewDBMessage(1, 1, "C12345", testMsg2)
				if err := mr.Insert(t.Context(), conn, dbm1, dbm2); err != nil {
					t.Error(err)
				}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			me := mock_chunk.NewMockEncoder(ctrl)
			if tt.expectFn != nil {
				tt.expectFn(me)
			}
			src := &Source{
				conn:     testPersistentDB(t),
				canClose: tt.fields.canClose,
			}
			if tt.prepFn != nil {
				tt.prepFn(t, src.conn)
			}
			if err := src.ToChunk(tt.args.ctx, me, tt.args.sessID); (err != nil) != tt.wantErr {
				t.Errorf("Source.ToChunk() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSource_Sessions(t *testing.T) {
	testCreatedAt := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	testUpdatedAt := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	sessions := []repository.Session{
		{ID: 1, CreatedAt: testCreatedAt, UpdatedAt: testUpdatedAt, Mode: "test"},
		{ID: 2, CreatedAt: testCreatedAt, UpdatedAt: testUpdatedAt, Mode: "test"},
		{ID: 3, CreatedAt: testCreatedAt, UpdatedAt: testUpdatedAt, Mode: "test"},
	}
	type fields struct {
		conn     *sqlx.DB
		canClose bool
	}
	type args struct {
		ctx context.Context
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		prepFn  utilityFunc
		want    []repository.Session
		wantErr bool
	}{
		{
			name: "returns sessions",
			fields: fields{
				conn:     testDB(t),
				canClose: true,
			},
			args: args{
				ctx: t.Context(),
			},
			prepFn: func(t *testing.T, ec repository.PrepareExtContext) {
				sr := repository.NewSessionRepository()
				for _, s := range sessions {
					if _, err := sr.Insert(t.Context(), ec, &s); err != nil {
						t.Error(err)
					}
				}
			},
			want:    sessions,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepFn != nil {
				tt.prepFn(t, tt.fields.conn)
			}
			src := &Source{
				conn:     tt.fields.conn,
				canClose: tt.fields.canClose,
			}
			got, err := src.Sessions(tt.args.ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Source.Sessions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// pagingTestMessages is the shared fixture for the Source paging tests.  The
// timestamps are deliberately realistic (10-digit seconds, 6-digit
// microseconds): fasttime.TS2int concatenates the digits either side of the
// dot without decimal alignment, so ordering only holds between timestamps of
// equal digit width.
var pagingTestMessages = []slack.Message{
	{Msg: slack.Msg{Timestamp: "1234567890.000001", Text: "one"}},
	{Msg: slack.Msg{Timestamp: "1234567890.000002", Text: "two"}},
	{Msg: slack.Msg{Timestamp: "1234567890.000003", Text: "three"}},
}

// newPagingSource returns a Source over a database holding
// pagingTestMessages in channel C01.
func newPagingSource(t *testing.T) *Source {
	t.Helper()
	conn := testDB(t)
	prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: pagingTestMessages})(t, conn)
	return &Source{conn: conn, canClose: true}
}

// collectText drains it, returning the text of each message.
func collectText(t *testing.T, it iter.Seq2[slack.Message, error]) []string {
	t.Helper()
	var got []string
	for m, err := range it {
		if err != nil {
			t.Fatalf("iteration error = %v", err)
		}
		got = append(got, m.Text)
	}
	return got
}

func TestSource_CountMessages(t *testing.T) {
	got, err := newPagingSource(t).CountMessages(t.Context(), "C01")
	if err != nil {
		t.Fatalf("CountMessages() error = %v", err)
	}
	if got != 3 {
		t.Errorf("CountMessages() = %d, want 3", got)
	}
}

func TestSource_MessagesPage(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		offset int
		want   []string
	}{
		{"first window", 2, 0, []string{"one", "two"}},
		{"second window", 2, 2, []string{"three"}},
		{"offset past the end", 2, 99, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it, err := newPagingSource(t).MessagesPage(t.Context(), "C01", tt.limit, tt.offset)
			if err != nil {
				t.Fatalf("MessagesPage() error = %v", err)
			}
			if got := collectText(t, it); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MessagesPage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSource_MessageOrdinal(t *testing.T) {
	tests := []struct {
		name string
		ts   string
		want int64
	}{
		{"first message", "1234567890.000001", 0},
		{"second message", "1234567890.000002", 1},
		{"third message", "1234567890.000003", 2},
		{"older than everything", "1.000000", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newPagingSource(t).MessageOrdinal(t.Context(), "C01", tt.ts)
			if err != nil {
				t.Fatalf("MessageOrdinal() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("MessageOrdinal() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSource_SearchMessages(t *testing.T) {
	msgs := []slack.Message{
		{Msg: slack.Msg{Timestamp: "1234567890.000001", Text: "alpha login"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000002", Text: "beta login"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000003", Text: "gamma"}},
	}
	newSrc := func(t *testing.T) *Source {
		t.Helper()
		conn := testDB(t)
		prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: msgs})(t, conn)
		return &Source{conn: conn, canClose: true}
	}

	t.Run("returns newest first and stamps the channel", func(t *testing.T) {
		got, truncated, err := newSrc(t).SearchMessages(t.Context(), "login", "", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if truncated {
			t.Error("truncated = true, want false")
		}
		if len(got) != 2 {
			t.Fatalf("got %d hits, want 2", len(got))
		}
		if got[0].Text != "beta login" || got[1].Text != "alpha login" {
			t.Errorf("hits = %q, %q; want newest first", got[0].Text, got[1].Text)
		}
		for i, m := range got {
			if m.Channel != "C01" {
				t.Errorf("hit %d Channel = %q, want C01", i, m.Channel)
			}
		}
	})

	t.Run("reports truncation and trims to the cap", func(t *testing.T) {
		got, truncated, err := newSrc(t).SearchMessages(t.Context(), "login", "", SearchModeContains, false, 1)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if !truncated {
			t.Error("truncated = false, want true")
		}
		if len(got) != 1 {
			t.Errorf("got %d hits, want exactly the cap (1)", len(got))
		}
	})

	t.Run("exactly at the cap is not truncated", func(t *testing.T) {
		got, truncated, err := newSrc(t).SearchMessages(t.Context(), "login", "", SearchModeContains, false, 2)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if truncated {
			t.Error("truncated = true, want false when the result exactly fills the cap")
		}
		if len(got) != 2 {
			t.Errorf("got %d hits, want 2", len(got))
		}
	})

	t.Run("scoped to a channel", func(t *testing.T) {
		got, _, err := newSrc(t).SearchMessages(t.Context(), "login", "C01", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d hits, want 2", len(got))
		}
		got, _, err = newSrc(t).SearchMessages(t.Context(), "login", "CNOPE", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d hits for an unknown channel, want 0", len(got))
		}
	})

	t.Run("non-positive limits return nothing rather than panicking", func(t *testing.T) {
		// A negative limit used to reach msgs[:limit] and panic: the
		// repository reads Limit <= 0 as "no LIMIT clause", so the query ran
		// unbounded and the trim then sliced with a negative bound.
		for _, limit := range []int{0, -1} {
			got, truncated, err := newSrc(t).SearchMessages(t.Context(), "login", "", SearchModeContains, false, limit)
			if err != nil {
				t.Fatalf("limit=%d: SearchMessages() error = %v", limit, err)
			}
			if len(got) != 0 || truncated {
				t.Errorf("limit=%d: got %d hits truncated=%v, want 0/false", limit, len(got), truncated)
			}
		}
	})

	t.Run("no matches", func(t *testing.T) {
		got, truncated, err := newSrc(t).SearchMessages(t.Context(), "zzz", "", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if len(got) != 0 || truncated {
			t.Errorf("got %d hits truncated=%v, want 0/false", len(got), truncated)
		}
	})

	// loginMsgs gives word mode a real whole-token/substring contrast: "log"
	// is a substring of both "login" and "logout" but is nobody's whole
	// token, so it is the term the whole feature exists to handle differently.
	loginMsgs := []slack.Message{
		{Msg: slack.Msg{Timestamp: "1234567890.000001", Text: "alpha login succeeded"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000002", Text: "beta login failed"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000003", Text: "gamma logout"}},
	}
	newWordSrc := func(t *testing.T) *Source {
		t.Helper()
		conn := testDB(t)
		prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: loginMsgs})(t, conn)
		return &Source{conn: conn, canClose: true}
	}

	ftsTableExists := func(t *testing.T, conn *sqlx.DB) bool {
		t.Helper()
		var n int
		if err := conn.QueryRowxContext(t.Context(),
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='MESSAGE_FTS'`).Scan(&n); err != nil {
			t.Fatalf("sqlite_master query: %v", err)
		}
		return n > 0
	}

	t.Run("word mode finds a whole-token match", func(t *testing.T) {
		got, truncated, err := newWordSrc(t).SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("SearchMessages() error = %v", err)
		}
		if truncated {
			t.Error("truncated = true, want false")
		}
		if len(got) != 2 {
			t.Fatalf("got %d hits, want 2", len(got))
		}
		if got[0].Text != "beta login failed" || got[1].Text != "alpha login succeeded" {
			t.Errorf("hits = %q, %q; want newest first", got[0].Text, got[1].Text)
		}
	})

	t.Run("word mode and contains mode differ on a substring that is not a token", func(t *testing.T) {
		src := newWordSrc(t)
		contains, _, err := src.SearchMessages(t.Context(), "log", "", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("contains: SearchMessages() error = %v", err)
		}
		if len(contains) != 3 {
			t.Fatalf("contains: got %d hits, want 3 (login x2 + logout)", len(contains))
		}
		words, _, err := src.SearchMessages(t.Context(), "log", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("words: SearchMessages() error = %v", err)
		}
		if len(words) != 0 {
			t.Errorf("words: got %d hits, want 0 - \"log\" is nobody's whole token", len(words))
		}
	})

	t.Run("an unrecognised mode degrades to contains", func(t *testing.T) {
		src := newWordSrc(t)
		want, _, err := src.SearchMessages(t.Context(), "log", "", SearchModeContains, false, 10)
		if err != nil {
			t.Fatalf("contains: SearchMessages() error = %v", err)
		}
		got, _, err := src.SearchMessages(t.Context(), "log", "", "bogus-mode", false, 10)
		if err != nil {
			t.Fatalf("bogus mode: SearchMessages() error = %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("got %d hits, want %d (same as contains)", len(got), len(want))
		}
		for i := range want {
			if got[i].Text != want[i].Text {
				t.Errorf("hit %d = %q, want %q", i, got[i].Text, want[i].Text)
			}
		}
		if ftsTableExists(t, src.conn) {
			t.Error("MESSAGE_FTS exists after an unrecognised-mode search; it degrades to contains and must not index")
		}
	})

	t.Run("the index is not built until word mode is used", func(t *testing.T) {
		src := newWordSrc(t)
		if _, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeContains, false, 10); err != nil {
			t.Fatalf("contains: SearchMessages() error = %v", err)
		}
		if ftsTableExists(t, src.conn) {
			t.Error("MESSAGE_FTS exists after a contains-mode-only search")
		}
		if _, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10); err != nil {
			t.Fatalf("words: SearchMessages() error = %v", err)
		}
		if !ftsTableExists(t, src.conn) {
			t.Error("MESSAGE_FTS does not exist after a word-mode search")
		}
	})

	t.Run("EnsureFTS runs once per Source, not per query", func(t *testing.T) {
		src := newWordSrc(t)
		if _, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10); err != nil {
			t.Fatalf("first search: %v", err)
		}
		// A foreign write that goes through prepTestChunk, not through
		// SearchMessages, mimics a later dump appending to the same archive.
		// If EnsureFTS ran again on the second search it would notice MESSAGE
		// changed and rebuild, picking up this new message. It must not: the
		// build runs once per Source.
		prepTestChunk(&chunk.Chunk{
			Type:      chunk.CMessages,
			ChannelID: "C01",
			Messages: []slack.Message{
				{Msg: slack.Msg{Timestamp: "1234567890.000004", Text: "delta login again"}},
			},
		})(t, src.conn)

		got, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("second search: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d hits, want 2 (the index must still reflect only the first build)", len(got))
		}
		for _, m := range got {
			if m.Text == "delta login again" {
				t.Error("second search saw the foreign write; EnsureFTS ran again instead of once per Source")
			}
		}
	})

	t.Run("byRelevance is accepted in word mode and returns the same set as newest ordering", func(t *testing.T) {
		src := newWordSrc(t)
		newest, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("newest: SearchMessages() error = %v", err)
		}
		byRel, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, true, 10)
		if err != nil {
			t.Fatalf("byRelevance: SearchMessages() error = %v", err)
		}
		if len(byRel) != len(newest) {
			t.Fatalf("got %d hits, want %d", len(byRel), len(newest))
		}
		newestSet := make(map[string]bool, len(newest))
		for _, m := range newest {
			newestSet[m.Text] = true
		}
		for _, m := range byRel {
			if !newestSet[m.Text] {
				t.Errorf("byRelevance hit %q not present in the newest-ordering result set", m.Text)
			}
		}
	})

	// newReadOnlySrc returns a Source over an archive that genuinely cannot be
	// written to, which is the one way the index build legitimately fails:
	// creating the index is a write, and a read-only file refuses it.
	newReadOnlySrc := func(t *testing.T) *Source {
		t.Helper()
		path := filepath.Join(t.TempDir(), "archive.sqlite")
		conn := testDBDSN(t, path)
		prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: loginMsgs})(t, conn)
		// Close before reopening read-only so the WAL is checkpointed away:
		// a leftover WAL would need a write to replay and the failure under
		// test would be the wrong one.
		if err := conn.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		ro, err := sqlx.Open(repository.Driver, "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatalf("open read-only: %v", err)
		}
		t.Cleanup(func() { ro.Close() })
		if err := ro.PingContext(t.Context()); err != nil {
			t.Fatalf("ping read-only: %v", err)
		}
		return &Source{conn: ro, canClose: true}
	}

	t.Run("a cancelled first word-mode search does not disable word search", func(t *testing.T) {
		// The viewer searches on keyup and htmx aborts the in-flight request
		// when the next keystroke fires, so a cancelled first word-mode
		// search is the common case, not an exotic one.  Caching that failure
		// used to kill word mode for the life of the Source.
		src := newWordSrc(t)

		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, err := src.SearchMessages(cancelled, "login", "", SearchModeWords, false, 10); err == nil {
			t.Fatal("a search on a cancelled context should fail")
		}

		got, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("the next search on the same Source must succeed, got error = %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d hits, want 2", len(got))
		}
	})

	t.Run("a cancelled search reports cancellation, not an unavailable index", func(t *testing.T) {
		src := newWordSrc(t)
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()

		_, _, err := src.SearchMessages(cancelled, "login", "", SearchModeWords, false, 10)
		if err == nil {
			t.Fatal("a search on a cancelled context should fail")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled", err)
		}
		if errors.Is(err, ErrIndexUnavailable) {
			t.Errorf("err = %v, want it not to claim an unavailable index: "+
				"the archive is writable, the caller just went away", err)
		}
	})

	t.Run("a read-only archive keeps reporting an unavailable index", func(t *testing.T) {
		// Retrying the build is fine — it fails immediately on the first
		// write — but the error the caller sees must stay right on every call,
		// because that is what the viewer turns into its explanation.
		src := newReadOnlySrc(t)
		for i := range 2 {
			_, _, err := src.SearchMessages(t.Context(), "login", "", SearchModeWords, false, 10)
			if err == nil {
				t.Fatalf("call %d: word search on a read-only archive should fail", i)
			}
			if !errors.Is(err, ErrIndexUnavailable) {
				t.Errorf("call %d: err = %v, want it to wrap ErrIndexUnavailable", i, err)
			}
		}
	})

	// relevanceMsgs' bm25 order and recency order genuinely disagree: the
	// older message is two tokens long and both are the search term, the
	// newer mentions it once in some two hundred words of filler.  bm25
	// prefers the short dense document, recency the long sparse one.  The
	// loginMsgs fixture cannot tell the two orderings apart, so without this
	// pair a Source that dropped byRelevance on the floor would look fine.
	relevanceMsgs := []slack.Message{
		{Msg: slack.Msg{Timestamp: "1234567890.000001", Text: "needle needle"}},
		{Msg: slack.Msg{Timestamp: "1234567890.000002", Text: "needle " + strings.Repeat("filler ", 200)}},
	}
	newRelevanceSrc := func(t *testing.T) *Source {
		t.Helper()
		conn := testDB(t)
		prepTestChunk(&chunk.Chunk{Type: chunk.CMessages, ChannelID: "C01", Messages: relevanceMsgs})(t, conn)
		return &Source{conn: conn, canClose: true}
	}

	t.Run("byRelevance changes which hit comes first", func(t *testing.T) {
		src := newRelevanceSrc(t)
		short, long := relevanceMsgs[0].Text, relevanceMsgs[1].Text

		newest, _, err := src.SearchMessages(t.Context(), "needle", "", SearchModeWords, false, 10)
		if err != nil {
			t.Fatalf("newest: SearchMessages() error = %v", err)
		}
		byRel, _, err := src.SearchMessages(t.Context(), "needle", "", SearchModeWords, true, 10)
		if err != nil {
			t.Fatalf("byRelevance: SearchMessages() error = %v", err)
		}
		if len(newest) != 2 || len(byRel) != 2 {
			t.Fatalf("both orderings should return both messages, got %d and %d", len(newest), len(byRel))
		}
		if newest[0].Text != long {
			t.Errorf("newest-first hit = %.30q..., want the newer, longer message", newest[0].Text)
		}
		if byRel[0].Text != short {
			t.Errorf("byRelevance hit = %.30q..., want the older, denser message", byRel[0].Text)
		}
		if newest[0].Text == byRel[0].Text {
			t.Error("the two orderings agreed on the first hit; the fixture cannot tell a live byRelevance from an ignored one")
		}
	})
}
