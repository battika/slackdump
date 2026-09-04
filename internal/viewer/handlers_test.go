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

package viewer

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rusq/slack"

	st "github.com/rusq/slackdump/v4/internal/structures"
	"github.com/rusq/slackdump/v4/internal/viewer/renderer"
	"github.com/rusq/slackdump/v4/source"
)

func Test_isInvalid(t *testing.T) {
	type args struct {
		path string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{"relative path", args{"../test.txt"}, true},
		{"home dir ref", args{"~/test.txt"}, true},
		{"filename with tilda #561", args{"test~1.txt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInvalid(tt.args.path); got != tt.want {
				t.Errorf("isInvalid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStaticHandler_ServesEmbeddedAssets(t *testing.T) {
	src := newViewerRouteSource()
	src.wi = &slack.AuthTestResponse{}

	v, err := New(context.Background(), "", src)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/static/htmx.min.js", want: "var htmx="},
		{path: "/static/viewer.js", want: "function syncActiveChannel"},
		{path: "/static/viewer.js", want: "function expandGroup"},
		// The side panel is display:none until .container gets thread-open,
		// and the search box is an <input> firing on keyup, so without this
		// listener search results swap into a hidden panel.
		{path: "/static/viewer.js", want: `htmx:afterSwap`},
		// Search hit stepping and scrolling the activated message into view.
		{path: "/static/viewer.js", want: "onSearchKeydown"},
		{path: "/static/viewer.js", want: "scrollHitIntoView"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()

			v.srv.Handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("static handler status = %d, want %d", rr.Code, http.StatusOK)
			}
			if rr.Body.Len() == 0 {
				t.Fatal("static handler returned empty body")
			}
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Fatalf("static handler should serve %s, got: %q", tc.path, rr.Body.String())
			}
		})
	}
}

func TestUserHandler_RendersFullPageWithoutHTMX(t *testing.T) {
	v := &Viewer{
		src: &aliasSourceStub{},
		um:  st.UserIndex{"U1": &slack.User{ID: "U1", Profile: slack.UserProfile{RealName: "Ada"}}},
		lg:  slog.Default(),
		r:   &renderer.Debug{},
		rts: renderer.NewRoutes(renderer.ModeLive),
	}
	initTemplates(v)

	req := httptest.NewRequest(http.MethodGet, "/team/U1", nil)
	req.SetPathValue("user_id", "U1")
	rr := httptest.NewRecorder()

	v.userHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("userHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("userHandler() response should include full page HTML: %q", body)
	}
	if !strings.Contains(body, "Profile") {
		t.Fatalf("userHandler() response should include profile panel: %q", body)
	}
}

func TestUserHandler_RendersHTMXUserPanel(t *testing.T) {
	v := &Viewer{
		src: &aliasSourceStub{},
		um: st.UserIndex{"U1": &slack.User{ID: "U1", Profile: slack.UserProfile{
			RealName: "Ada Lovelace",
			Image512: "https://example.com/avatar.png",
		}}},
		lg:  slog.Default(),
		r:   &renderer.Debug{},
		rts: renderer.NewRoutes(renderer.ModeLive),
	}
	initTemplates(v)

	req := httptest.NewRequest(http.MethodGet, "/team/U1", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("user_id", "U1")
	rr := httptest.NewRecorder()

	v.userHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("userHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("userHandler() HTMX response should not include full page HTML: %q", body)
	}
	if !strings.Contains(body, "Ada Lovelace") {
		t.Fatalf("userHandler() HTMX response should include user details: %q", body)
	}
	if !strings.Contains(body, `<button type="button" id="close-user"`) || !strings.Contains(body, `aria-label="Close profile panel"`) {
		t.Fatalf("userHandler() HTMX response should include accessible close button: %q", body)
	}
	profileTitle := strings.Index(body, `<h3>Profile</h3>`)
	closeUser := strings.Index(body, `<button type="button" id="close-user"`)
	if profileTitle < 0 || closeUser < profileTitle {
		t.Fatalf("userHandler() HTMX response should render close button after profile title: %q", body)
	}
	if strings.Contains(body, "Unknown") {
		t.Fatalf("userHandler() HTMX response should not render unknown user: %q", body)
	}
}

func newHandlerTestViewer(src *aliasSourceStub) *Viewer {
	v := &Viewer{
		src: src,
		ch:  initChannels(src.chs, st.NewUserIndex(src.users)),
		um:  st.NewUserIndex(src.users),
		lg:  slog.Default(),
		r:   &renderer.Debug{},
		rts: renderer.NewRoutes(renderer.ModeLive),
	}
	initTemplates(v)
	return v
}

func TestChannelHandler_RendersFullPageWithoutHTMX(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1", nil)
	rr := httptest.NewRecorder()

	v.channelHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("channelHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("channelHandler() should render full page HTML: %q", body)
	}
	if !strings.Contains(body, `hx-get="/archives/C1"`) {
		t.Fatalf("channelHandler() full page should preserve live HTMX routes: %q", body)
	}
	if !strings.Contains(body, "thread root") {
		t.Fatalf("channelHandler() should render channel messages: %q", body)
	}
}

func TestChannelHandler_RendersHTMXPartial(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	v.channelHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("channelHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("channelHandler() HTMX response should not include full page HTML: %q", body)
	}
	if !strings.Contains(body, `id="tab-panel-conversation"`) {
		t.Fatalf("channelHandler() HTMX response should include conversation panel: %q", body)
	}
	if !strings.Contains(body, "thread root") {
		t.Fatalf("channelHandler() HTMX response should include message content: %q", body)
	}
}

func TestThreadHandler_RendersFullPageWithoutHTMX(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/1710000000.000001", nil)
	req.SetPathValue("ts", "1710000000.000001")
	rr := httptest.NewRecorder()

	v.threadHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("threadHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("threadHandler() should render full page HTML: %q", body)
	}
	if !strings.Contains(body, "Link to this thread") {
		t.Fatalf("threadHandler() should include thread panel: %q", body)
	}
	if !strings.Contains(body, `id="channel-link-C1"`) || !strings.Contains(body, `id="tab-panel-conversation"`) {
		t.Fatalf("threadHandler() full page should include sidebar and conversation content: %q", body)
	}
	if !strings.Contains(body, "thread root") || !strings.Contains(body, "reply body") {
		t.Fatalf("threadHandler() should render thread messages: %q", body)
	}
}

func TestThreadHandler_RendersHTMXPartial(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/1710000000.000001", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("ts", "1710000000.000001")
	rr := httptest.NewRecorder()

	v.threadHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("threadHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("threadHandler() HTMX response should not include full page HTML: %q", body)
	}
	if !strings.Contains(body, "Link to this thread") {
		t.Fatalf("threadHandler() HTMX response should include thread body: %q", body)
	}
	if !strings.Contains(body, `<button type="button" id="close-thread"`) || !strings.Contains(body, `aria-label="Close thread panel"`) {
		t.Fatalf("threadHandler() HTMX response should include accessible close button: %q", body)
	}
	header := strings.Index(body, `<header class="thread-header">`)
	closeButton := strings.Index(body, `<button type="button" id="close-thread"`)
	title := strings.Index(body, `<h2>Thread: 1710000000.000001</h2>`)
	if header < 0 || title < header || closeButton < title {
		t.Fatalf("threadHandler() HTMX response should render close button after thread title: %q", body)
	}
}

func TestCanvasHandler_RendersFullPageWithoutHTMX(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/canvas", nil)
	rr := httptest.NewRecorder()

	v.canvasHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("canvasHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("canvasHandler() should render full page HTML: %q", body)
	}
	if !strings.Contains(body, `src="/archives/C1/canvas/content"`) {
		t.Fatalf("canvasHandler() should include canvas iframe: %q", body)
	}
	if !strings.Contains(body, `id="channel-link-C1"`) || !strings.Contains(body, `id="tab-panel-canvas"`) {
		t.Fatalf("canvasHandler() full page should include sidebar and canvas content: %q", body)
	}
}

func TestCanvasHandler_RendersHTMXPartial(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/canvas", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()

	v.canvasHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("canvasHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("canvasHandler() HTMX response should not include full page HTML: %q", body)
	}
	if !strings.Contains(body, `id="tab-panel-canvas"`) {
		t.Fatalf("canvasHandler() HTMX response should include canvas panel: %q", body)
	}
	if !strings.Contains(body, `sandbox="allow-same-origin"`) {
		t.Fatalf("canvasHandler() HTMX response should preserve iframe sandbox: %q", body)
	}
}

func TestCanvasContentHandler_ServesCanvasHTML(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/canvas/content", nil)
	rr := httptest.NewRecorder()

	v.canvasContentHandler(rr, req, "C1")

	if rr.Code != http.StatusOK {
		t.Fatalf("canvasContentHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("canvasContentHandler() content type = %q", got)
	}
	if !strings.Contains(rr.Body.String(), "canvas body") {
		t.Fatalf("canvasContentHandler() should stream canvas HTML: %q", rr.Body.String())
	}
}

func TestCanvasContentHandler_DegradesGracefullyWithoutFileByID(t *testing.T) {
	src := newViewerRouteSource()
	src.files = storageStub{
		fsys:      fstest.MapFS{},
		byName:    map[string]string{},
		byID:      map[string]string{},
		allowByID: false,
	}
	v := newHandlerTestViewer(src)
	req := httptest.NewRequest(http.MethodGet, "/archives/C1/canvas/content", nil)
	rr := httptest.NewRecorder()

	v.canvasContentHandler(rr, req, "C1")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("canvasContentHandler() status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestFileHandler_ServesDownloadedFile(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/slackdump/file/F1/hello.txt", nil)
	req.SetPathValue("id", "F1")
	req.SetPathValue("filename", "hello.txt")
	rr := httptest.NewRecorder()

	v.fileHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("fileHandler() status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("fileHandler() content type = %q", got)
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("fileHandler() body = %q, want %q", rr.Body.String(), "hello")
	}
}

func TestFileHandler_RejectsInvalidPath(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/slackdump/file/F1/../hello.txt", nil)
	req.SetPathValue("id", "F1")
	req.SetPathValue("filename", "../hello.txt")
	rr := httptest.NewRecorder()

	v.fileHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("fileHandler() status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestFileHandler_ReturnsNotFoundForMissingFile(t *testing.T) {
	v := newHandlerTestViewer(newViewerRouteSource())
	req := httptest.NewRequest(http.MethodGet, "/slackdump/file/F1/missing.txt", nil)
	req.SetPathValue("id", "F1")
	req.SetPathValue("filename", "missing.txt")
	rr := httptest.NewRecorder()

	v.fileHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("fileHandler() status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestChannelHandler_Paging(t *testing.T) {
	newPagedViewer := func(t *testing.T, n, size int) *Viewer {
		t.Helper()
		mm := make([]slack.Message, 0, n)
		for i := 1; i <= n; i++ {
			mm = append(mm, slack.Message{Msg: slack.Msg{
				Text:      "msg" + strconv.Itoa(i),
				Timestamp: strconv.Itoa(100+i) + ".000000",
				User:      "U1",
			}})
		}
		src := newViewerRouteSource()
		src.msgs = map[string][]slack.Message{"C1": mm}
		v := newHandlerTestViewer(src)
		v.pageSize = size
		v.rts = renderer.NewRoutes(renderer.ModeLive, renderer.WithPaging(size > 0))
		initTemplates(v)
		return v
	}

	t.Run("defaults to the last page", func(t *testing.T) {
		v := newPagedViewer(t, 5, 2)
		req := httptest.NewRequest(http.MethodGet, "/archives/C1", nil)
		rr := httptest.NewRecorder()

		v.channelHandler(rr, req, "C1")

		body := rr.Body.String()
		if !strings.Contains(body, "msg5") {
			t.Errorf("last page should contain msg5: %q", body)
		}
		if strings.Contains(body, "msg1") {
			t.Errorf("last page should not contain msg1")
		}
		if !strings.Contains(body, "Page 3 of 3") {
			t.Errorf("body should contain the page indicator: %q", body)
		}
	})

	t.Run("explicit page", func(t *testing.T) {
		v := newPagedViewer(t, 5, 2)
		req := httptest.NewRequest(http.MethodGet, "/archives/C1?p=1", nil)
		rr := httptest.NewRecorder()

		v.channelHandler(rr, req, "C1")

		body := rr.Body.String()
		if !strings.Contains(body, "msg1") || !strings.Contains(body, "msg2") {
			t.Errorf("page 1 should contain msg1 and msg2: %q", body)
		}
		if strings.Contains(body, "msg5") {
			t.Errorf("page 1 should not contain msg5")
		}
	})

	t.Run("out of range and junk page numbers clamp to the last page", func(t *testing.T) {
		for _, p := range []string{"0", "999", "abc", "-2"} {
			v := newPagedViewer(t, 5, 2)
			req := httptest.NewRequest(http.MethodGet, "/archives/C1?p="+p, nil)
			rr := httptest.NewRecorder()

			v.channelHandler(rr, req, "C1")

			if rr.Code != http.StatusOK {
				t.Fatalf("p=%s status = %d, want 200", p, rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "msg5") {
				t.Errorf("p=%s should clamp to the last page", p)
			}
		}
	})

	t.Run("htmx partial carries the paging footer", func(t *testing.T) {
		v := newPagedViewer(t, 5, 2)
		req := httptest.NewRequest(http.MethodGet, "/archives/C1?p=2", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()

		v.channelHandler(rr, req, "C1")

		body := rr.Body.String()
		if strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("HTMX request should return a partial")
		}
		if !strings.Contains(body, "Page 2 of 3") {
			t.Errorf("partial should contain the page indicator: %q", body)
		}
	})

	t.Run("paging disabled renders everything and no footer", func(t *testing.T) {
		v := newPagedViewer(t, 5, 0)
		req := httptest.NewRequest(http.MethodGet, "/archives/C1", nil)
		rr := httptest.NewRecorder()

		v.channelHandler(rr, req, "C1")

		body := rr.Body.String()
		if !strings.Contains(body, "msg1") || !strings.Contains(body, "msg5") {
			t.Errorf("unpaged view should contain every message")
		}
		// Assert on the nav's aria-label, not on the "paging-nav" class name:
		// the class appears in the inlined stylesheet on every full page.
		if strings.Contains(body, `aria-label="Message pages"`) {
			t.Errorf("unpaged view should not render the paging footer")
		}
	})
}

func TestRenderCanvasContent_MissingCanvasReturnsNotExist(t *testing.T) {
	v := newHandlerTestViewer(&aliasSourceStub{
		chs: []slack.Channel{{
			GroupConversation: slack.GroupConversation{
				Name:         "general",
				Conversation: slack.Conversation{ID: "C1"},
			},
			IsChannel: true,
		}},
		files: source.NoStorage{},
	})

	err := v.RenderCanvasContent(t.Context(), "C1", httptest.NewRecorder())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("RenderCanvasContent() error = %v, want fs.ErrNotExist", err)
	}
}

func TestRenderThread_ShowsPageOfParent(t *testing.T) {
	mm := make([]slack.Message, 0, 5)
	for i := 1; i <= 5; i++ {
		mm = append(mm, slack.Message{Msg: slack.Msg{
			Text:      "msg" + strconv.Itoa(i),
			Timestamp: strconv.Itoa(1700000000+i) + ".000000",
		}})
	}
	// The parent sits at ordinal 2, i.e. the middle page.  A parent on page 1
	// would not discriminate: a messagePageOf that ignored ts and always
	// returned 1 would still satisfy every assertion below.
	const parentTS = "1700000003.000000"
	src := newViewerRouteSource()
	src.msgs = map[string][]slack.Message{"C1": mm}
	src.threads = map[string]map[string][]slack.Message{
		"C1": {parentTS: {{Msg: slack.Msg{Text: "parent", Timestamp: parentTS, ThreadTimestamp: parentTS}}}},
	}
	v := newHandlerTestViewer(src)
	v.pageSize = 2
	v.rts = renderer.NewRoutes(renderer.ModeLive, renderer.WithPaging(true))
	initTemplates(v)

	var buf strings.Builder
	if err := v.RenderThread(t.Context(), "C1", parentTS, &buf); err != nil {
		t.Fatalf("RenderThread() error = %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "msg3") || !strings.Contains(body, "msg4") {
		t.Errorf("background timeline should show the parent's page: %q", body)
	}
	if strings.Contains(body, "msg1") {
		t.Errorf("background timeline should not show the first page")
	}
	if strings.Contains(body, "msg5") {
		t.Errorf("background timeline should not show the last page")
	}
	if !strings.Contains(body, "Page 2 of 3") {
		t.Errorf("background timeline should report page 2 of 3: %q", body)
	}
}

func TestPostRedirectHandler_ResolvesPage(t *testing.T) {
	// Timestamps use a realistic 10-digit epoch base rather than "100+i": the
	// p-link form flows through structures.ThreadIDtoTS, which slices
	// threadID[1:11]/threadID[11:] on the assumption of a 10-digit seconds
	// field followed by a 6-digit microsecond field. Shorter synthetic
	// timestamps panic there on an unrelated, pre-existing bug that is out of
	// scope for this task.
	const base = 1700000000
	mm := make([]slack.Message, 0, 5)
	for i := 1; i <= 5; i++ {
		mm = append(mm, slack.Message{Msg: slack.Msg{
			Text:      "msg" + strconv.Itoa(i),
			Timestamp: strconv.Itoa(base+i) + ".000000",
		}})
	}
	src := newViewerRouteSource()
	src.msgs = map[string][]slack.Message{"C1": mm}

	tests := []struct {
		name    string
		size    int
		ts      string
		wantLoc string
	}{
		{"first message lands on page 1", 2, "p" + strconv.Itoa(base+1) + "000000", "/archives/C1?p=1#1700000001.000000"},
		{"third message lands on page 2", 2, "p" + strconv.Itoa(base+3) + "000000", "/archives/C1?p=2#1700000003.000000"},
		{"last message lands on the last page", 2, "p" + strconv.Itoa(base+5) + "000000", "/archives/C1?p=3#1700000005.000000"},
		{"unpaged keeps the plain anchor", 0, "p" + strconv.Itoa(base+3) + "000000", "/archives/C1#1700000003.000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newHandlerTestViewer(src)
			v.pageSize = tt.size
			v.rts = renderer.NewRoutes(renderer.ModeLive, renderer.WithPaging(tt.size > 0))
			initTemplates(v)

			req := httptest.NewRequest(http.MethodGet, "/archives/C1/"+tt.ts, nil)
			req.SetPathValue("ts", tt.ts)
			rr := httptest.NewRecorder()

			v.postRedirectHandler(rr, req, "C1")

			if rr.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusSeeOther)
			}
			if got := rr.Header().Get("Location"); got != tt.wantLoc {
				t.Errorf("Location = %q, want %q", got, tt.wantLoc)
			}
		})
	}
}
