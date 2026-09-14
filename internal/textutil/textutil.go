// Package textutil holds the tiny string helpers every package used to
// copy: one shared home keeps the copies from drifting apart.
package textutil

import "strings"

// Truncate caps s at n bytes, marking the cut so a bounded payload or
// notification never reads as complete.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// FirstLine keeps multi-line command output to one actionable line for
// error messages; empty output reports a bare failure instead of "".
func FirstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" {
		return "command failed"
	}
	return line
}
