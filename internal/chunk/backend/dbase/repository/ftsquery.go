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

import "strings"

// ftsQuery converts user input into an FTS5 MATCH expression that cannot raise
// a syntax error.  FTS5 treats ", *, (, ), :, ^ and the words AND/OR/NOT as
// syntax, and malformed input fails the *query* rather than returning nothing,
// so every token is wrapped in double quotes with internal quotes doubled.
// Quoted tokens are literals, and FTS5 ANDs adjacent ones — which is what makes
// a two-word search find messages containing both.
//
// A trailing "*" is kept outside the quotes so prefix search stays reachable:
// that is what recovers stem matching, the one thing LIKE does better.
//
// An empty or whitespace-only input yields "", as does input that is nothing
// but NULs; callers must treat "" as "no query" rather than passing it to
// MATCH.
func ftsQuery(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// A NUL truncates the SQL text before the parser sees it, leaving the
		// opening quote unclosed and turning a search into a syntax error.
		// Strip them; a token that was nothing else is dropped entirely.
		f = strings.ReplaceAll(f, "\x00", "")
		if f == "" {
			continue
		}
		prefix := ""
		if strings.HasSuffix(f, "*") && len(f) > 1 {
			f = strings.TrimSuffix(f, "*")
			prefix = "*"
		}
		out = append(out, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`+prefix)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ")
}
