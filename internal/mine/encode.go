package mine

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

const editTool = "str_replace_based_edit_tool"

var dropped = map[string]bool{
	editTool: true, "grep": true, "glob": true,
	"start_skill": true, "finish_skill": true, "fail_run": true,
}

var noise = map[string]bool{"cd": true, "export": true, "echo": true, "printf": true, "true": true, "set": true, "sleep": true, ":": true}

var readOnly = map[string]bool{
	"grep": true, "rg": true, "ls": true, "cat": true, "head": true, "tail": true, "sed": true, "awk": true,
	"wc": true, "jq": true, "find": true, "fd": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"du": true, "stat": true, "file": true, "which": true, "diff": true, "tree": true, "less": true,
	"date": true, "pwd": true, "basename": true, "dirname": true, "realpath": true, "xargs": true,
	"test": true, "[": true,
}

var withSubcommand = map[string]bool{"jj": true, "go": true, "git": true, "gh": true, "npm": true, "pnpm": true, "make": true, "docker": true, "uv": true, "poetry": true}

var keywords = map[string]bool{
	"for": true, "while": true, "until": true, "do": true, "done": true, "if": true, "then": true,
	"else": true, "elif": true, "fi": true, "in": true, "case": true, "esac": true, "!": true,
}

var (
	verbShape       = regexp.MustCompile(`^[\w.+-]+$`)
	subcommandShape = regexp.MustCompile(`^[a-z][\w-]*$`)
)

// Encode turns one tool call into the symbols the miner sees; most calls become one symbol, some none.
func Encode(name string, input json.RawMessage) []string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	cmd := in.Command
	if name == editTool && cmd == "create" {
		return []string{"create_file"}
	}
	if dropped[name] {
		return nil
	}
	if name == "bash" {
		return encodeBash(cmd)
	}
	return []string{name}
}

func encodeBash(command string) []string {
	var out []string
	for _, words := range commands(command) {
		if sym := verbSymbol(words); sym != "" {
			out = append(out, sym)
		}
	}
	return out
}

func verbSymbol(words []string) string {
	if len(words) > 0 && (words[0] == "for" || words[0] == "case" || words[0] == "select") {
		return ""
	}
	for len(words) > 0 && (isAssignment(words[0]) || keywords[words[0]]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	verb := filepath.Base(words[0])
	if noise[verb] || readOnly[verb] || !verbShape.MatchString(verb) {
		return ""
	}
	if strings.HasPrefix(verb, "python") {
		return "python"
	}
	if withSubcommand[verb] && len(words) > 1 && subcommandShape.MatchString(words[1]) {
		return verb + "_" + words[1]
	}
	return verb
}

func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return false
	}
	for i, c := range word[:eq] {
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// commands splits a command line into the commands it runs, each as its words.
// Unlike the policy lexer it never refuses: anything it cannot read is skipped, not fatal.
func commands(command string) [][]string {
	var (
		out       [][]string
		cur       []string
		word      strings.Builder
		started   bool
		afterPipe bool
		heredocs  []string
	)
	flushWord := func() {
		if started {
			cur = append(cur, word.String())
			word.Reset()
			started = false
		}
	}
	endCommand := func(pipe bool) {
		flushWord()
		if len(cur) > 0 && !afterPipe {
			out = append(out, cur)
		}
		cur = nil
		afterPipe = pipe
	}

	r := []rune(command)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r):
			i++
			if r[i] != '\n' {
				word.WriteRune(r[i])
				started = true
			}
		case c == '\'':
			end := indexFrom(r, i+1, '\'')
			word.WriteString(string(r[i+1 : end]))
			started = true
			i = end
		case c == '"':
			end := closingDouble(r, i+1)
			word.WriteString(string(r[i+1 : end]))
			started = true
			i = end
		case c == '$' && i+1 < len(r) && r[i+1] == '(', c == '`':
			i = skipSubstitution(r, i)
			started = true
		case c == '<' && i+1 < len(r) && r[i+1] == '<':
			i, heredocs = readHeredocMarker(r, i, heredocs)
		case c == '>' || c == '<':
			if started && allDigits(word.String()) {
				word.Reset()
				started = false
			}
			flushWord()
			i = skipRedirectTarget(r, i)
		case c == '&' && i+1 < len(r) && r[i+1] == '>':
			flushWord()
			i = skipRedirectTarget(r, i+1)
		case c == '#' && !started:
			i = indexFrom(r, i, '\n') - 1
		case c == '\n':
			endCommand(false)
			if len(heredocs) > 0 {
				i = skipHeredocBodies(r, i+1, heredocs) - 1
				heredocs = nil
			}
		case c == '|':
			pipe := !(i+1 < len(r) && r[i+1] == '|')
			if !pipe {
				i++
			}
			endCommand(pipe)
		case c == '&' || c == ';':
			if i+1 < len(r) && r[i+1] == c {
				i++
			}
			endCommand(false)
		case c == '(' || c == ')' || c == '{' || c == '}':
			endCommand(false)
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
		default:
			word.WriteRune(c)
			started = true
		}
	}
	endCommand(false)
	return out
}

func indexFrom(r []rune, i int, c rune) int {
	for i < len(r) && r[i] != c {
		i++
	}
	return min(i, len(r))
}

func closingDouble(r []rune, i int) int {
	for i < len(r) && r[i] != '"' {
		if r[i] == '\\' {
			i++
		}
		i++
	}
	return min(i, len(r))
}

// skipSubstitution returns the index of the rune closing a $(…) or `…` starting at i.
func skipSubstitution(r []rune, i int) int {
	if r[i] == '`' {
		return indexFrom(r, i+1, '`')
	}
	depth := 0
	for j := i + 1; j < len(r); j++ {
		switch r[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(r)
}

func skipRedirectTarget(r []rune, i int) int {
	for i+1 < len(r) && (r[i+1] == '>' || r[i+1] == '<' || r[i+1] == '&' || r[i+1] == '|') {
		i++
	}
	for i+1 < len(r) && (r[i+1] == ' ' || r[i+1] == '\t') {
		i++
	}
	for i+1 < len(r) && !strings.ContainsRune(" \t\r\n;&|()", r[i+1]) {
		i++
	}
	return i
}

func readHeredocMarker(r []rune, i int, markers []string) (int, []string) {
	i += 2
	if i < len(r) && r[i] == '-' {
		i++
	}
	for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
		i++
	}
	var m strings.Builder
	for i < len(r) && !strings.ContainsRune(" \t\r\n;&|()<>", r[i]) {
		if r[i] != '\'' && r[i] != '"' {
			m.WriteRune(r[i])
		}
		i++
	}
	if m.Len() > 0 {
		markers = append(markers, m.String())
	}
	return i - 1, markers
}

// skipHeredocBodies returns the index just past the line ending the last heredoc body that starts at i.
func skipHeredocBodies(r []rune, i int, markers []string) int {
	for _, m := range markers {
		for i < len(r) {
			end := indexFrom(r, i, '\n')
			line := strings.TrimSpace(string(r[i:end]))
			i = end + 1
			if line == m {
				break
			}
		}
	}
	return min(i, len(r))
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
