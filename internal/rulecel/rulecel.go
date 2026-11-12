// Package rulecel is the CEL environment a hold rule's when condition is written in, and the functions it may call.
package rulecel

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// Call is the call a condition reads as call.
type Call struct {
	Tool     string
	Args     map[string]any
	Commands []string
	Path     string
}

// Session is what a condition reads as session; Agent is "" for the main agent. Home, Listed and Descendants are not
// readable: they back broadRoot, listed and descendants, and a nil one makes its function an error.
type Session struct {
	Cwd, Repo, Agent, Home string
	Listed                 func(rev string) bool
	Descendants            func(rev string) ([]string, error)
}

// Condition is a compiled when expression.
type Condition struct{ prg cel.Program }

var env = sync.OnceValues(newEnv)

// Compile parses and type-checks expr, refusing one that is not a bool.
func Compile(expr string) (*Condition, error) {
	e, err := env()
	if err != nil {
		return nil, err
	}
	ast, iss := e.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if !ast.OutputType().IsExactType(types.BoolType) {
		return nil, fmt.Errorf("the condition is a %s, not a bool", ast.OutputType())
	}
	prg, err := e.Program(ast)
	if err != nil {
		return nil, err
	}
	return &Condition{prg: prg}, nil
}

// Eval reports whether the condition holds for the call; a missing key or a wrong type is an error.
func (c *Condition) Eval(call Call, session Session) (bool, error) {
	holds, _, err := c.Explain(call, session)
	return holds, err
}

// Explain is Eval plus, when the condition holds, what broadRoot, descendants and listed found that made it true.
func (c *Condition) Explain(call Call, session Session) (bool, []string, error) {
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	commands := call.Commands
	if commands == nil {
		commands = []string{}
	}
	ev := &evidence{}
	act, err := cel.FunctionVars(map[string]any{
		"call":    map[string]any{"tool": call.Tool, "args": args, "commands": commands, "path": call.Path},
		"session": map[string]string{"cwd": session.Cwd, "repo": session.Repo, "agent": session.Agent},
	}, lateFunctions(session, ev))
	if err != nil {
		return false, nil, err
	}
	out, _, err := c.prg.Eval(act)
	if err != nil {
		return false, nil, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, nil, fmt.Errorf("the condition gave %v, not a bool", out.Value())
	}
	if !b {
		return false, nil, nil
	}
	return true, ev.lines(), nil
}

// evidence is what the session functions returned during one evaluation, in the order they were first asked.
type evidence struct {
	broad []string
	order []string
	revs  map[string]*revFacts
}

type revFacts struct {
	stacked                       int
	stackedChecked, asked, listed bool
}

func (e *evidence) rev(r string) *revFacts {
	if e.revs == nil {
		e.revs = map[string]*revFacts{}
	}
	f, ok := e.revs[r]
	if !ok {
		f = &revFacts{}
		e.revs[r] = f
		e.order = append(e.order, r)
	}
	return f
}

// lines leaves out a revision with nothing stacked on it or one an earlier jj log listed: neither made a hold true.
func (e *evidence) lines() []string {
	var out []string
	for _, p := range e.broad {
		out = append(out, p+" is a broad root")
	}
	for _, r := range e.order {
		f := e.revs[r]
		if (f.stackedChecked && f.stacked == 0) || (f.asked && f.listed) {
			continue
		}
		switch {
		case f.stackedChecked && f.asked:
			out = append(out, sitOn(f.stacked, r)+" and no earlier jj log in this conversation listed them")
		case f.stackedChecked:
			out = append(out, sitOn(f.stacked, r))
		case f.asked:
			out = append(out, "no earlier jj log in this conversation listed what sits on "+r)
		}
	}
	return out
}

func sitOn(n int, rev string) string {
	if n == 1 {
		return "1 change sits on " + rev
	}
	return fmt.Sprintf("%d changes sit on %s", n, rev)
}

func newEnv() (*cel.Env, error) {
	str := cel.StringType
	return cel.NewEnv(append(pureFunctions(),
		cel.Variable("call", cel.MapType(str, cel.DynType)),
		cel.Variable("session", cel.MapType(str, str)),
		cel.Function("broadRoot", cel.Overload("broadRoot_string", []*cel.Type{str}, cel.BoolType, cel.LateFunctionBinding())),
		cel.Function("listed", cel.Overload("listed_string", []*cel.Type{str}, cel.BoolType, cel.LateFunctionBinding())),
		cel.Function("descendants", cel.Overload("descendants_string", []*cel.Type{str}, cel.ListType(str), cel.LateFunctionBinding())),
	)...)
}

// pureFunctions are the functions that read only their arguments, which hold rules and skill evidence share.
func pureFunctions() []cel.EnvOption {
	str := cel.StringType
	return []cel.EnvOption{
		cel.Function("words", cel.Overload("words_string", []*cel.Type{str}, cel.IntType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				s, ok := v.Value().(string)
				if !ok {
					return types.NewErr("words takes a string, not %s", v.Type())
				}
				return types.Int(Words(s))
			}))),
		cel.Function("cmd", cel.Overload("cmd_string_string", []*cel.Type{str, str}, cel.BoolType,
			twoStrings(Cmd))),
		cel.Function("hasFlag", cel.Overload("hasFlag_string_string", []*cel.Type{str, str}, cel.BoolType,
			twoStrings(HasFlag))),
		cel.Function("positional", cel.Overload("positional_string", []*cel.Type{str}, cel.ListType(str),
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				s, ok := v.Value().(string)
				if !ok {
					return types.NewErr("positional takes a string, not %s", v.Type())
				}
				return types.NewStringList(types.DefaultTypeAdapter, Positional(s))
			}))),
		cel.Function("revs", cel.Overload("revs_string", []*cel.Type{str}, cel.ListType(str),
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				s, ok := v.Value().(string)
				if !ok {
					return types.NewErr("revs takes a string, not %s", v.Type())
				}
				return types.NewStringList(types.DefaultTypeAdapter, Revs(s))
			}))),
	}
}

// lateFunctions binds the functions that read the session; descendants runs once per revision per evaluation.
func lateFunctions(session Session, ev *evidence) map[string]cel.LateBoundFunction {
	seen := map[string]ref.Val{}
	return map[string]cel.LateBoundFunction{
		"broadRoot": oneString("broadRoot", func(s string) ref.Val {
			broad := BroadRoot(s, session.Cwd, session.Home)
			if broad && !slices.Contains(ev.broad, s) {
				ev.broad = append(ev.broad, s)
			}
			return types.Bool(broad)
		}),
		"listed": oneString("listed", func(rev string) ref.Val {
			if session.Listed == nil {
				return types.NewErr("listed is not available here")
			}
			listed := session.Listed(rev)
			f := ev.rev(rev)
			f.asked, f.listed = true, listed
			return types.Bool(listed)
		}),
		"descendants": oneString("descendants", func(rev string) ref.Val {
			if v, ok := seen[rev]; ok {
				return v
			}
			var v ref.Val
			if session.Descendants == nil {
				v = types.NewErr("descendants is not available here")
			} else if revs, err := session.Descendants(rev); err != nil {
				v = types.NewErr("descendants(%s): %v", rev, err)
			} else {
				v = types.NewStringList(types.DefaultTypeAdapter, revs)
				f := ev.rev(rev)
				f.stacked, f.stackedChecked = len(revs), true
			}
			seen[rev] = v
			return v
		}),
	}
}

func oneString(name string, f func(string) ref.Val) cel.LateBoundFunction {
	return func(_ string, a ...ref.Val) ref.Val {
		s, ok := a[0].Value().(string)
		if !ok {
			return types.NewErr("%s takes a string, not %s", name, a[0].Type())
		}
		return f(s)
	}
}

var (
	targetFlags = map[string]bool{"-r": true, "--revision": true, "--revisions": true, "-s": true, "--source": true, "-b": true, "--branch": true}
	valueFlags  = map[string]bool{"-d": true, "--destination": true, "-o": true, "--onto": true, "-A": true, "--insert-after": true,
		"--after": true, "-B": true, "--insert-before": true, "--before": true, "-m": true, "--message": true}
	// shellOperators end a command inside the raw text call.commands also carries.
	shellOperators = map[string]bool{"&&": true, "||": true, ";": true, "|": true}
	// redirection starts a token like 2>&1, >out or a bare > whose target is the next token; no revset starts so.
	redirection = regexp.MustCompile(`^(\d+|&)?(>>?|<)`)
)

// Revs is the revisions a jj command acts on: the values of -r, -s and -b and its bare arguments, or @ when it names
// none. Destinations and messages are skipped; the subcommand is taken to be one word.
func Revs(segment string) []string {
	words := shellWords(normalise(segment))
	out := []string{}
	if len(words) < 2 || words[0] != "jj" {
		return out
	}
	rest := words[2:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if shellOperators[tok] {
			break
		}
		if op := redirection.FindString(tok); op != "" {
			if op == tok {
				i++
			}
			continue
		}
		if !strings.HasPrefix(tok, "-") {
			out = append(out, unquote(tok))
			continue
		}
		name, val, hasVal := strings.Cut(tok, "=")
		if !hasVal && !strings.HasPrefix(name, "--") && len(name) > 2 {
			name, val, hasVal = name[:2], name[2:], true
		}
		if !targetFlags[name] && !valueFlags[name] {
			continue
		}
		if !hasVal && i+1 < len(rest) {
			i++
			val, hasVal = rest[i], true
		}
		if hasVal && targetFlags[name] {
			out = append(out, unquote(val))
		}
	}
	if len(out) == 0 {
		out = append(out, "@")
	}
	return out
}

// shellWords splits on spaces outside quotes, keeping the quotes on each word.
func shellWords(s string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(c)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// BroadRoot reports whether a search from path would walk far more than one project: /, a directory directly under
// it, home, a non-dot directory directly under home, ~/.local/share or ~/.local/state. A relative path resolves
// against cwd, and is never broad without one; an existing file is never broad.
func BroadRoot(path, cwd, home string) bool {
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return false
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return false
	}
	parent := filepath.Dir(path)
	if path == "/" || parent == "/" {
		return true
	}
	if home == "" {
		return false
	}
	home = filepath.Clean(home)
	return path == home ||
		(parent == home && !strings.HasPrefix(filepath.Base(path), ".")) ||
		path == filepath.Join(home, ".local", "share") || path == filepath.Join(home, ".local", "state")
}

func twoStrings(f func(a, b string) bool) cel.OverloadOpt {
	return cel.BinaryBinding(func(a, b ref.Val) ref.Val {
		x, ok1 := a.Value().(string)
		y, ok2 := b.Value().(string)
		if !ok1 || !ok2 {
			return types.NewErr("takes two strings, not %s and %s", a.Type(), b.Type())
		}
		return types.Bool(f(x, y))
	})
}

// Words is the number of whitespace-separated words in s.
func Words(s string) int { return len(strings.Fields(s)) }

// Cmd matches whole words, so "jj new" does not match "jj newt", against the segment as written and as normalised.
func Cmd(segment, prefix string) bool {
	return wordPrefix(strings.TrimSpace(segment), prefix) || wordPrefix(normalise(segment), normalise(prefix))
}

// HasFlag reports whether a token of segment is flag, flag=value, or a long-flag abbreviation of it like --colo.
func HasFlag(segment, flag string) bool {
	for _, tok := range strings.Fields(segment) {
		if tok == flag || strings.HasPrefix(tok, flag+"=") {
			return true
		}
		if strings.HasPrefix(flag, "--") && strings.HasPrefix(tok, "--") && len(tok) > 2 && strings.HasPrefix(flag, tok) {
			return true
		}
	}
	return false
}

// Positional is the arguments after the program and its subcommand that are not flags; flag values are not told apart.
func Positional(segment string) []string {
	out := []string{}
	toks := strings.Fields(segment)
	if len(toks) < 2 {
		return out
	}
	for _, tok := range toks[2:] {
		if !strings.HasPrefix(tok, "-") {
			out = append(out, tok)
		}
	}
	return out
}

func wordPrefix(cmd, prefix string) bool {
	if !strings.HasPrefix(cmd, prefix) {
		return false
	}
	return len(cmd) == len(prefix) || strings.HasSuffix(prefix, " ") || cmd[len(prefix)] == ' '
}

// globalValueFlags are the global flags that take a separate value, for the tools hold rules name.
var globalValueFlags = map[string][]string{
	"jj": {"-R", "--repository", "--at-op", "--at-operation", "--color", "--config", "--config-file"},
	"gh": {"-R", "--repo"},
}

var scriptRunners = map[string]bool{"sh": true, "bash": true, "zsh": true, "source": true, ".": true}

// normalise unwraps a script given to a shell, reduces the program to its base name and drops jj and gh global
// flags, so "jj -R x git push" reads "jj git push" and "bash /abs/scripts/setup.sh" reads "setup.sh".
func normalise(cmd string) string {
	words := strings.Fields(cmd)
	if len(words) == 0 {
		return ""
	}
	if scriptRunners[words[0]] && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
		words = words[1:]
	}
	words[0] = filepath.Base(words[0])
	if valued, ok := globalValueFlags[words[0]]; ok {
		rest := words[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			takesValue := !strings.Contains(rest[0], "=") && slices.Contains(valued, rest[0])
			rest = rest[1:]
			if takesValue && len(rest) > 0 {
				rest = rest[1:]
			}
		}
		words = append(words[:1], rest...)
	}
	return strings.Join(words, " ")
}
