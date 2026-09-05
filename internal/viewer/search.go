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
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/chunk/backend/dbase"
)

// minQueryLen is the shortest query that reaches the source.  Anything shorter
// would match most of the archive and is not worth a round trip.
const minQueryLen = 2

// searchLimit caps the result set.  Without relevance ranking the cap is by
// recency; the panel says so, and says when it truncated.
const searchLimit = 500

// The viewer's mode strings are the dbase constants, aliased rather than
// re-declared: they are the wire values of ?m= and of the search interface's
// mode parameter at once, so a second copy could drift from the layer that
// interprets them.
const (
	searchModeWords    = dbase.SearchModeWords
	searchModeContains = dbase.SearchModeContains
)

// messageSearcher is the feature gate for search.  Sources implementing it get
// a search UI; sources that do not get none, and /search returns 404 — the
// same shape as the aliaser gate.  It is unexported and satisfied by runtime
// type assertion so that source.Sourcer is not widened.
//
// mode and byRelevance mirror dbase.Source.SearchMessages's mode/ordering
// parameters (dbase.SearchModeWords / dbase.SearchModeContains), passed
// through as plain strings/bools rather than a shared type because dbase
// cannot import this package.  Both come straight from the request; see
// requestedMode and requestedRelevance.
type messageSearcher interface {
	SearchMessages(ctx context.Context, query, channelID, mode string, byRelevance bool, limit int) ([]slack.Message, bool, error)
}

func (v *Viewer) searcher() (messageSearcher, bool) {
	s, ok := v.src.(messageSearcher)
	return s, ok
}

func (v *Viewer) canSearch() bool {
	_, ok := v.searcher()
	return ok
}

// searchHit is one row in the results panel.
type searchHit struct {
	Index int // 1-based position in the result set
	// ChannelName is template.HTML, not string, because channelDisplayName
	// emits <em> around an alias.  It escapes the user-supplied parts itself,
	// so this is safe; typing it as string would render a literal "<em>".
	ChannelName template.HTML
	Excerpt     string // plain text; no term marking, deliberately
	IsThread    bool
	Active      bool
	URL         string
}

// searchView is the results panel's template data.
// The panel never needs an Interactive flag: search only renders in live mode,
// so inside this view it would always be true.  Nor a scope name — the scope is
// already visible as the checked radio in the sidebar.
type searchView struct {
	Query     string
	ChannelID string // "" means all conversations
	// Mode and ByRelevance are the choices this search was run with, echoed
	// back so the controls render in the state that produced the results.
	// They are set on every render path, including the ones that never reach
	// the source: a selector that resets itself as you type is a bug.
	//
	// There is deliberately no SearchIndexReady counterpart.  The full-text
	// index is built lazily on the first word search, so at render time
	// "is there an index" has no answer — only IndexUnavailable, which is the
	// answer a search actually produced.
	Mode        string // searchModeWords or searchModeContains
	ByRelevance bool
	// IndexUnavailable reports that word mode was asked for on an archive
	// whose full-text index cannot be built.  It is a view state, not an
	// error: the panel explains it and the mode stays as chosen.
	IndexUnavailable bool
	Hits             []searchHit
	Truncated        bool
	Active           int // 1-based index of the active hit, 0 when none
	TooShort         bool
	PrevURL          string // activation URL for the previous hit; empty on the first
	NextURL          string // activation URL for the next hit; empty on the last
	// PrefixURL re-runs the query as a prefix search; ContainsURL re-runs it as a
	// substring scan.  Both are set only for an empty word-mode result, the one
	// state where the user needs an escape offered rather than explained.
	PrefixURL   string
	ContainsURL string
	// OOB is the conversation view accompanying an activated hit, rendered as
	// an hx-swap-oob fragment that replaces #conversation.  Nil when no hit is
	// active.
	OOB *mainView
}

// excerptLen is how much of a matching message a result row shows.
const excerptLen = 140

// excerpt shortens s for a result row.  There is deliberately no term marking:
// the term is already in the search box, and a future FTS snippet() replaces
// this wholesale.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= excerptLen {
		return s
	}
	return string([]rune(s)[:excerptLen]) + "…"
}

// buildHits converts raw matches into panel rows.  mode and byRelevance are
// carried into every row URL so that clicking a result re-runs the search the
// user actually asked for.
func (v *Viewer) buildHits(msgs []slack.Message, query, channelID, mode string, byRelevance bool) []searchHit {
	hits := make([]searchHit, 0, len(msgs))
	for i, m := range msgs {
		ch, _ := v.ch.find(m.Channel)
		hits = append(hits, searchHit{
			Index:       i + 1,
			ChannelName: v.channelDisplayName(ch),
			Excerpt:     excerpt(m.Text),
			IsThread:    m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp,
			URL:         v.rts.SearchHit(query, channelID, i+1, mode, byRelevance),
		})
	}
	return hits
}

// renderSearch writes the panel: a partial for HTMX, a full page for a direct
// link so the results are bookmarkable.
func (v *Viewer) renderSearch(w http.ResponseWriter, r *http.Request, sv searchView) {
	if isHXRequest(r) {
		// hx_search_response is the panel plus an out-of-band refresh of the
		// mode controls, which live in the sidebar and would otherwise keep
		// showing whatever mode the page was loaded with.  The wrapper exists
		// so the OOB fragment cannot reach the full-page branch below, where
		// hx-swap-oob is inert markup rather than an instruction.
		if err := v.tmpl.ExecuteTemplate(w, "hx_search_response", sv); err != nil {
			v.lg.ErrorContext(r.Context(), "ExecuteTemplate", "error", err, "template", "hx_search_response")
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// A direct link must render the activated hit into the real #conversation
	// section.  hx-swap-oob only means something to htmx while processing an
	// AJAX response: emitted in a freshly loaded document it is inert markup,
	// leaving a second #conversation nested inside the results panel and the
	// real one showing the placeholder.  So fold the OOB view into the page and
	// drop the fragment.
	page := v.view()
	if sv.OOB != nil {
		page = *sv.OOB
		sv.OOB = nil
	}
	page.Search = &sv
	if err := v.tmpl.ExecuteTemplate(w, "index.html", page); err != nil {
		v.lg.ErrorContext(r.Context(), "ExecuteTemplate", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// searchHandler serves the search results panel.
func (v *Viewer) searchHandler(w http.ResponseWriter, r *http.Request) {
	if !v.canSearch() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	query := r.URL.Query().Get("q")
	channelID := r.URL.Query().Get("ch")
	mode := requestedMode(r)
	byRelevance := requestedRelevance(r)

	view := searchView{
		Query:       query,
		ChannelID:   channelID,
		Mode:        mode,
		ByRelevance: byRelevance,
	}
	if len([]rune(query)) < minQueryLen {
		view.TooShort = true
		v.renderSearch(w, r, view)
		return
	}
	s, _ := v.searcher()
	hits, truncated, err := s.SearchMessages(ctx, query, channelID, mode, byRelevance, searchLimit)
	if err != nil {
		if errors.Is(err, dbase.ErrIndexUnavailable) {
			// Word search needs an index this archive cannot build — a
			// read-only file, typically.  That is a state the panel can
			// explain, not a server fault, so it renders with no hits and a
			// 200.  Quietly retrying in contains mode would answer a question
			// the user did not ask, which is exactly what a per-query mode
			// switch exists to avoid.
			v.lg.WarnContext(ctx, "full-text index unavailable", "error", err)
			view.IndexUnavailable = true
			view.ContainsURL = v.rts.SearchHit(query, channelID, 0, searchModeContains, false)
			v.renderSearch(w, r, view)
			return
		}
		v.lg.ErrorContext(ctx, "SearchMessages", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.Truncated = truncated
	view.Hits = v.buildHits(hits, query, channelID, mode, byRelevance)
	if mode == searchModeWords && len(view.Hits) == 0 {
		// Word matching is whole-token, so on an inflected language a stem
		// finds nothing that a substring scan would find.  Offer both ways out
		// rather than explaining the difference in prose.  A query that already
		// ends in a star has no prefix retry left to suggest.
		if !strings.HasSuffix(query, "*") {
			view.PrefixURL = v.rts.SearchHit(query+"*", channelID, 0, searchModeWords, byRelevance)
		}
		view.ContainsURL = v.rts.SearchHit(query, channelID, 0, searchModeContains, false)
	}

	if i := requestedHit(r); i >= 1 && i <= len(view.Hits) {
		view.Hits[i-1].Active = true
		view.Active = i
		if i > 1 {
			view.PrevURL = v.rts.SearchHit(query, channelID, i-1, mode, byRelevance)
		}
		if i < len(view.Hits) {
			view.NextURL = v.rts.SearchHit(query, channelID, i+1, mode, byRelevance)
		}
		oob, err := v.activate(ctx, hits[i-1])
		if err != nil {
			v.lg.ErrorContext(ctx, "activate", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		view.OOB = oob
	}
	v.renderSearch(w, r, view)
}

// requestedHit reads the ?i= hit index.  Absent and malformed values yield 0,
// meaning "no active hit" — the same tolerant shape as requestedPage.
func requestedHit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil {
		return 0
	}
	return n
}

// requestedMode reads the ?m= matching mode.  Words is the default, so absent,
// empty and unrecognised values all yield it and only an exact "contains"
// selects the substring scan.  Like requestedPage and requestedHit it never
// errors: a hand-edited URL degrades to the mode most searches want rather
// than producing a 500.
func requestedMode(r *http.Request) string {
	if r.URL.Query().Get("m") == searchModeContains {
		return searchModeContains
	}
	return searchModeWords
}

// requestedRelevance reads the ?sort= ordering.  Only an exact "relevance"
// asks for bm25 ranking; everything else, absent and malformed included, means
// newest first.  The source ignores it outside word mode, a LIKE scan having
// no score to rank by.
func requestedRelevance(r *http.Request) bool {
	return r.URL.Query().Get("sort") == "relevance"
}

// activate builds the conversation view that accompanies an activated hit.
// A channel message resolves to the page holding it; a thread reply renders
// the thread itself, anchored on the reply.  Either way the message is
// highlighted.
func (v *Viewer) activate(ctx context.Context, m slack.Message) (*mainView, error) {
	ci, err := v.src.ChannelInfo(ctx, m.Channel)
	if err != nil {
		return nil, err
	}
	page := v.view()
	if err := v.setConversation(&page, ci); err != nil {
		return nil, err
	}
	page.HighlightTS = m.Timestamp
	if m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
		it, err := v.src.AllThreadMessages(ctx, m.Channel, m.ThreadTimestamp)
		if err != nil {
			return nil, err
		}
		page.ThreadMessages = it
		page.ThreadID = m.ThreadTimestamp
		page.ThreadInMain = true
		return &page, nil
	}
	n, err := v.messagePageOf(ctx, m.Channel, m.Timestamp)
	if err != nil {
		return nil, err
	}
	it, pv, err := v.channelPage(ctx, m.Channel, n)
	if err != nil {
		return nil, err
	}
	page.Messages = it
	page.Paging = pv
	return &page, nil
}
