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
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/rusq/slack"
)

// minQueryLen is the shortest query that reaches the source.  Anything shorter
// would match most of the archive and is not worth a round trip.
const minQueryLen = 2

// searchLimit caps the result set.  Without relevance ranking the cap is by
// recency; the panel says so, and says when it truncated.
const searchLimit = 500

// messageSearcher is the feature gate for search.  Sources implementing it get
// a search UI; sources that do not get none, and /search returns 404 — the
// same shape as the aliaser gate.  It is unexported and satisfied by runtime
// type assertion so that source.Sourcer is not widened.
type messageSearcher interface {
	SearchMessages(ctx context.Context, query, channelID string, limit int) ([]slack.Message, bool, error)
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
	Hits      []searchHit
	Truncated bool
	Active    int // 1-based index of the active hit, 0 when none
	TooShort  bool
	PrevURL   string // activation URL for the previous hit; empty on the first
	NextURL   string // activation URL for the next hit; empty on the last
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

// buildHits converts raw matches into panel rows.
func (v *Viewer) buildHits(msgs []slack.Message, query, channelID string) []searchHit {
	hits := make([]searchHit, 0, len(msgs))
	for i, m := range msgs {
		ch, _ := v.ch.find(m.Channel)
		hits = append(hits, searchHit{
			Index:       i + 1,
			ChannelName: v.channelDisplayName(ch),
			Excerpt:     excerpt(m.Text),
			IsThread:    m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp,
			URL:         v.rts.SearchHit(query, channelID, i+1),
		})
	}
	return hits
}

// renderSearch writes the panel: a partial for HTMX, a full page for a direct
// link so the results are bookmarkable.
func (v *Viewer) renderSearch(w http.ResponseWriter, r *http.Request, sv searchView) {
	if isHXRequest(r) {
		if err := v.tmpl.ExecuteTemplate(w, "hx_search", sv); err != nil {
			v.lg.ErrorContext(r.Context(), "ExecuteTemplate", "error", err, "template", "hx_search")
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

	view := searchView{
		Query:     query,
		ChannelID: channelID,
	}
	if len([]rune(query)) < minQueryLen {
		view.TooShort = true
		v.renderSearch(w, r, view)
		return
	}
	s, _ := v.searcher()
	hits, truncated, err := s.SearchMessages(ctx, query, channelID, searchLimit)
	if err != nil {
		v.lg.ErrorContext(ctx, "SearchMessages", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.Truncated = truncated
	view.Hits = v.buildHits(hits, query, channelID)

	if i := requestedHit(r); i >= 1 && i <= len(view.Hits) {
		view.Hits[i-1].Active = true
		view.Active = i
		if i > 1 {
			view.PrevURL = v.rts.SearchHit(query, channelID, i-1)
		}
		if i < len(view.Hits) {
			view.NextURL = v.rts.SearchHit(query, channelID, i+1)
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
