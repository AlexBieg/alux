// Package desktop sends macOS notifications for this session's own transitions.
package desktop

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const maxBodyChars = 180

// Any basename from /System/Library/Sounds; empty for a silent notification.
//
// Two macOS behaviours worth knowing, both of which fail quietly like the rest of this path: an
// unknown sound name is not an error, it simply plays nothing, and the sound obeys System
// Settings → Notifications → Script Editor, since that is the app osascript delivers as.
const sound = "Glass"

// AppleScript string literals cannot hold raw newlines, and quotes and backslashes need
// escaping.
func quote(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, `"`, `\"`)
	return `"` + text + `"`
}

// TerminalInFront reports whether the app this process runs in is frontmost, and true when it
// cannot tell. It blocks for a few hundred milliseconds.
func TerminalInFront() bool {
	own := os.Getenv("__CFBundleIdentifier")
	if runtime.GOOS != "darwin" || own == "" {
		return true
	}
	asn, err := exec.Command("lsappinfo", "front").Output()
	if err != nil {
		return true
	}
	out, err := exec.Command("lsappinfo", "info", "-only", "bundleid", strings.TrimSpace(string(asn))).Output()
	if err != nil {
		return true
	}
	front := bundleID(string(out))
	return front == "" || front == own
}

// bundleID reads lsappinfo's `"CFBundleIdentifier"="com.example.app"` line.
func bundleID(out string) string {
	_, value, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok {
		return ""
	}
	return strings.Trim(value, `"`)
}

// Notify is fire and forget: a notification is never worth blocking a turn or surfacing an
// error for.
func Notify(title, body string) {
	if runtime.GOOS != "darwin" {
		return
	}
	if len(body) > maxBodyChars {
		body = body[:maxBodyChars]
	}
	script := "display notification " + quote(body) + " with title " + quote(title)
	if sound != "" {
		script += " sound name " + quote(sound)
	}
	cmd := exec.Command("osascript", "-e", script)
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}
