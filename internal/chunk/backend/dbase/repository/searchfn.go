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

import (
	"database/sql/driver"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite"
)

// lowerFn is the SQL name of the Unicode-aware lowercase function registered
// below.  SQLite's own LOWER() and LIKE fold ASCII only, so an accented word
// typed in a different case never matches — a real defect for any archive that
// is not purely English.  Go's strings.ToLower is fully Unicode-aware.
//
// The name is deliberately namespaced: registration is process-global, so a
// generic name could collide with another modernc.org/sqlite user in the same
// binary.
const lowerFn = "slackdump_lower"

func init() {
	err := sqlite.RegisterDeterministicScalarFunction(lowerFn, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, ok := args[0].(string)
			if !ok {
				// NULL and non-text values have no lowercase form; returning
				// an empty string keeps LIKE comparisons well-defined.
				return "", nil
			}
			return strings.ToLower(s), nil
		})
	if err != nil {
		panic(fmt.Sprintf("registering %s: %v", lowerFn, err))
	}
}

// escapeLike escapes the LIKE metacharacters in s so user input matches
// literally.  Without it a query of "%" matches every message in the archive.
// The result must be used with an ESCAPE '\' clause.
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
