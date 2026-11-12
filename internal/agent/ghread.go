package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// The only gh reads a poll may run unsandboxed and unscreened: numbers and a repo slug are all that leave the machine.
var ghReads = []*regexp.Regexp{
	regexp.MustCompile(`^gh (pr checks [0-9]+ --repo [A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+( --required)?)$`),
	regexp.MustCompile(`^gh (run view [0-9]+ --log-failed --repo [A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)$`),
}

var (
	ghPinOnce sync.Once
	ghPinned  string
	// ghLookup finds the gh to pin; tests replace it.
	ghLookup = func() string { return pinGh(os.Getenv("PATH")) }
)

// GhRead is command rewritten to run the pinned gh, when it is one of the exact gh reads and a gh is pinned.
func GhRead(command string) (string, bool) {
	command = strings.TrimSpace(command)
	for _, re := range ghReads {
		if m := re.FindStringSubmatch(command); m != nil {
			ghPinOnce.Do(func() { ghPinned = ghLookup() })
			if ghPinned == "" {
				return "", false
			}
			return "'" + ghPinned + "' " + m[1], true
		}
	}
	return "", false
}

// pinGh is the first gh on path outside mise, whose shims pick a binary from the working directory's config.
func pinGh(path string) string {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) || strings.Contains(dir, "/mise/") || strings.Contains(dir, "'") {
			continue
		}
		bin := filepath.Join(dir, "gh")
		real, err := filepath.EvalSymlinks(bin)
		if err != nil || strings.Contains(real, "/mise/") {
			continue
		}
		if info, err := os.Stat(real); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return bin
		}
	}
	return ""
}

// ghReadPoll reports a poll outside the sandbox whose command is one of the exact gh reads.
func ghReadPoll(call Call) bool {
	if call.Name != pollTool || !escalates(call) {
		return false
	}
	_, ok := GhRead(commandOf(call))
	return ok
}
