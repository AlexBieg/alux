package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

// fileCall is one alux tool call that does the work of part of a bash read.
type fileCall struct {
	tool   string
	input  map[string]any
	target string
}

// word is one shell word with its quotes removed; quoted records whether any of it was quoted.
type word struct {
	text   string
	quoted bool
}

// shellPieces splits a command on ; && and newlines into pipelines, refusing what no plain read needs: expansions, redirects, globs.
func shellPieces(command string) ([][][]word, bool) {
	var pieces [][][]word
	var stages [][]word
	var words []word
	var cur strings.Builder
	inWord, quoted := false, false
	var quote rune
	endWord := func() {
		if inWord {
			words = append(words, word{cur.String(), quoted})
		}
		cur.Reset()
		inWord, quoted = false, false
	}
	endStage := func() bool {
		endWord()
		if len(words) == 0 {
			return false
		}
		stages = append(stages, words)
		words = nil
		return true
	}
	endPiece := func() bool {
		if len(words) == 0 && !inWord && len(stages) == 0 {
			return true
		}
		if !endStage() {
			return false
		}
		pieces = append(pieces, stages)
		stages = nil
		return true
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '$' || c == '`':
				return nil, false
			case c == '\\' && i+1 < len(runes) && strings.ContainsRune(`"\`, runes[i+1]):
				i++
				cur.WriteRune(runes[i])
			default:
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord, quoted = c, true, true
		case c == '\\':
			if i+1 >= len(runes) || runes[i+1] == '\n' {
				return nil, false
			}
			i++
			cur.WriteRune(runes[i])
			inWord, quoted = true, true
		case c == ' ' || c == '\t':
			endWord()
		case c == '\n' || c == ';':
			if !endPiece() {
				return nil, false
			}
		case c == '&':
			if i+1 >= len(runes) || runes[i+1] != '&' || !endPiece() {
				return nil, false
			}
			i++
		case c == '|':
			if i+1 < len(runes) && runes[i+1] == '|' || !endStage() {
				return nil, false
			}
		case strings.ContainsRune("$`<>*?[](){}!#", c):
			return nil, false
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 || !endPiece() {
		return nil, false
	}
	return pieces, len(pieces) > 0
}

var (
	sedRange = regexp.MustCompile(`^(\d+)(?:,(\d+))?p$`)
	count    = regexp.MustCompile(`^\d+$`)
)

// readShape reports the alux tool calls that do the same reading as command, or nil when any part of it is not a plain read.
func readShape(command, dir string) []fileCall {
	pieces, ok := shellPieces(command)
	if !ok {
		return nil
	}
	var calls []fileCall
	for i, stages := range pieces {
		args := stages[0]
		name := args[0].text
		if args[0].quoted {
			return nil
		}
		if name == "cd" && i == 0 && len(args) == 2 && len(stages) == 1 {
			dir = readPath(dir, args[1].text)
			continue
		}
		if (name == "grep" || name == "rg") && len(stages) <= 2 {
			limit := 0
			if len(stages) == 2 {
				if limit = headCount(stages[1]); limit == 0 {
					return nil
				}
			}
			got := grepCalls(name, args[1:], dir, limit)
			if got == nil {
				return nil
			}
			calls = append(calls, got...)
			continue
		}
		if len(stages) != 1 {
			return nil
		}
		var got []fileCall
		switch name {
		case "echo":
			continue
		case "cat":
			got = catCalls(args[1:], dir)
		case "sed":
			got = sedCalls(args[1:], dir)
		case "head", "tail":
			got = headTailCalls(name, args[1:], dir)
		case "ls":
			got = lsCalls(args[1:], dir)
		}
		if got == nil {
			return nil
		}
		calls = append(calls, got...)
	}
	return calls
}

func readPath(dir, path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

func view(path string, lines ...int) fileCall {
	in := map[string]any{"command": "view", "path": path}
	if len(lines) == 2 {
		in["view_range"] = []int{lines[0], lines[1]}
	}
	return fileCall{tool: editorTool, input: in, target: path}
}

func flagged(args []word) bool {
	for _, a := range args {
		if strings.HasPrefix(a.text, "-") {
			return true
		}
	}
	return false
}

func catCalls(args []word, dir string) []fileCall {
	if len(args) == 0 || flagged(args) {
		return nil
	}
	var calls []fileCall
	for _, a := range args {
		calls = append(calls, view(readPath(dir, a.text)))
	}
	return calls
}

func sedCalls(args []word, dir string) []fileCall {
	if len(args) != 3 || args[0].text != "-n" {
		return nil
	}
	m := sedRange.FindStringSubmatch(args[1].text)
	if m == nil || strings.HasPrefix(args[2].text, "-") {
		return nil
	}
	from, _ := strconv.Atoi(m[1])
	to := from
	if m[2] != "" {
		to, _ = strconv.Atoi(m[2])
	}
	if from < 1 || to < from {
		return nil
	}
	return []fileCall{view(readPath(dir, args[2].text), from, to)}
}

// lineCount parses head and tail's -n N, -nN and -N; from is tail's -n +N.
func lineCount(args []word) (n int, from bool, rest []word, ok bool) {
	n = 10
	for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
		a := args[0].text
		var v string
		switch {
		case a == "-n" && len(args) > 1:
			v, args = args[1].text, args[2:]
		case strings.HasPrefix(a, "-n"):
			v, args = a[2:], args[1:]
		case count.MatchString(a[1:]):
			v, args = a[1:], args[1:]
		default:
			return 0, false, nil, false
		}
		from = strings.HasPrefix(v, "+")
		v = strings.TrimPrefix(v, "+")
		if !count.MatchString(v) {
			return 0, false, nil, false
		}
		n, _ = strconv.Atoi(v)
	}
	return n, from, args, n > 0
}

func headCount(stage []word) int {
	if stage[0].text != "head" || stage[0].quoted {
		return 0
	}
	n, from, rest, ok := lineCount(stage[1:])
	if !ok || from || len(rest) != 0 {
		return 0
	}
	return n
}

func headTailCalls(name string, args []word, dir string) []fileCall {
	n, from, files, ok := lineCount(args)
	if !ok || len(files) != 1 || flagged(files) || from && name == "head" {
		return nil
	}
	path := readPath(dir, files[0].text)
	switch {
	case name == "head":
		return []fileCall{view(path, 1, n)}
	case from:
		return []fileCall{view(path, n, -1)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	total := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	return []fileCall{view(path, max(1, total-n+1), -1)}
}

func lsCalls(args []word, dir string) []fileCall {
	if flagged(args) {
		return nil
	}
	if len(args) == 0 {
		return []fileCall{view(dir)}
	}
	var calls []fileCall
	for _, a := range args {
		calls = append(calls, view(readPath(dir, a.text)))
	}
	return calls
}

// grepCalls carries a grep or rg's flags across to the grep tool one to one, and refuses any flag the tool cannot honour.
func grepCalls(name string, args []word, dir string, limit int) []fileCall {
	in := map[string]any{}
	var pattern *string
	var paths []string
	takes := func(i *int) (string, bool) {
		if *i+1 >= len(args) {
			return "", false
		}
		*i++
		return args[*i].text, true
	}
	setCount := func(key, v string) bool {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return false
		}
		in[key] = n
		return true
	}
	mode := func(m string) bool {
		if prev, set := in["output_mode"]; set && prev != m {
			return false
		}
		in["output_mode"] = m
		return true
	}
	flagsDone := false
	for i := 0; i < len(args); i++ {
		a := args[i].text
		if flagsDone || !strings.HasPrefix(a, "-") || a == "-" {
			if pattern == nil {
				p := a
				pattern = &p
			} else {
				paths = append(paths, a)
			}
			continue
		}
		if a == "--" {
			flagsDone = true
			continue
		}
		if long, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "="); strings.HasPrefix(a, "--") {
			switch {
			case long == "include" && hasVal && name == "grep", long == "glob" && hasVal && name == "rg":
				in["glob"] = val
			case long == "type" && hasVal && name == "rg":
				in["type"] = val
			case long == "files-with-matches" && !hasVal:
				if !mode(tools.ModeFiles) {
					return nil
				}
			case long == "files-without-match" && !hasVal:
				if !mode(tools.ModeFilesWithout) {
					return nil
				}
			case long == "count" && !hasVal:
				if !mode(tools.ModeCount) {
					return nil
				}
			case long == "ignore-case" && !hasVal:
				in["-i"] = true
			case (long == "line-number" || long == "recursive" || long == "extended-regexp") && !hasVal:
			default:
				return nil
			}
			continue
		}
		short := a[1:]
		for j := 0; j < len(short); j++ {
			c := short[j]
			switch {
			case strings.IndexByte("nrRHE", c) >= 0:
			case c == 'i':
				in["-i"] = true
			case c == 'o':
				in["-o"] = true
			case c == 'U' && name == "rg":
				in["multiline"] = true
			case c == 'l':
				if !mode(tools.ModeFiles) {
					return nil
				}
			case c == 'L':
				if !mode(tools.ModeFilesWithout) {
					return nil
				}
			case c == 'c':
				if !mode(tools.ModeCount) {
					return nil
				}
			case strings.IndexByte("ABCetg", c) >= 0:
				if (c == 't' || c == 'g') && name != "rg" {
					return nil
				}
				v := short[j+1:]
				if v == "" {
					var ok bool
					if v, ok = takes(&i); !ok {
						return nil
					}
				}
				switch c {
				case 'e':
					if pattern != nil {
						return nil
					}
					pattern = &v
				case 't':
					in["type"] = v
				case 'g':
					in["glob"] = v
				default:
					if !setCount("-"+string(c), v) {
						return nil
					}
				}
				j = len(short)
			default:
				return nil
			}
		}
	}
	if pattern == nil || *pattern == "" {
		return nil
	}
	in["pattern"] = *pattern
	if limit > 0 {
		in["head_limit"] = limit
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	var calls []fileCall
	for _, p := range paths {
		call := map[string]any{}
		for k, v := range in {
			call[k] = v
		}
		path := readPath(dir, p)
		call["path"] = path
		calls = append(calls, fileCall{tool: "grep", input: call, target: path})
	}
	return calls
}

// gnuRegex marks a pattern written for GNU grep's basic syntax, which Rust regex reads differently.
var gnuRegex = regexp.MustCompile(`\\[|(){}+?]`)

// holdRead holds a bash command that only reads files, naming the view and grep calls that do the same reading.
func (a *Agent) holdRead(call Call, cb Callbacks, in Intent, dir string) *anthropic.ContentBlockParamUnion {
	if call.Name != bashTool || cb.suggestions == nil {
		return nil
	}
	command := commandOf(call)
	calls := readShape(command, dir)
	if len(calls) == 0 {
		return nil
	}
	var names, targets, lines []string
	gnu := false
	for _, c := range calls {
		if !a.has(c.tool) {
			return nil
		}
		menu := c.tool
		if menu == editorTool {
			menu = "view"
		}
		if !slices.Contains(names, menu) {
			names = append(names, menu)
		}
		targets = append(targets, c.target)
		input, _ := json.Marshal(c.input)
		lines = append(lines, "- "+c.tool+" "+string(input))
		if p, ok := c.input["pattern"].(string); ok && gnuRegex.MatchString(p) {
			gnu = true
		}
	}
	reason := "it only reads files, which alux's own tools do without the shell"
	id := cb.suggestions.add(pendingSuggestion{
		command: command, label: in.Label, body: in.Body, reason: reason, call: call, targets: targets,
		suggestion: guardian.Suggestion{Tools: names, Source: guardian.SourceRule},
	})
	text := "Not run: " + reason + ". Make these calls instead; independent ones can go in parallel:\n" + strings.Join(lines, "\n")
	if gnu {
		text += "\nThe grep pattern is copied as you wrote it, but the grep tool reads Rust regex: GNU's \\| \\( \\) are | ( ) there."
	}
	text += "\nIf they cannot do what you need, call decline_suggestion with id \"" + id + "\" and a reason, which puts this " +
		"exact command to the user to approve (or refuses it when nobody can be asked); or do neither and carry on another way."
	return block(anthropic.NewToolResultBlock(call.ID, text, true))
}
