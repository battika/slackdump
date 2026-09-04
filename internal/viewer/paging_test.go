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
	"iter"
	"strconv"
	"testing"

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/viewer/renderer"
)

// seqOf builds a message sequence whose texts are "1".."n".
func seqOf(n int) iter.Seq2[slack.Message, error] {
	mm := make([]slack.Message, 0, n)
	for i := 1; i <= n; i++ {
		mm = append(mm, slack.Message{Msg: slack.Msg{Text: strconv.Itoa(i)}})
	}
	return messageSeq(mm)
}

func texts(mm []slack.Message) []string {
	out := make([]string, 0, len(mm))
	for _, m := range mm {
		out = append(out, m.Text)
	}
	return out
}

func Test_pageCount(t *testing.T) {
	tests := []struct {
		name  string
		total int
		size  int
		want  int
	}{
		{"empty channel is one page", 0, 100, 1},
		{"exact multiple", 200, 100, 2},
		{"partial last page", 201, 100, 3},
		{"smaller than a page", 5, 100, 1},
		{"zero size", 5, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pageCount(tt.total, tt.size); got != tt.want {
				t.Errorf("pageCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func Test_clampPage(t *testing.T) {
	tests := []struct {
		name  string
		page  int
		pages int
		want  int
	}{
		{"unspecified means last", 0, 96, 96},
		{"negative means last", -3, 96, 96},
		{"past the end means last", 999, 96, 96},
		{"in range is kept", 42, 96, 42},
		{"first page", 1, 96, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampPage(tt.page, tt.pages); got != tt.want {
				t.Errorf("clampPage() = %d, want %d", got, tt.want)
			}
		})
	}
}

func Test_pageFromSeq(t *testing.T) {
	tests := []struct {
		name         string
		n            int
		size         int
		page         int
		wantTexts    []string
		wantTotal    int
		wantResolved int
	}{
		{"last page by default", 5, 2, 0, []string{"5"}, 5, 3},
		// The only input where the trim computes to a full size rather than a
		// remainder, so it guards the exact-multiple branch of the arithmetic.
		{"last page is a full page (exact multiple)", 4, 2, 0, []string{"3", "4"}, 4, 2},
		{"first page", 5, 2, 1, []string{"1", "2"}, 5, 1},
		{"middle page", 5, 2, 2, []string{"3", "4"}, 5, 2},
		{"page past the end clamps to last", 5, 2, 99, []string{"5"}, 5, 3},
		{"page zero clamps to last", 5, 2, -1, []string{"5"}, 5, 3},
		{"single full page", 2, 2, 1, []string{"1", "2"}, 2, 1},
		{"empty channel", 0, 2, 0, nil, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm, total, resolved, err := pageFromSeq(seqOf(tt.n), tt.size, tt.page)
			if err != nil {
				t.Fatalf("pageFromSeq() error = %v", err)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}
			if resolved != tt.wantResolved {
				t.Errorf("resolved page = %d, want %d", resolved, tt.wantResolved)
			}
			got := texts(mm)
			if len(got) != len(tt.wantTexts) {
				t.Fatalf("messages = %v, want %v", got, tt.wantTexts)
			}
			for i := range got {
				if got[i] != tt.wantTexts[i] {
					t.Fatalf("messages = %v, want %v", got, tt.wantTexts)
				}
			}
		})
	}

	t.Run("walks the sequence exactly once", func(t *testing.T) {
		var passes int
		it := func(yield func(slack.Message, error) bool) {
			passes++
			for i := 1; i <= 5; i++ {
				if !yield(slack.Message{Msg: slack.Msg{Text: strconv.Itoa(i)}}, nil) {
					return
				}
			}
		}
		if _, _, _, err := pageFromSeq(it, 2, 99); err != nil {
			t.Fatalf("pageFromSeq() error = %v", err)
		}
		if passes != 1 {
			t.Errorf("pageFromSeq() walked the sequence %d times, want 1", passes)
		}
	})

	t.Run("propagates an iteration error", func(t *testing.T) {
		sentinel := errors.New("boom")
		it := func(yield func(slack.Message, error) bool) {
			yield(slack.Message{Msg: slack.Msg{Text: "1"}}, nil)
			yield(slack.Message{}, sentinel)
		}
		if _, _, _, err := pageFromSeq(it, 2, 1); !errors.Is(err, sentinel) {
			t.Errorf("pageFromSeq() error = %v, want %v", err, sentinel)
		}
	})
}

// pagerStub is an aliasSourceStub that also implements messagePager, so the
// type assertion in channelPage succeeds.
type pagerStub struct {
	*aliasSourceStub
	pageCalls int
}

func (p *pagerStub) CountMessages(_ context.Context, channelID string) (int64, error) {
	return int64(len(p.msgs[channelID])), nil
}

func (p *pagerStub) MessagesPage(_ context.Context, channelID string, limit, offset int) (iter.Seq2[slack.Message, error], error) {
	p.pageCalls++
	mm := p.msgs[channelID]
	if offset > len(mm) {
		offset = len(mm)
	}
	end := len(mm)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return messageSeq(mm[offset:end]), nil
}

func (p *pagerStub) MessageOrdinal(_ context.Context, channelID, ts string) (int64, error) {
	for i, m := range p.msgs[channelID] {
		if m.Timestamp == ts {
			return int64(i), nil
		}
	}
	return 0, nil
}

func newPagerViewer(t *testing.T, n, pageSize int) (*Viewer, *pagerStub) {
	t.Helper()
	mm := make([]slack.Message, 0, n)
	for i := 1; i <= n; i++ {
		mm = append(mm, slack.Message{Msg: slack.Msg{Text: strconv.Itoa(i), Timestamp: strconv.Itoa(100+i) + ".000000"}})
	}
	src := newViewerRouteSource()
	src.msgs = map[string][]slack.Message{"C1": mm}
	stub := &pagerStub{aliasSourceStub: src}
	v := newHandlerTestViewer(src)
	v.src = stub
	v.pageSize = pageSize
	v.rts = renderer.NewRoutes(renderer.ModeLive, renderer.WithPaging(pageSize > 0))
	initTemplates(v)
	return v, stub
}

func TestViewer_channelPage(t *testing.T) {
	t.Run("uses the source fast path when available", func(t *testing.T) {
		v, stub := newPagerViewer(t, 5, 2)
		it, pv, err := v.channelPage(t.Context(), "C1", 0)
		if err != nil {
			t.Fatalf("channelPage() error = %v", err)
		}
		if stub.pageCalls != 1 {
			t.Errorf("MessagesPage called %d times, want 1", stub.pageCalls)
		}
		// The window itself matters as much as the footer data: the last page
		// of 5 messages at 2 per page holds only the fifth.
		var got []string
		for m, err := range it {
			if err != nil {
				t.Fatalf("iteration error = %v", err)
			}
			got = append(got, m.Text)
		}
		if len(got) != 1 || got[0] != "5" {
			t.Errorf("messages = %v, want [5]", got)
		}
		if pv == nil {
			t.Fatal("channelPage() returned nil paging view")
		}
		if pv.Page != 3 || pv.Pages != 3 || pv.Total != 5 {
			t.Errorf("paging = page %d of %d, total %d; want page 3 of 3, total 5", pv.Page, pv.Pages, pv.Total)
		}
		if pv.PrevURL == "" || pv.NextURL != "" {
			t.Errorf("last page should link back and not forward, got prev=%q next=%q", pv.PrevURL, pv.NextURL)
		}
		if pv.PrevURL != "/archives/C1?p=2" {
			t.Errorf("PrevURL = %q, want %q", pv.PrevURL, "/archives/C1?p=2")
		}
	})

	t.Run("falls back to iterating sources without the interface", func(t *testing.T) {
		src := newViewerRouteSource()
		src.msgs = map[string][]slack.Message{"C1": {
			{Msg: slack.Msg{Text: "1"}}, {Msg: slack.Msg{Text: "2"}}, {Msg: slack.Msg{Text: "3"}},
		}}
		v := newHandlerTestViewer(src)
		v.pageSize = 2
		v.rts = renderer.NewRoutes(renderer.ModeLive, renderer.WithPaging(true))
		initTemplates(v)

		it, pv, err := v.channelPage(t.Context(), "C1", 1)
		if err != nil {
			t.Fatalf("channelPage() error = %v", err)
		}
		if pv.Page != 1 || pv.Pages != 2 || pv.Total != 3 {
			t.Errorf("paging = page %d of %d, total %d; want page 1 of 2, total 3", pv.Page, pv.Pages, pv.Total)
		}
		var got []string
		for m, err := range it {
			if err != nil {
				t.Fatalf("iteration error = %v", err)
			}
			got = append(got, m.Text)
		}
		if len(got) != 2 || got[0] != "1" || got[1] != "2" {
			t.Errorf("messages = %v, want [1 2]", got)
		}
	})

	t.Run("paging disabled returns everything and no paging view", func(t *testing.T) {
		v, _ := newPagerViewer(t, 5, 0)
		it, pv, err := v.channelPage(t.Context(), "C1", 0)
		if err != nil {
			t.Fatalf("channelPage() error = %v", err)
		}
		if pv != nil {
			t.Errorf("paging view = %+v, want nil", pv)
		}
		var n int
		for range it {
			n++
		}
		if n != 5 {
			t.Errorf("messages = %d, want 5", n)
		}
	})

	t.Run("missing channel degrades to empty", func(t *testing.T) {
		v := newHandlerTestViewer(newViewerRouteSource())
		v.pageSize = 2
		it, _, err := v.channelPage(t.Context(), "NOPE", 0)
		if err != nil {
			t.Fatalf("channelPage() error = %v", err)
		}
		for range it {
			t.Fatal("expected no messages")
		}
	})
}
