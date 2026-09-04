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

	"github.com/rusq/slack"

	"github.com/rusq/slackdump/v4/internal/viewer/renderer"
	"github.com/rusq/slackdump/v4/source"
)

// pageCount returns the number of pages needed to hold total messages at size
// messages per page.  It never returns less than 1: an empty channel is page 1
// of 1.
func pageCount(total, size int) int {
	if size <= 0 {
		return 1
	}
	return max(1, (total+size-1)/size)
}

// clampPage brings a requested page number into range.  Anything outside
// [1, pages] — including the 0 that means "unspecified" — resolves to the last
// page, which is where the viewer opens a channel.
func clampPage(page, pages int) int {
	if page < 1 || page > pages {
		return pages
	}
	return page
}

// pageFromSeq walks it exactly once and returns the messages of the requested
// page, the total number of messages, and the page actually resolved.  A page
// of 0 or less selects the last page.
//
// The sequence is walked once and only once: for database sources it is backed
// by live sql.Rows and re-iterating would yield nothing.  Because an
// out-of-range page always clamps to the last page, keeping the requested
// window alongside a rolling window of the trailing size messages covers every
// outcome without a second pass.
//
// It is only ever called with size > 0; channelPage short-circuits before this
// when paging is disabled.
func pageFromSeq(it iter.Seq2[slack.Message, error], size, page int) (msgs []slack.Message, total, resolved int, err error) {
	var (
		window []slack.Message // the explicitly requested page
		tail   []slack.Message // rolling window of the last size messages
		lo, hi int
		wanted = page >= 1
	)
	if wanted {
		lo = (page - 1) * size
		hi = lo + size
	}
	for m, iterErr := range it {
		if iterErr != nil {
			return nil, 0, 0, iterErr
		}
		if wanted && total >= lo && total < hi {
			window = append(window, m)
		}
		tail = append(tail, m)
		if len(tail) > size {
			tail = tail[1:]
		}
		total++
	}

	pages := pageCount(total, size)
	resolved = clampPage(page, pages)
	if resolved == page {
		return window, total, resolved, nil
	}
	// Clamping only ever lands on the last page, which tail holds — but tail
	// is a full-size rolling buffer and the last page is usually shorter, so
	// trim it to the real length of the final page.
	last := total - (pages-1)*size
	if last < 0 || last > len(tail) {
		last = len(tail)
	}
	return tail[len(tail)-last:], total, resolved, nil
}

// messagePager is an optional extension interface for sources that can return
// a window of a channel timeline without materialising all of it.  It is
// deliberately unexported and satisfied by runtime type assertion: adding
// these methods to source.Sourcer would break third-party implementations.
// Sources that do not implement it fall back to pageFromSeq.
type messagePager interface {
	CountMessages(ctx context.Context, channelID string) (int64, error)
	MessagesPage(ctx context.Context, channelID string, limit, offset int) (iter.Seq2[slack.Message, error], error)
	MessageOrdinal(ctx context.Context, channelID, ts string) (int64, error)
}

// pagingView is the paging footer's template data.  A nil *pagingView means
// the timeline is unpaged and no footer renders.
type pagingView struct {
	Page    int    // 1-based, already clamped into range
	Pages   int    // total number of pages, never less than 1
	Total   int    // total number of messages in the timeline
	PrevURL string // link to the older page; empty on the first page
	NextURL string // link to the newer page; empty on the last page
}

// newPagingView builds the footer data for a resolved page.  An empty PrevURL
// or NextURL is what the template tests to disable that control — there are no
// separate HasPrev/HasNext flags to keep in sync.
func newPagingView(rts *renderer.Routes, channelID string, page, pages, total int) *pagingView {
	pv := &pagingView{Page: page, Pages: pages, Total: total}
	if page > 1 {
		pv.PrevURL = rts.ChannelPage(channelID, page-1)
	}
	if page < pages {
		pv.NextURL = rts.ChannelPage(channelID, page+1)
	}
	return pv
}

// channelPage returns the messages for the requested page of a channel and the
// footer data describing it.  A page of 0 or less selects the last page, which
// is where a channel opens.  When paging is disabled it returns the whole
// timeline and a nil *pagingView, which renders exactly as it did before paging
// existed.
func (v *Viewer) channelPage(ctx context.Context, channelID string, page int) (iter.Seq2[slack.Message, error], *pagingView, error) {
	if v.pageSize <= 0 {
		it, err := v.allMessagesOrEmpty(ctx, channelID)
		return it, nil, err
	}

	if mp, ok := v.src.(messagePager); ok {
		total, err := mp.CountMessages(ctx, channelID)
		if err != nil {
			if !errors.Is(err, source.ErrNotFound) {
				return nil, nil, err
			}
			total = 0
		}
		pages := pageCount(int(total), v.pageSize)
		resolved := clampPage(page, pages)
		it, err := mp.MessagesPage(ctx, channelID, v.pageSize, (resolved-1)*v.pageSize)
		if err != nil {
			if !errors.Is(err, source.ErrNotFound) {
				return nil, nil, err
			}
			it = sliceMessages(nil)
		}
		return it, newPagingView(v.rts, channelID, resolved, pages, int(total)), nil
	}

	src, err := v.allMessagesOrEmpty(ctx, channelID)
	if err != nil {
		return nil, nil, err
	}
	mm, total, resolved, err := pageFromSeq(src, v.pageSize, page)
	if err != nil {
		return nil, nil, err
	}
	return sliceMessages(mm), newPagingView(v.rts, channelID, resolved, pageCount(total, v.pageSize), total), nil
}

// messagePageOf returns the 1-based page that the message ts falls on.  It
// returns 0 when paging is disabled, which makes the paged and unpaged link
// forms interchangeable at the call site.
func (v *Viewer) messagePageOf(ctx context.Context, channelID, ts string) (int, error) {
	if v.pageSize <= 0 {
		return 0, nil
	}
	if mp, ok := v.src.(messagePager); ok {
		ord, err := mp.MessageOrdinal(ctx, channelID, ts)
		if err != nil {
			return 0, err
		}
		return int(ord)/v.pageSize + 1, nil
	}
	it, err := v.allMessagesOrEmpty(ctx, channelID)
	if err != nil {
		return 0, err
	}
	var ord int
	for m, iterErr := range it {
		if iterErr != nil {
			return 0, iterErr
		}
		if m.Timestamp == ts {
			break
		}
		ord++
	}
	return ord/v.pageSize + 1, nil
}

// sliceMessages adapts a materialised page back to the iterator the templates
// consume.
func sliceMessages(mm []slack.Message) iter.Seq2[slack.Message, error] {
	return func(yield func(slack.Message, error) bool) {
		for _, m := range mm {
			if !yield(m, nil) {
				return
			}
		}
	}
}
