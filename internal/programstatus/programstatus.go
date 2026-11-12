// Package programstatus builds OSC 7501 Program Status reports, which terminals and multiplexers
// such as Herdr read to show whether a program is working or waiting on the user.
package programstatus

import (
	"encoding/base64"
	"strings"
	"unicode"
)

// Clear removes every status record this program left on the terminal.
const Clear = "\x1b]7501;state=clear\x1b\\"

// Report returns the root-record report for state (idle, working, done or blocked). kind applies
// to blocked only; msg is shown as one line.
func Report(state, kind, msg string) string {
	var b strings.Builder
	b.WriteString("\x1b]7501;state=" + state + ":app=alux")
	if kind != "" {
		b.WriteString(":kind=" + kind)
	}
	if msg = oneLine(msg); msg != "" {
		b.WriteString(":msg=" + base64.StdEncoding.EncodeToString([]byte(msg)))
	}
	b.WriteString("\x1b\\")
	return b.String()
}

// oneLine replaces control characters, since a terminal discards a whole report whose msg has one.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}
