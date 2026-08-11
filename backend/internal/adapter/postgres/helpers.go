package postgres

import "strconv"

// Query-building helpers shared by the stores.

// defaultPageLimit and maxPageLimit bound a page.
//
// The maximum is a denial-of-service control as much as a performance one: without
// it a client can ask for every row a user has ever produced in one request, and
// session_logs makes that an unbounded amount of data.
const (
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// clampLimit normalises a caller-supplied page size.
//
// Clamps rather than rejecting: a client asking for 10,000 rows is better served
// 500 than an error, and an unspecified limit should behave sensibly rather than
// returning nothing.
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultPageLimit
	case n > maxPageLimit:
		return maxPageLimit
	default:
		return n
	}
}

// itoa renders a positional-parameter index, e.g. "$3".
func itoa(n int) string { return strconv.Itoa(n) }

// nonNilStrings coerces a nil slice to an empty one.
//
// A nil Go slice marshals to SQL NULL, and every TEXT[] column in this schema is
// NOT NULL. That distinction is not one the domain makes — an absent capability
// list and an empty one both mean "supports nothing" — so it is flattened here at
// the boundary rather than requiring every caller to remember.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nullZero maps 0 to SQL NULL for columns where zero is not a real value.
//
// A PID of 0 means "no process", which is semantically NULL. Storing 0 would make
// it indistinguishable from a genuine PID on a system where 0 is valid.
func nullZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
