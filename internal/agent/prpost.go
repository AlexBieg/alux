package agent

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A refusal list, so a script file or a query read from disk evades it; it catches the spellings agents reach for.
const prPostRefusal = "agents do not post to a pull request through a shell; draft review comments with " +
	"pr_pending_review, reply to or resolve a thread with pr_thread_reply, open a draft " +
	"PR with pr_create, change one or mark it ready with pr_edit, and read reviews and threads with pr_detail"

var (
	// Reads of these paths are refused too; pr_detail answers those.
	prPostPath     = regexp.MustCompile(`(^|/)(pulls/[^/\s]+/(reviews|comments)|issues/[^/\s]+/comments)\b`)
	prPostMutation = regexp.MustCompile(`(?i)\b(add|submit|update)PullRequestReview(Comment|Thread)?\b|\baddComment\b|\b(un)?resolveReviewThread\b`)
	toGitHub       = regexp.MustCompile(`\bgh\b|api\.github\.com`)
	prVerb         = regexp.MustCompile(`\bgh\b.*\bpr\s+(review|comment|create|new|edit|ready)\b`)
	prWriteVerbs   = []string{"review", "comment", "create", "new", "edit", "ready"}
)

// postsToPullRequest returns why a shell call must not run, or "" when it may.
func postsToPullRequest(in Intent, raw string) string {
	if in.Class != ClassBash {
		return ""
	}
	if in.Opaque != "" {
		// A false refusal costs a rephrasing; a miss costs a posted review.
		if toGitHub.MatchString(raw) && (prVerb.MatchString(raw) || prPostPath.MatchString(raw) || prPostMutation.MatchString(raw)) {
			return prPostRefusal
		}
		return ""
	}
	for _, words := range in.Commands {
		if commandPostsToPullRequest(words) {
			return prPostRefusal
		}
	}
	return ""
}

func commandPostsToPullRequest(words []string) bool {
	switch filepath.Base(words[0]) {
	case "gh":
		args := words[1:]
		if i := slices.Index(args, "pr"); i >= 0 && i+1 < len(args) && slices.Contains(prWriteVerbs, args[i+1]) {
			return true
		}
		if slices.Contains(args, "api") {
			return anyWord(args, prPostPath, prPostMutation)
		}
	case "curl", "wget", "http", "https", "xh":
		return slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, "api.github.com") }) &&
			anyWord(words[1:], prPostPath, prPostMutation)
	}
	return false
}

func anyWord(words []string, patterns ...*regexp.Regexp) bool {
	for _, w := range words {
		for _, p := range patterns {
			if p.MatchString(w) {
				return true
			}
		}
	}
	return false
}
