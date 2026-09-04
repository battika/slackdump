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

func Test_escapeLike(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is unchanged", "login", "login"},
		{"percent is escaped", "100%", `100\%`},
		{"underscore is escaped", "a_b", `a\_b`},
		{"backslash is escaped", `a\b`, `a\\b`},
		{"all three", `%_\`, `\%\_\\`},
		{"empty", "", ""},
		{"unicode is untouched", "áéíóú", "áéíóú"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := escapeLike(tt.in); got != tt.want {
				t.Errorf("escapeLike() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_slackdumpLower(t *testing.T) {
	conn := testConn(t)
	tests := []struct {
		name string
		expr string
		want int
	}{
		{"ascii folds", `slackdump_lower('SLACKDUMP') = 'slackdump'`, 1},
		{"accented folds", `slackdump_lower('ÁÉÍÓÚ') = 'áéíóú'`, 1},
		{"plain LIKE does not fold accents (why this exists)", `'áéíóú' LIKE '%ÁÉÍÓÚ%'`, 0},
		{"null is tolerated", `slackdump_lower(NULL) IS NULL OR slackdump_lower(NULL) = ''`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got int
			if err := conn.QueryRowxContext(t.Context(), "SELECT "+tt.expr).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tt.want {
				t.Errorf("%s = %d, want %d", tt.expr, got, tt.want)
			}
		})
	}
}
