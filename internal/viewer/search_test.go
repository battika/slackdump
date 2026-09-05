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
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase"
)

// searchStub is an aliasSourceStub that also implements messageSearcher, so
// the type assertion in the gate succeeds.
type searchStub struct {
	*aliasSourceStub
	calls          int
	forceTruncated bool
	// mode and byRelevance record what the handler asked for.  The stub
	// matches by substring whatever the mode, so recording them is the only
	// evidence that ?m= and ?sort= reached the source at all — asserting on
	// the view model alone would pass even if the call still hardcoded a mode.
	mode        string
	byRelevance bool
	err         error // returned instead of matches when set
}

func (s *searchStub) SearchMessages(_ context.Context, query, channelID, mode string, byRelevance bool, limit int) ([]slack.Message, bool, error) {
	s.calls++
	s.mode = mode
	s.byRelevance = byRelevance
	if s.err != nil {
		return nil, false, s.err
	}
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

	t.Run("an empty word search offers a prefix retry and a substring scan", func(t *testing.T) {
		// Word mode is the default and matches whole tokens, so on an inflected
		// language a stem legitimately finds nothing that a substring scan
		// would find.  Saying only "No matches" leaves the user stuck in the
		// mode that cannot answer them.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=zzzz", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		for _, want := range []string{"/search?q=zzzz%2A", "/search?m=contains&q=zzzz"} {
			if !hasSearchURL(t, body, want) {
				t.Errorf("an empty word result should offer %q: %q", want, body)
			}
		}
		// The escapes have to behave like the result rows: an href alone would
		// reload the whole page instead of swapping the panel.
		if !strings.Contains(body, `hx-get="/search?q=zzzz%2A"`) {
			t.Errorf("the prefix escape should be wired for htmx: %q", body)
		}
	})

	t.Run("no prefix escape when the query already ends in a star", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q="+url.QueryEscape("zzzz*"), nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if hasSearchURL(t, body, "/search?q=zzzz%2A%2A") {
			t.Errorf("suggesting a second star is noise: %q", body)
		}
		if want := "/search?m=contains&q=zzzz%2A"; !hasSearchURL(t, body, want) {
			t.Errorf("the substring escape still applies, want %q: %q", want, body)
		}
	})

	t.Run("contains mode offers no escapes on an empty result", func(t *testing.T) {
		// There is nowhere further to go: contains already matches inside words.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=zzzz&m=contains", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, "No matches") {
			t.Errorf("an empty contains result is just empty: %q", body)
		}
		if urls := searchURLs(t, body); len(urls) != 0 {
			t.Errorf("contains mode should offer no escape links, got %q", urls)
		}
	})

	t.Run("a too-short query offers no escapes", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=z", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if urls := searchURLs(t, body); len(urls) != 0 {
			t.Errorf("nothing was searched yet, so nothing to escape from, got %q", urls)
		}
	})

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
			// The controls refresh is out of band on every htmx response, so
			// the assertion has to name the conversation fragment rather than
			// hx-swap-oob at large.
			if strings.Contains(rr.Body.String(), `id="conversation"`) {
				t.Errorf("i=%s should not swap the conversation", i)
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

// searchURLs returns every /search link in a rendered panel: the result rows
// and the Prev/Next controls.  All of them come from Routes.SearchHit, so a
// mode dropped anywhere along that path shows up here.
func searchURLs(t *testing.T, body string) []string {
	t.Helper()
	re := regexp.MustCompile(`href="(/search\?[^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// hasSearchURL reports whether the panel emits exactly the given /search link.
func hasSearchURL(t *testing.T, body, want string) bool {
	t.Helper()
	return slices.Contains(searchURLs(t, body), want)
}

func Test_requestedMode(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"absent defaults to words", "/search?q=hello", searchModeWords},
		{"explicit words", "/search?q=hello&m=words", searchModeWords},
		{"explicit contains", "/search?q=hello&m=contains", searchModeContains},
		{"empty falls back to words", "/search?q=hello&m=", searchModeWords},
		{"junk falls back to words", "/search?q=hello&m=garbage", searchModeWords},
		{"the match is exact", "/search?q=hello&m=Contains", searchModeWords},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if got := requestedMode(req); got != tt.want {
				t.Errorf("requestedMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_requestedRelevance(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"absent means newest first", "/search?q=hello", false},
		{"relevance", "/search?q=hello&sort=relevance", true},
		{"explicit newest", "/search?q=hello&sort=newest", false},
		{"junk falls back to newest", "/search?q=hello&sort=garbage", false},
		{"the match is exact", "/search?q=hello&sort=Relevance", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if got := requestedRelevance(req); got != tt.want {
				t.Errorf("requestedRelevance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchHandler_Mode(t *testing.T) {
	// Every assertion here is either on what the searcher received or on what
	// the emitted URLs contain.  Checking only that searchView has the field
	// would pass against a handler that still hardcoded the mode.
	modeReaches := []struct {
		name            string
		url             string
		wantMode        string
		wantByRelevance bool
	}{
		{"words and newest first are the defaults", "/search?q=hello", searchModeWords, false},
		{"m=contains selects the substring scan", "/search?q=hello&m=contains", searchModeContains, false},
		{"junk mode degrades to words", "/search?q=hello&m=garbage", searchModeWords, false},
		{"sort=relevance asks for bm25 ordering", "/search?q=hello&sort=relevance", searchModeWords, true},
		{"junk sort degrades to newest first", "/search?q=hello&sort=garbage", searchModeWords, false},
		{"both at once", "/search?q=hello&m=contains&sort=relevance", searchModeContains, true},
	}
	for _, tt := range modeReaches {
		t.Run(tt.name, func(t *testing.T) {
			v, stub := newSearchViewer(t, 3)
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Header.Set("HX-Request", "true")
			rr := httptest.NewRecorder()
			v.searchHandler(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if stub.calls != 1 {
				t.Fatalf("SearchMessages called %d times, want 1", stub.calls)
			}
			if stub.mode != tt.wantMode {
				t.Errorf("searcher received mode %q, want %q", stub.mode, tt.wantMode)
			}
			if stub.byRelevance != tt.wantByRelevance {
				t.Errorf("searcher received byRelevance %v, want %v", stub.byRelevance, tt.wantByRelevance)
			}
		})
	}

	t.Run("mode and sort survive hit activation", func(t *testing.T) {
		// SearchHit is the only search URL the viewer emits.  If a step link
		// dropped the mode, moving from hit 2 to hit 3 would re-run the query
		// in a different mode and land on a different message.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&m=contains&sort=relevance&i=2", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		urls := searchURLs(t, body)
		if len(urls) != 5 { // three result rows plus Prev and Next
			t.Fatalf("got %d search URLs, want 5 (3 rows + prev + next): %q", len(urls), urls)
		}
		for _, u := range urls {
			if !strings.Contains(u, "m=contains") {
				t.Errorf("search URL %q drops the mode", u)
			}
			if !strings.Contains(u, "sort=relevance") {
				t.Errorf("search URL %q drops the ordering", u)
			}
		}
		// The step links specifically, by hit index.
		for _, want := range []string{"i=1&m=contains", "i=3&m=contains"} {
			if !strings.Contains(body, want) && !strings.Contains(body, strings.ReplaceAll(want, "&", "&amp;")) {
				t.Errorf("stepping link %q missing from the panel: %q", want, body)
			}
		}
	})

	t.Run("word mode with newest first emits the URL shape it always did", func(t *testing.T) {
		// A bookmarked default-mode URL must not grow parameters, so existing
		// links keep working and round-trip back to the same search.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&i=2", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		for _, u := range searchURLs(t, rr.Body.String()) {
			if strings.Contains(u, "m=") || strings.Contains(u, "sort=") {
				t.Errorf("default-mode URL %q should carry neither parameter", u)
			}
		}
	})

	t.Run("the controls keep their state on a too-short query", func(t *testing.T) {
		// A mode selector that resets itself the moment you type one character
		// is a bug, so the refreshed controls carry the choice even on the path
		// that never reaches the source.
		v, stub := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=h&m=contains&sort=relevance", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		if stub.calls != 0 {
			t.Errorf("a 1-character query must not reach the source, got %d calls", stub.calls)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "at least 2 characters") {
			t.Errorf("the panel should still prompt for a longer query: %q", body)
		}
		for _, want := range []string{
			`name="m" value="contains" checked`,
			`name="sort" value="relevance" disabled`,
			`name="sort" value="newest" checked`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the refreshed controls should contain %q: %q", want, body)
			}
		}
	})

	t.Run("an unbuildable index is a view state, not a 500", func(t *testing.T) {
		// A word search whose index cannot be built is explainable, so the
		// panel explains it.  It must not quietly re-run in contains mode
		// either: the user picked a mode and would be shown answers to a
		// different question.
		//
		// The copy deliberately does not name a single cause.  Through the
		// viewer a genuinely read-only archive never gets this far — it fails
		// in migrate at open — so the reachable case is a transient write
		// failure such as another process holding the write lock.
		v, stub := newSearchViewer(t, 3)
		stub.err = dbase.ErrIndexUnavailable
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if stub.calls != 1 {
			t.Errorf("SearchMessages called %d times, want 1 — no silent retry in another mode", stub.calls)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "could not be built") {
			t.Errorf("the panel should explain why word search has no index: %q", body)
		}
		if strings.Contains(body, "No matches") {
			t.Error("an unbuildable index is not an empty result and must not be reported as one")
		}
		if want := "/search?m=contains&q=hello"; !hasSearchURL(t, body, want) {
			t.Errorf("the explanation should offer %q as the way out: %q", want, body)
		}
		if !strings.Contains(body, `name="m" value="words" checked`) {
			t.Error("the mode must stay as chosen, so the controls still show Words")
		}
	})

	t.Run("a wrapped unavailability is still recognised", func(t *testing.T) {
		v, stub := newSearchViewer(t, 3)
		stub.err = fmt.Errorf("search: %w: database is readonly", dbase.ErrIndexUnavailable)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "could not be built") {
			t.Errorf("a wrapped ErrIndexUnavailable should still render the explanation: %q", rr.Body.String())
		}
	})

	t.Run("an unrelated error is still a 500", func(t *testing.T) {
		v, stub := newSearchViewer(t, 3)
		stub.err = errors.New("disk on fire")
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rr.Code)
		}
	})
}

func TestSearchControls(t *testing.T) {
	// The controls live in the sidebar, which renders from mainView.  Its
	// .Search is nil until a search runs, so the default state is a template
	// question, not a handler one.
	t.Run("an ordinary page load defaults to words and newest", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		var buf strings.Builder
		if err := v.RenderIndex(t.Context(), &buf); err != nil {
			t.Fatalf("RenderIndex() error = %v", err)
		}
		body := buf.String()
		for _, want := range []string{
			`id="search-controls"`,
			`name="m" value="words" checked`,
			`name="m" value="contains"`,
			`name="sort" value="newest" checked`,
			`name="sort" value="relevance"`,
			"Words (FTS)",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the sidebar should render %q: %q", want, body)
			}
		}
		for _, absent := range []string{
			`name="m" value="contains" checked`,
			`name="sort" value="relevance" checked`,
			`value="relevance" disabled`,
			"hx-swap-oob",
		} {
			if strings.Contains(body, absent) {
				t.Errorf("a page load with no search must not render %q", absent)
			}
		}
	})

	t.Run("m=contains checks Contains and disables Relevance", func(t *testing.T) {
		// Disabled, never hidden: a control that vanishes teaches nothing, a
		// greyed one teaches that relevance belongs to word search.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&m=contains", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, `name="m" value="contains" checked`) {
			t.Errorf("the requested mode should be the checked one: %q", body)
		}
		if strings.Contains(body, `name="m" value="words" checked`) {
			t.Error("both modes cannot be checked at once")
		}
		if !strings.Contains(body, `name="sort" value="relevance" disabled`) {
			t.Errorf("relevance has no meaning for a substring scan and must render disabled: %q", body)
		}
		if !strings.Contains(body, "Relevance") {
			t.Error("the disabled control must still be visible")
		}
	})

	t.Run("sort=relevance checks Relevance", func(t *testing.T) {
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&sort=relevance", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, `name="sort" value="relevance" checked`) {
			t.Errorf("the requested ordering should be the checked one: %q", body)
		}
		if strings.Contains(body, `value="relevance" checked disabled`) {
			t.Error("relevance is available in word mode and must not be disabled there")
		}
		if strings.Contains(body, `name="sort" value="newest" checked`) {
			t.Error("both orderings cannot be checked at once")
		}
	})

	t.Run("contains mode shows the sort that actually ran", func(t *testing.T) {
		// A substring scan has no bm25 score, so the source ignores byRelevance
		// and orders by recency.  A checked Relevance radio would report a sort
		// that did not happen.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&m=contains&sort=relevance", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, `name="sort" value="newest" checked`) {
			t.Errorf("contains mode sorts by recency and the controls should say so: %q", body)
		}
		if !strings.Contains(body, `name="sort" value="relevance" disabled`) {
			t.Errorf("relevance should render disabled and unchecked: %q", body)
		}
		if strings.Contains(body, `value="relevance" checked`) {
			t.Error("a relevance sort that did not happen must not render as checked")
		}

		// The same ordering in word mode is a real relevance search, so the
		// request state itself is untouched — it is what SearchHit puts in the
		// URL and what comes back when the mode flips.
		v2, _ := newSearchViewer(t, 3)
		req2 := httptest.NewRequest(http.MethodGet, "/search?q=hello&sort=relevance", nil)
		rr2 := httptest.NewRecorder()
		v2.searchHandler(rr2, req2)
		if !strings.Contains(rr2.Body.String(), `name="sort" value="relevance" checked`) {
			t.Errorf("word mode still checks Relevance: %q", rr2.Body.String())
		}
	})

	t.Run("the htmx response refreshes the controls out of band", func(t *testing.T) {
		// The sidebar does not re-render on a panel swap, so a mode reached by
		// clicking an escape link would leave the radios lying about the
		// results — and the next keystroke would silently revert the mode.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&m=contains", nil)
		req.Header.Set("HX-Request", "true")
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if !strings.Contains(body, `id="search-controls" class="search-controls-group" hx-swap-oob="outerHTML"`) {
			t.Errorf("the htmx response should carry an out-of-band controls refresh: %q", body)
		}
		if !strings.Contains(body, `name="m" value="contains" checked`) {
			t.Errorf("the refreshed controls should show the mode that produced the results: %q", body)
		}
	})

	t.Run("a full page render carries no out-of-band controls", func(t *testing.T) {
		// hx-swap-oob is inert outside an htmx response: emitted into a freshly
		// loaded document it is stray markup, and an earlier cut of this
		// feature shipped exactly that, rendering the controls twice.
		v, _ := newSearchViewer(t, 3)
		req := httptest.NewRequest(http.MethodGet, "/search?q=hello&m=contains", nil) // no HX-Request
		rr := httptest.NewRecorder()
		v.searchHandler(rr, req)

		body := rr.Body.String()
		if n := strings.Count(body, `id="search-controls"`); n != 1 {
			t.Errorf("found %d #search-controls elements, want exactly 1", n)
		}
		if strings.Contains(body, "hx-swap-oob") {
			t.Error("a bookmarked search must not emit an out-of-band fragment")
		}
	})
}
