// Package netfail tells text that reports the network failing from text that reports an answer or a refusal.
package netfail

import "strings"

var markers = []string{
	"could not resolve host",
	"dial tcp",
	"connection refused",
	"connection reset",
	"network is unreachable",
	"i/o timeout",
	"tls handshake timeout",
	"context deadline exceeded",
	"error connecting to api.github.com",
	"operation timed out",
	"no route to host",
	"temporary failure in name resolution",
	"unexpected eof",
	"broken pipe",
}

// Transient reports whether text, an error or a command's output, reads as the network failing rather than a remote answering or refusing.
func Transient(text string) bool { return Line(text) != "" }

// Line is the first line of text naming a network failure, or "" when none does.
func Line(text string) string {
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		// A local server that is still starting refuses connections; that is an answer, not the network failing.
		if strings.Contains(lower, "connection refused") && local(lower) {
			continue
		}
		for _, m := range markers {
			if strings.Contains(lower, m) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

func local(lower string) bool {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "::1", "0.0.0.0"} {
		if strings.Contains(lower, host) {
			return true
		}
	}
	return false
}
