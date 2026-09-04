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

package renderer

import (
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/source"
)

// Mode defines how routes are rendered.
type Mode uint8

const (
	ModeLive Mode = iota
	ModeStatic
)

// Routes generates links for either live viewer or static HTML output.
type Routes struct {
	mode          Mode
	workspaceHost string
	liveHost      string
	// paged is true when the live viewer serves channels in pages.  It changes
	// ChannelMessage to emit the self-resolving p-form, because the page a
	// message lives on is only known server-side.
	paged bool
}

type RouteOption func(*Routes)

func WithWorkspaceURL(wspURL string) RouteOption {
	return func(r *Routes) {
		u, err := url.Parse(wspURL)
		if err != nil {
			slog.Warn("error parsing workspace URL", "error", err)
			return
		}
		r.workspaceHost = u.Hostname()
	}
}

func WithLiveHost(host string) RouteOption {
	return func(r *Routes) {
		r.liveHost = host
	}
}

// WithPaging tells the route generator that channel timelines are served in
// pages.  It has no effect in [ModeStatic], where paging is always off.
func WithPaging(paged bool) RouteOption {
	return func(r *Routes) {
		r.paged = paged
	}
}

func NewRoutes(mode Mode, opts ...RouteOption) *Routes {
	r := &Routes{mode: mode}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Routes) Interactive() bool {
	return r != nil && r.mode == ModeLive
}

func (r *Routes) Channel(id string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("archives", id, "index.html")
	}
	return routePath("archives", id)
}

// ChannelMessage returns the link used by templates to point at a single
// message.  With paging enabled the page containing the message is not known
// at render time, so it returns the "p" permalink form, which
// postRedirectHandler resolves to the correct page.
func (r *Routes) ChannelMessage(id, ts string) string {
	if r != nil && r.paged && r.mode == ModeLive {
		if tid := structures.TStoThreadID(ts); tid != "" {
			return routePath("archives", id, tid)
		}
	}
	return withFragment(r.Channel(id), ts)
}

// ChannelPage returns the link to a specific page of a channel.  A page of 0
// or less, or paging being disabled, yields the plain channel link.
func (r *Routes) ChannelPage(id string, page int) string {
	if r == nil || !r.paged || r.mode == ModeStatic || page < 1 {
		return r.Channel(id)
	}
	return fmt.Sprintf("%s?p=%d", r.Channel(id), page)
}

// ChannelPageMessage returns the terminal link to a message on a known page.
// Handlers must use this rather than [Routes.ChannelMessage], which would
// redirect back into the resolving handler and loop.
func (r *Routes) ChannelPageMessage(id string, page int, ts string) string {
	return withFragment(r.ChannelPage(id, page), ts)
}

func (r *Routes) Thread(id, ts string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("archives", id, "threads", ts+".html")
	}
	return routePath("archives", id, ts)
}

func (r *Routes) ThreadMessage(id, threadTS, msgTS string) string {
	return withFragment(r.Thread(id, threadTS), msgTS)
}

func (r *Routes) User(userID string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("team", userID, "index.html")
	}
	return routePath("team", userID)
}

func (r *Routes) Canvas(id string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("archives", id, "canvas", "index.html")
	}
	return routePath("archives", id, "canvas")
}

func (r *Routes) CanvasContent(id string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("archives", id, "canvas", "content.html")
	}
	return routePath("archives", id, "canvas", "content")
}

func (r *Routes) File(id, filename string) string {
	if r != nil && r.mode == ModeStatic {
		return routePath("files", id, source.SanitizeFilename(filename))
	}
	return routePath("slackdump", "file", id, filename)
}

// SearchHit returns the URL that activates hit i of a search.  It is the only
// search URL the viewer emits: result rows and the prev/next controls all use
// it, so a row click and a hit step are the same request.  A non-positive i
// means "no hit activated".  Static output has no search, so it returns "".
func (r *Routes) SearchHit(query, channelID string, i int) string {
	if r == nil || r.mode == ModeStatic {
		return ""
	}
	vals := url.Values{}
	vals.Set("q", query)
	if channelID != "" {
		vals.Set("ch", channelID)
	}
	if i > 0 {
		vals.Set("i", strconv.Itoa(i))
	}
	return "/search?" + vals.Encode()
}

func (r *Routes) StaticAsset(name string) string {
	return routePath("static", name)
}

func (r *Routes) Avatar(userID, filename string) string {
	return routePath("avatars", userID, filename)
}

func (r *Routes) RewriteSlackURL(src string) string {
	if r == nil || r.workspaceHost == "" {
		return src
	}
	u, err := url.Parse(src)
	if err != nil {
		slog.Warn("error parsing url", "url", src, "error", err)
		return src
	}
	if u.Hostname() != r.workspaceHost {
		return src
	}

	parts := splitPath(u.Path)
	if len(parts) >= 2 {
		switch parts[0] {
		case "archives":
			channelID := parts[1]
			switch {
			case len(parts) == 2:
				return r.Channel(channelID)
			case len(parts) == 3 && parts[2] == "canvas":
				return r.Canvas(channelID)
			case len(parts) == 4 && parts[2] == "canvas" && parts[3] == "content":
				return r.CanvasContent(channelID)
			case len(parts) >= 3:
				ts := parts[2]
				if strings.HasPrefix(ts, "p") {
					if threadTS := u.Query().Get("thread_ts"); threadTS != "" {
						return r.ThreadMessage(channelID, threadTS, structures.ThreadIDtoTS(ts))
					}
					return r.ChannelMessage(channelID, structures.ThreadIDtoTS(ts))
				}
				return r.Thread(channelID, ts)
			}
		case "team":
			return r.User(parts[1])
		}
	}

	if r.mode == ModeLive && r.liveHost != "" {
		u.Host = r.liveHost
		u.Scheme = "http"
		return u.String()
	}
	return src
}

func routePath(parts ...string) string {
	escaped := make([]string, 0, len(parts)+1)
	escaped = append(escaped, "")
	for _, part := range parts {
		if part == "" {
			continue
		}
		escaped = append(escaped, url.PathEscape(part))
	}
	return strings.Join(escaped, "/")
}

func withFragment(raw, fragment string) string {
	if fragment == "" {
		return raw
	}
	return fmt.Sprintf("%s#%s", raw, url.PathEscape(fragment))
}

func splitPath(p string) []string {
	p = strings.Trim(path.Clean(p), "/")
	if p == "." || p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
