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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rusq/slack"
)

// searchStub is an aliasSourceStub that also implements messageSearcher, so
// the type assertion in the gate succeeds.
type searchStub struct {
	*aliasSourceStub
	calls          int
	forceTruncated bool
}

func (s *searchStub) SearchMessages(_ context.Context, query, channelID string, limit int) ([]slack.Message, bool, error) {
	s.calls++
	var out []slack.Message
	for ch, mm := range s.msgs {
		if channelID != "" && ch != channelID {
			continue
		}
		for _, m := range mm {
			if strings.Contains(strings.ToLower(m.Text), strings.ToLower(query)) {
				m.Channel = ch
				out = append(out, m)
			}
		}
	}
	for ch, threads := range s.threads {
		if channelID != "" && ch != channelID {
			continue
		}
		for _, mm := range threads {
			for _, m := range mm {
				if strings.Contains(strings.ToLower(m.Text), strings.ToLower(query)) {
					m.Channel = ch
					out = append(out, m)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, s.forceTruncated, nil
}

func newSearchViewer(t *testing.T, n int) (*Viewer, *searchStub) {
	t.Helper()
	mm := make([]slack.Message, 0, n)
	for i := 1; i <= n; i++ {
		mm = append(mm, slack.Message{Msg: slack.Msg{
			Text:      "hello world " + strconv.Itoa(i),
			Timestamp: strconv.Itoa(1700000000+i) + ".000000",
		}})
	}
	src := newViewerRouteSource()
	src.msgs = map[string][]slack.Message{"C1": mm}
	stub := &searchStub{aliasSourceStub: src}
	v := newHandlerTestViewer(src)
	v.src = stub
	initTemplates(v)
	return v, stub
}

func TestViewer_canSearch(t *testing.T) {
	t.Run("database source can search", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		if !v.canSearch() {
			t.Error("canSearch() = false, want true for a source implementing messageSearcher")
		}
		if !v.view().CanSearch {
			t.Error("view().CanSearch = false, want true")
		}
	})

	t.Run("other sources cannot", func(t *testing.T) {
		v := newHandlerTestViewer(newViewerRouteSource())
		if v.canSearch() {
			t.Error("canSearch() = true, want false for a source without messageSearcher")
		}
		if v.view().CanSearch {
			t.Error("view().CanSearch = true, want false")
		}
	})
}

func TestSearchHandler_Gate(t *testing.T) {
	t.Run("404 when the source cannot search", func(t *testing.T) {
		v := newHandlerTestViewer(newViewerRouteSource())
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("searches when supported", func(t *testing.T) {
		v, stub := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if stub.calls != 1 {
			t.Errorf("SearchMessages called %d times, want 1", stub.calls)
		}
		if !strings.Contains(rr.Body.String(), "3") {
			t.Errorf("body should report 3 hits: %q", rr.Body.String())
		}
	})

	t.Run("short query does not hit the source", func(t *testing.T) {
		v, stub := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=h", nil)
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if stub.calls != 0 {
			t.Errorf("SearchMessages called %d times for a 1-char query, want 0", stub.calls)
		}
	})
}

func TestSearchBox_Gate(t *testing.T) {
	// The gate is the central design decision — database archives get search,
	// other formats get no search UI at all.  Without this test, dropping
	// .CanSearch from the template passes the whole suite.
	const marker = `id="search-input"`

	t.Run("rendered for a searchable source", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		var buf strings.Builder
		if err := v.RenderIndex(t.Context(), &buf); err != nil {
			t.Fatalf("RenderIndex() error = %v", err)
		}
		if !strings.Contains(buf.String(), marker) {
			t.Error("a database source should render the search box")
		}
	})

	t.Run("absent for a source that cannot search", func(t *testing.T) {
		v := newHandlerTestViewer(newViewerRouteSource())
		var buf strings.Builder
		if err := v.RenderIndex(t.Context(), &buf); err != nil {
			t.Fatalf("RenderIndex() error = %v", err)
		}
		if strings.Contains(buf.String(), marker) {
			t.Error("a non-database source must not render the search box")
		}
	})
}

func TestSearchHandler_Panel(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "prompt when the query is too short",
			query:       "h",
			wantContain: []string{"at least 2 characters"},
			wantAbsent:  []string{"hello world"},
		},
		{
			name:        "no results",
			query:       "zzzz",
			wantContain: []string{"No matches"},
		},
		{
			name:        "results list",
			query:       "hello",
			wantContain: []string{"hello world 3", "hello world 1", "3 matches"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := newSearchViewer(t, 3)
			req := httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape(tt.query), nil)
			req.Header.Set("HX-Request", "true")
			rr := httptest.NewRecorder()
			v.searchHandler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			body := rr.Body.String()
			for _, want := range tt.wantContain {
				if !strings.Contains(body, want) {
					t.Errorf("body should contain %q: %q", want, body)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body should not contain %q", absent)
				}
			}
		})
	}

	t.Run("truncation is reported", func(t *testing.T) {
		v, stub := newSearchViewer(t, 3)
		stub.forceTruncated = true
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if !strings.Contains(rr.Body.String(), "narrow your search") {
			t.Errorf("truncated result set should say so: %q", rr.Body.String())
		}
	})

	t.Run("a direct link to an activated hit renders it in the real conversation section", func(t *testing.T) {
		// hx-swap-oob is inert outside an htmx AJAX response, so on this path
		// the hit must be folded into the page itself.  Getting it wrong yields
		// two #conversation elements — a placeholder plus an inert copy nested
		// inside the results panel.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=1", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if n := strings.Count(body, `id="conversation"`); n != 1 {
			t.Errorf("found %d #conversation elements, want exactly 1", n)
		}
		if strings.Contains(body, "hx-swap-oob") {
			t.Error("the OOB fragment is meaningless on a full-page render and must not be emitted")
		}
		if strings.Contains(body, "Please select the conversation") {
			t.Error("the conversation section should hold the activated hit, not the placeholder")
		}
		if !strings.Contains(body, "search-hit") {
			t.Error("the activated message should be highlighted")
		}
	})

	t.Run("a direct link renders a full page with the panel open", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		body := rr.Body.String()
		if !strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("a direct link should render a full page: %q", body)
		}
		if !strings.Contains(body, "thread-open") {
			t.Errorf("the side panel must be open on a direct search link")
		}
		if !strings.Contains(body, "hello world 3") {
			t.Errorf("full page should contain the results")
		}
	})
}

func TestSearchHandler_Activation(t *testing.T) {
	t.Run("activating a hit swaps the conversation out of band", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=1", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, "hx-swap-oob") {
			t.Errorf("response should carry an OOB fragment: %q", body)
		}
		if !strings.Contains(body, `id="conversation"`) {
			t.Errorf("the OOB fragment should replace #conversation: %q", body)
		}
		if !strings.Contains(body, "search-hit-row active") {
			t.Errorf("the activated row should be marked active")
		}
	})

	t.Run("the activated message is highlighted in the conversation", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		// hits are newest-first, so i=1 is "hello world 3" at ts 1700000003.000000
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=1", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if !strings.Contains(rr.Body.String(), "search-hit") {
			t.Errorf("the hit message should carry the highlight class: %q", rr.Body.String())
		}
	})

	t.Run("out of range and junk indices do not activate", func(t *testing.T) {
		for _, i := range []string{"0", "99", "abc", "-1"} {
			v, _ := newSearchViewer(t, 3)
			req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i="+i, nil)
			req.Header.Set("HX-Request", "true")
			rr := httptest.NewRecorder()
			v.searchHandler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("i=%s status = %d, want 200", i, rr.Code)
			}
			if strings.Contains(rr.Body.String(), "hx-swap-oob") {
				t.Errorf("i=%s should not activate anything", i)
			}
		}
	})
}

func TestSearchHandler_ThreadHit(t *testing.T) {
	const parentTS = "1700000001.000000"
	const replyTS = "1700000002.000000"
	src := newViewerRouteSource()
	src.msgs = map[string][]slack.Message{"C1": {
		{Msg: slack.Msg{Text: "parent", Timestamp: parentTS, ThreadTimestamp: parentTS}},
	}}
	src.threads = map[string]map[string][]slack.Message{
		"C1": {parentTS: {
			{Msg: slack.Msg{Text: "parent", Timestamp: parentTS, ThreadTimestamp: parentTS}},
			{Msg: slack.Msg{Text: "hello from a reply", Timestamp: replyTS, ThreadTimestamp: parentTS}},
		}},
	}
	stub := &searchStub{aliasSourceStub: src}
	v := newHandlerTestViewer(src)
	v.src = stub
	initTemplates(v)

	req := httptest.NewRequest(http.MethodGet, "/search?q=reply&i=1", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	v.searchHandler(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "hello from a reply") {
		t.Errorf("a thread hit should render the thread: %q", body)
	}
	if !strings.Contains(body, "Back to") {
		t.Errorf("a thread rendered in the conversation pane needs a back link: %q", body)
	}
	// Assert on id="close-thread", which is unique to hx_thread's own close
	// button.  A body-wide check for data-close-panel would be wrong: the
	// search panel's header legitimately carries that attribute, and this
	// response contains both the panel and the OOB conversation fragment.
	if strings.Contains(body, `id="close-thread"`) {
		t.Errorf("the thread close button must not render in the conversation pane — it would close the search results")
	}

	// The same hit reached by a direct link takes a different path: the thread
	// is folded into the page rather than swapped out of band, so index.html
	// needs its own ThreadInMain branch or the conversation shows the
	// placeholder.
	t.Run("direct link renders the thread in the page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/search?q=reply&i=1", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		body := rr.Body.String()
		if !strings.Contains(body, "<!DOCTYPE html>") {
			t.Fatal("a direct link should render a full page")
		}
		if strings.Contains(body, "Please select the conversation") {
			t.Error("the conversation section should hold the thread, not the placeholder")
		}
		if !strings.Contains(body, "hello from a reply") {
			t.Error("the thread should be rendered in the page")
		}
		if !strings.Contains(body, "Back to") {
			t.Error("the in-page thread still needs its back link")
		}
	})
}

func TestSearchHandler_Stepping(t *testing.T) {
	t.Run("middle hit links both ways", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=2", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, "2 of 3") {
			t.Errorf("footer should report the position: %q", body)
		}
		if !strings.Contains(body, "i=1") || !strings.Contains(body, "i=3") {
			t.Errorf("footer should link to both neighbours: %q", body)
		}
	})

	t.Run("first hit has no previous", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=1", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		body := rr.Body.String()
		if !strings.Contains(body, "1 of 3") {
			t.Errorf("footer should report 1 of 3: %q", body)
		}
		if !strings.Contains(body, `rel="next"`) {
			t.Errorf("first hit should still link forward")
		}
		if strings.Contains(body, `rel="prev"`) {
			t.Errorf("first hit must not link backward")
		}
	})

	t.Run("last hit has no next", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=3", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		body := rr.Body.String()
		if !strings.Contains(body, `rel="prev"`) {
			t.Errorf("last hit should still link backward")
		}
		if strings.Contains(body, `rel="next"`) {
			t.Errorf("last hit must not link forward")
		}
	})

	t.Run("no footer when nothing is activated", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)
		if strings.Contains(rr.Body.String(), "of 3") {
			t.Errorf("the position footer should only appear once a hit is active")
		}
	})
}
