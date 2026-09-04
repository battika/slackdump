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
	"slices"
	"testing"

	"github.com/rusq/slack"

	st "github.com/rusq/slackdump/v4/internal/structures"
)

// testChannel builds a slack.Channel for sidebar tests.  With no options it
// yields a public channel; the as* options switch the conversation type.
func testChannel(id, name string, opts ...func(*slack.Channel)) slack.Channel {
	ch := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Name:         name,
			Conversation: slack.Conversation{ID: id},
		},
	}
	for _, opt := range opts {
		opt(&ch)
	}
	return ch
}

func asIM(ch *slack.Channel)       { ch.IsIM = true }
func asMPIM(ch *slack.Channel)     { ch.IsMpIM = true }
func asPrivate(ch *slack.Channel)  { ch.IsPrivate = true }
func asArchived(ch *slack.Channel) { ch.IsArchived = true }

// withUser sets the counterparty user ID, which is how st.UserIndex derives
// the display name of an IM conversation.
func withUser(id string) func(*slack.Channel) {
	return func(ch *slack.Channel) { ch.User = id }
}

// withPurpose sets the purpose text, which is how st.UserIndex derives the
// display name of an MPIM conversation.
func withPurpose(s string) func(*slack.Channel) {
	return func(ch *slack.Channel) { ch.Purpose = slack.Purpose{Value: s} }
}

func channelIDs(cc []slack.Channel) []string {
	ids := make([]string, 0, len(cc))
	for _, ch := range cc {
		ids = append(ids, ch.ID)
	}
	return ids
}

func Test_channels_groups(t *testing.T) {
	var (
		pub1 = testChannel("C1", "alpha")
		pub2 = testChannel("C2", "beta")
		priv = testChannel("G1", "secret", asPrivate)
		mpim = testChannel("G2", "", asMPIM, withPurpose("team chat"))
		dm   = testChannel("D1", "", asIM, withUser("U1"))
	)
	full := channels{
		Public:  []slack.Channel{pub1, pub2},
		Private: []slack.Channel{priv},
		MPIM:    []slack.Channel{mpim},
		DM:      []slack.Channel{dm},
	}
	// wantGroup pairs a group's DOM id with the channel IDs it must contain, in order.
	type wantGroup struct {
		ID    string
		Items []string
	}
	allGroups := []wantGroup{{"public", []string{"C1", "C2"}}, {"private", []string{"G1"}}, {"mpim", []string{"G2"}}, {"dm", []string{"D1"}}}

	tests := []struct {
		name       string
		ch         channels
		activeID   string
		wantGroups []wantGroup
		wantOpen   string
	}{
		{"active public channel", full, "C2", allGroups, "public"},
		{"active private channel", full, "G1", allGroups, "private"},
		{"active mpim", full, "G2", allGroups, "mpim"},
		{"active dm", full, "D1", allGroups, "dm"},
		{"no active channel opens first group", full, "", allGroups, "public"},
		{"unknown active channel opens first group", full, "CNOPE", allGroups, "public"},
		{"empty groups omitted", channels{MPIM: []slack.Channel{mpim}, DM: []slack.Channel{dm}}, "", []wantGroup{{"mpim", []string{"G2"}}, {"dm", []string{"D1"}}}, "mpim"},
		{"no channels yields no groups", channels{}, "", []wantGroup{}, ""},
		{"blank-id channel does not hijack open group",
			channels{Public: []slack.Channel{pub1}, DM: []slack.Channel{testChannel("", "ghost", asIM)}},
			"", []wantGroup{{"public", []string{"C1"}}, {"dm", []string{""}}}, "public"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ch.groups(tt.activeID)
			if len(got) != len(tt.wantGroups) {
				t.Fatalf("groups() returned %d groups, want %d: %+v", len(got), len(tt.wantGroups), got)
			}
			var openID string
			for i, g := range got {
				if g.ID != tt.wantGroups[i].ID {
					t.Errorf("groups()[%d].ID = %q, want %q", i, g.ID, tt.wantGroups[i].ID)
				}
				if g.Label == "" {
					t.Errorf("groups()[%d].Label is empty", i)
				}
				if ids := channelIDs(g.Items); !slices.Equal(ids, tt.wantGroups[i].Items) {
					t.Errorf("groups()[%d] (%s) items = %v, want %v", i, g.ID, ids, tt.wantGroups[i].Items)
				}
				if g.Open {
					if openID != "" {
						t.Errorf("groups() opened more than one group: %q and %q", openID, g.ID)
					}
					openID = g.ID
				}
			}
			if openID != tt.wantOpen {
				t.Errorf("groups() opened %q, want %q", openID, tt.wantOpen)
			}
		})
	}

	t.Run("labels", func(t *testing.T) {
		want := map[string]string{
			"public":  "Public channels",
			"private": "Private channels",
			"mpim":    "Group messages",
			"dm":      "Direct messages",
		}
		for _, g := range full.groups("") {
			w, ok := want[g.ID]
			if !ok {
				t.Errorf("unexpected group ID %q", g.ID)
				continue
			}
			if g.Label != w {
				t.Errorf("group %q label = %q, want %q", g.ID, g.Label, w)
			}
		}
	})
}

func Test_initChannels(t *testing.T) {
	um := st.NewUserIndex([]slack.User{{ID: "U1", Name: "zoe"}, {ID: "U2", Name: "adam"}})
	input := []slack.Channel{
		testChannel("C2", "Beta"),
		testChannel("C1", "alpha"),
		testChannel("C3", "gamma"),
		testChannel("C4", "delta", asArchived),
		testChannel("C0", "alpha", asArchived),
		testChannel("G1", "zulu", asPrivate),
		testChannel("G2", "kilo", asPrivate),
		testChannel("D1", "", asIM, withUser("U1")),
		testChannel("D2", "", asIM, withUser("U2")),
		testChannel("G3", "", asMPIM, withPurpose("zeta group")),
		testChannel("G4", "", asMPIM, withPurpose("alpha group")),
	}
	cc := initChannels(input, um)

	for _, tt := range []struct {
		name string
		got  []slack.Channel
		want []string
	}{
		// "alpha" (C1) and "alpha (archived)" (C0) share a base name, and
		// "alpha" is a prefix of "alpha (archived)" so the shorter string
		// sorts first; this pins the "(archived)" suffix into the sort key.
		// Without it, C0 and C1 would tie and the ID tie-break would flip
		// their order, silently masking a regression.
		{"public sorted case-insensitively, archived suffix pins ordering", cc.Public, []string{"C1", "C0", "C2", "C4", "C3"}},
		{"private sorted", cc.Private, []string{"G2", "G1"}},
		{"mpim sorted by purpose", cc.MPIM, []string{"G4", "G3"}},
		{"dm sorted by username", cc.DM, []string{"D2", "D1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := channelIDs(tt.got); !slices.Equal(got, tt.want) {
				t.Errorf("initChannels() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("deterministic ordering for identical names", func(t *testing.T) {
		cc := initChannels([]slack.Channel{testChannel("C9", "dup"), testChannel("C8", "dup")}, um)
		if got := channelIDs(cc.Public); !slices.Equal(got, []string{"C8", "C9"}) {
			t.Errorf("initChannels() = %v, want [C8 C9]", got)
		}
	})

	t.Run("empty user index", func(t *testing.T) {
		// viewer.New never passes a nil UserIndex: when r.Users(ctx) returns
		// source.ErrNotFound it builds st.NewUserIndex([]slack.User{}), a
		// non-nil empty map. Use that here so this pins the branch New
		// actually produces (userattr's "<external>:"+id path), not the
		// nil-index bare-id path.
		cc := initChannels([]slack.Channel{
			testChannel("D1", "", asIM, withUser("U2")),
			testChannel("D2", "", asIM, withUser("U1")),
		}, st.NewUserIndex(nil))
		if got := channelIDs(cc.DM); !slices.Equal(got, []string{"D2", "D1"}) {
			t.Errorf("initChannels() = %v, want [D2 D1]", got)
		}
	})
}
