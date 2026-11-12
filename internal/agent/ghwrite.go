package agent

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// GitHub writes no harness tool covers; guardian reads them as reads or records, so they go to the person by name.
var ghWriteVerbs = map[string][]string{
	"pr":       {"merge", "close", "reopen", "lock", "unlock", "update-branch", "revert"},
	"run":      {"rerun", "cancel", "delete"},
	"workflow": {"run", "enable", "disable"},
	"release":  {"create", "edit", "delete", "upload", "delete-asset"},
	"issue":    {"create", "new", "comment", "edit", "close", "reopen", "delete", "lock", "unlock", "transfer", "pin", "unpin"},
	"label":    {"create", "edit", "delete", "clone"},
	"secret":   {"set", "delete"},
	"variable": {"set", "delete"},
	"cache":    {"delete"},
}

var (
	ghWriteMethod = regexp.MustCompile(`(?i)^(POST|PATCH|PUT|DELETE)$`)
	ghWriteLine   = regexp.MustCompile(ghWriteLinePattern())
	ghAPIWrite    = regexp.MustCompile(`(?i)\bgh\b.*\bapi\b.*((-X\s*|--method[=\s]+)(POST|PATCH|PUT|DELETE)\b|\bmutation\b)`)
)

func ghWriteLinePattern() string {
	var alts []string
	for group, verbs := range ghWriteVerbs {
		alts = append(alts, group+`\s+(`+strings.Join(verbs, "|")+`)`)
	}
	slices.Sort(alts)
	return `\bgh\b.*\b(` + strings.Join(alts, "|") + `)\b`
}

// writesToGitHub returns why a shell call must go to the person, or "" when it need not.
func writesToGitHub(in Intent, raw string) string {
	if in.Class != ClassBash {
		return ""
	}
	if in.Opaque != "" {
		if ghWriteLine.MatchString(raw) || ghAPIWrite.MatchString(raw) {
			return ghWriteReason("gh")
		}
		return ""
	}
	for _, words := range in.Commands {
		if what := commandWritesToGitHub(words); what != "" {
			return ghWriteReason(what)
		}
	}
	return ""
}

func ghWriteReason(what string) string {
	return "it writes to GitHub (" + what + "), which always needs the person's say-so"
}

func commandWritesToGitHub(words []string) string {
	if filepath.Base(words[0]) != "gh" {
		return ""
	}
	args := ghPositional(words[1:])
	if len(args) == 0 {
		return ""
	}
	if args[0] == "api" {
		if apiWrites(words[1:]) {
			return "gh api write"
		}
		return ""
	}
	if len(args) > 1 && slices.Contains(ghWriteVerbs[args[0]], args[1]) {
		return "gh " + args[0] + " " + args[1]
	}
	return ""
}

// ghPositional drops flags, and the repo or host a global flag names, so gh -R o/r run rerun reads as run rerun.
func ghPositional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-R" || a == "--repo" || a == "--hostname":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			out = append(out, a)
		}
	}
	return out
}

// apiWrites follows gh api: a write method, or fields with no method (sent as POST); for graphql only a mutation.
func apiWrites(args []string) bool {
	method, fields, graphql, mutation := "", false, false, false
	for i, a := range args {
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = args[i+1]
			}
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			method = a[2:]
		case strings.HasPrefix(a, "--method="):
			method = strings.TrimPrefix(a, "--method=")
		case a == "-f" || a == "-F" || a == "--field" || a == "--raw-field" || a == "--input" ||
			strings.HasPrefix(a, "--field=") || strings.HasPrefix(a, "--raw-field=") || strings.HasPrefix(a, "--input="):
			fields = true
		case a == "graphql":
			graphql = true
		}
		if strings.Contains(a, "mutation") {
			mutation = true
		}
	}
	switch {
	case graphql:
		return mutation
	case method != "":
		return ghWriteMethod.MatchString(method)
	}
	return fields
}
