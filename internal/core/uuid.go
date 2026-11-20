package core

// IsUUID reports whether s is a canonical, hyphenated UUID string
// (8-4-4-4-12 hex digits, either case). Services check ids with it before
// passing them to a uuid column, so a malformed id from a URL answers
// ErrNotFound instead of a PostgreSQL cast error.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
