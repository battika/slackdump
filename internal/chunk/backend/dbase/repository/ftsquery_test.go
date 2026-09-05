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

package repository

import "testing"

func Test_ftsQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single term", "login", `"login"`},
		{"two terms become implicit AND", "login hint", `"login" "hint"`},
		{"colon is neutralised", "login:", `"login:"`},
		{"embedded quote is doubled", `a"b`, `"a""b"`},
		{"trailing star is preserved for prefix search", "kosz*", `"kosz"*`},
		{"star only on the last token", "a* b*", `"a"* "b"*`},
		{"operators are literals, not syntax", "a AND b", `"a" "AND" "b"`},
		{"parens are literals", "(a)", `"(a)"`},
		{"collapses whitespace", "  a   b  ", `"a" "b"`},
		{"empty", "", ""},
		{"only punctuation still yields a term", "!!!", `"!!!"`},
		{"unicode is untouched", "áéí", `"áéí"`},
		// A NUL would truncate the SQL text and leave the quote unclosed.
		{"nul is stripped", "a\x00b", `"ab"`},
		{"a token of only nul is dropped", "\x00", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ftsQuery(tt.in); got != tt.want {
				t.Errorf("ftsQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
