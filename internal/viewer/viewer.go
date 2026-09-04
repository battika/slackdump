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

// Package viewer implements the logic to view the slackdump files.
package viewer

import (
	"cmp"
	"context"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rusq/slack"

	st "github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/internal/viewer/renderer"
	"github.com/rusq/slackdump/v4/source"
)

var debug = os.Getenv("DEBUG") != ""

func init() {
	if debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
}

// PageRenderer renders Viewer pages to any io.Writer.
// Both the live HTTP viewer and the static HTML exporter implement it.
type PageRenderer interface {
	RenderIndex(ctx context.Context, w io.Writer) error
	RenderChannel(ctx context.Context, channelID string, w io.Writer) error
	RenderThread(ctx context.Context, channelID, threadTS string, w io.Writer) error
	RenderUser(ctx context.Context, userID string, w io.Writer) error
	RenderCanvas(ctx context.Context, channelID string, w io.Writer) error
	RenderCanvasContent(ctx context.Context, channelID string, w io.Writer) error
}

// compile-time check that *Viewer implements PageRenderer.
var _ PageRenderer = (*Viewer)(nil)

// Viewer is the slackdump viewer.
type Viewer struct {
	// data
	ch   channels
	um   st.UserIndex
	src  source.Sourcer
	tmpl *template.Template
	mode renderer.Mode
	rts  *renderer.Routes
	// pageSize is the number of channel messages per page; 0 disables paging
	// and renders the whole timeline, which is the default and what the static
	// HTML converter relies on.
	pageSize int

	// handles
	srv *http.Server
	lg  *slog.Logger
	r   renderer.Renderer
}

type Option func(*viewerOptions)

type viewerOptions struct {
	mode     renderer.Mode
	pageSize int
}

func WithMode(mode renderer.Mode) Option {
	return func(o *viewerOptions) {
		o.mode = mode
	}
}

// WithPageSize sets the number of channel messages rendered per page.  Zero or
// less disables paging.  Paging is always disabled in [renderer.ModeStatic].
func WithPageSize(n int) Option {
	return func(o *viewerOptions) {
		o.pageSize = n
	}
}

const (
	hour = 60 * time.Minute
)

// New creates new viewer instance.  Once [Viewer.ListenAndServe] is called, the
// viewer will start serving the web interface on the given address.  The
// address should be in the form of ":8080". The viewer will use the given
// [Sourcer] to retrieve the data, see "source" package for available options.
// It will initialise the logger from the context.
func New(ctx context.Context, addr string, r source.Sourcer, opts ...Option) (*Viewer, error) {
	options := viewerOptions{mode: renderer.ModeLive}
	for _, opt := range opts {
		opt(&options)
	}
	// Static HTML output is never paged: convert -html emits one page per
	// channel and its routes have no query strings.
	if options.mode == renderer.ModeStatic {
		options.pageSize = 0
	}

	all, err := r.Channels(ctx)
	if err != nil {
		return nil, err
	}

	uu, err := r.Users(ctx)
	if err != nil {
		if errors.Is(err, source.ErrNotFound) {
			uu = []slack.User{}
		} else {
			return nil, err
		}
	}
	um := st.NewUserIndex(uu)

	// initChannels needs the user index to sort DMs and group messages by
	// their resolved display names, so it must run after NewUserIndex.
	cc := initChannels(all, um)

	v := &Viewer{
		src:      r,
		ch:       cc,
		um:       um,
		lg:       slog.Default(),
		mode:     options.mode,
		pageSize: options.pageSize,
	}
	rtOpts := []renderer.RouteOption{renderer.WithPaging(options.pageSize > 0)}
	if addr != "" {
		rtOpts = append(rtOpts, renderer.WithLiveHost(normalise(addr)))
	}
	if wi, err := r.WorkspaceInfo(ctx); err == nil {
		rtOpts = append(rtOpts, renderer.WithWorkspaceURL(wi.URL))
	}
	v.rts = renderer.NewRoutes(options.mode, rtOpts...)
	// postinit
	if debug {
		v.r = &renderer.Debug{}
	} else {
		opts := []renderer.SlackOption{
			renderer.WithUsers(indexusers(uu)),
			renderer.WithChannels(indexchannels(all)),
			renderer.WithRoutes(v.rts),
		}
		v.r = renderer.NewSlack(
			template.New("viewer-renderer"),
			opts...,
		)
	}
	initTemplates(v)

	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(StaticFS()))))
	mux.HandleFunc("GET /", v.indexHandler)
	// https: //ora600.slack.com/archives/CHY5HUESG
	mux.HandleFunc("GET /archives/{id}", v.newFileHandler(v.channelHandler))
	mux.HandleFunc("GET /archives/{id}/canvas", v.newFileHandler(v.canvasHandler))
	mux.HandleFunc("GET /archives/{id}/canvas/content", v.newFileHandler(v.canvasContentHandler))
	// https: //ora600.slack.com/archives/DHMAB25DY/p1710063528879959
	// https://ora600.slack.com/archives/CHY5HUESG/p1738580940349469?thread_ts=1737716342.919259&cid=CHY5HUESG
	mux.HandleFunc("GET /archives/{id}/alias/", v.aliasHandler)
	mux.HandleFunc("PUT /archives/{id}/alias/", v.aliasPutHandler)
	mux.HandleFunc("DELETE /archives/{id}/alias/", v.aliasDeleteHandler)
	mux.HandleFunc("GET /archives/{id}/{ts}", v.newFileHandler(v.postRedirectHandler))
	mux.HandleFunc("GET /team/{user_id}", v.userHandler)
	mux.HandleFunc("GET /search", v.searchHandler)
	mux.Handle("GET /slackdump/file/{id}/{filename}", cacheMwareFunc(3*hour)(http.HandlerFunc(v.fileHandler)))
	v.srv = &http.Server{
		Addr:    addr,
		Handler: middleware.Logger(mux),
	}

	return v, nil
}

func normalise(addr string) string {
	if addr == "" {
		return "127.0.0.1:8080"
	}
	if addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

func (v *Viewer) ListenAndServe() error {
	return v.srv.ListenAndServe()
}

func (v *Viewer) Close() error {
	var ee error
	if err := v.srv.Close(); err != nil {
		ee = errors.Join(err)
	}
	v.lg.Debug("server closed")
	if ee != nil {
		v.lg.Error("close", "errors", ee)
	}
	return ee
}

func indexusers(uu []slack.User) map[string]slack.User {
	um := make(map[string]slack.User, len(uu))
	for _, u := range uu {
		um[u.ID] = u
	}
	return um
}

func indexchannels(cc []slack.Channel) map[string]slack.Channel {
	cm := make(map[string]slack.Channel, len(cc))
	for _, c := range cc {
		cm[c.ID] = c
	}
	return cm
}

type channels struct {
	Public  []slack.Channel
	Private []slack.Channel
	MPIM    []slack.Channel
	DM      []slack.Channel
}

func (c channels) find(id string) (slack.Channel, bool) {
	for _, chset := range [][]slack.Channel{
		c.Public,
		c.Private,
		c.MPIM,
		c.DM,
	} {
		for _, ch := range chset {
			if ch.ID == id {
				return ch, true
			}
		}
	}
	return slack.Channel{}, false
}

// initChannels classifies channels into sidebar buckets and sorts each bucket
// by canonical display name, case-insensitively, so the sidebar is scannable.
// Sorting uses the canonical channel name rather than any user-defined alias:
// sorting by alias would require re-sorting on every request and would
// reshuffle the sidebar whenever an alias is edited.
//
// DM names resolve through um, so DM ordering degrades to raw user IDs for
// sources with no user index; see AGENTS.md invariant 13.
func initChannels(c []slack.Channel, um st.UserIndex) channels {
	var cc channels
	for _, ch := range c {
		t := st.ChannelType(ch)
		switch t {
		case st.CIM:
			cc.DM = append(cc.DM, ch)
		case st.CMPIM:
			cc.MPIM = append(cc.MPIM, ch)
		case st.CPrivate:
			cc.Private = append(cc.Private, ch)
		default:
			cc.Public = append(cc.Public, ch)
		}
	}
	byName := func(a, b slack.Channel) int {
		if n := cmp.Compare(strings.ToLower(um.ChannelName(a)), strings.ToLower(um.ChannelName(b))); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	}
	for _, s := range []*[]slack.Channel{&cc.Public, &cc.Private, &cc.MPIM, &cc.DM} {
		slices.SortStableFunc(*s, byName)
	}
	return cc
}

// channelGroup is one collapsible section of the sidebar channel list.
type channelGroup struct {
	Label string          // heading text, e.g. "Public channels"
	ID    string          // stable DOM id suffix, e.g. "public"
	Items []slack.Channel // channels, already in display order; aliases the viewer's channels slices — read-only, never sort or append in place
	Open  bool            // true when this group renders expanded
}

// groups returns the sidebar channel groups in display order, omitting empty
// ones.  The group containing activeID is marked Open; when activeID is empty
// or matches no channel, the first non-empty group is opened instead, so the
// sidebar is never rendered fully collapsed.
func (c channels) groups(activeID string) []channelGroup {
	gg := slices.DeleteFunc([]channelGroup{
		{Label: "Public channels", ID: "public", Items: c.Public},
		{Label: "Private channels", ID: "private", Items: c.Private},
		{Label: "Group messages", ID: "mpim", Items: c.MPIM},
		{Label: "Direct messages", ID: "dm", Items: c.DM},
	}, func(g channelGroup) bool { return len(g.Items) == 0 })
	if len(gg) == 0 {
		return gg
	}
	if activeID != "" {
		for i := range gg {
			if slices.ContainsFunc(gg[i].Items, func(ch slack.Channel) bool { return ch.ID == activeID }) {
				gg[i].Open = true
				return gg
			}
		}
	}
	gg[0].Open = true
	return gg
}
