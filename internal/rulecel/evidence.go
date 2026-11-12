package rulecel

import (
	"fmt"
	"slices"
	"sync"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

// Evidence is a compiled skill evidence condition: call and the pure functions, plus a run's inputs, vars and scenario.
type Evidence struct {
	prg  cel.Program
	refs []Ref
}

// Ref is one key a condition reads from inputs, vars or scenario.
type Ref struct{ Var, Key string }

var evidenceEnv = sync.OnceValues(func() (*cel.Env, error) {
	str := cel.StringType
	return cel.NewEnv(append(pureFunctions(),
		cel.Variable("call", cel.MapType(str, cel.DynType)),
		cel.Variable("inputs", cel.MapType(str, cel.DynType)),
		cel.Variable("vars", cel.MapType(str, str)),
		cel.Variable("scenario", cel.MapType(str, cel.DynType)),
	)...)
})

var dataVars = map[string]bool{"inputs": true, "vars": true, "scenario": true}

// CompileEvidence parses and type-checks expr, refusing one that is not a bool or reads inputs, vars or scenario other than by a literal key.
func CompileEvidence(expr string) (*Evidence, error) {
	e, err := evidenceEnv()
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
	refs, err := dataRefs(ast.NativeRep())
	if err != nil {
		return nil, err
	}
	prg, err := e.Program(ast)
	if err != nil {
		return nil, err
	}
	return &Evidence{prg: prg, refs: refs}, nil
}

// Refs is every inputs, vars and scenario key the condition reads, once each in the order it first reads them.
func (e *Evidence) Refs() []Ref { return e.refs }

// dataRefs walks a checked expression for its reads of inputs, vars and scenario, since CEL cannot type map keys.
func dataRefs(a *celast.AST) ([]Ref, error) {
	keyed := map[int64]string{}
	var refs []Ref
	bad := ""
	celast.PreOrderVisit(a.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.SelectKind:
			sel := e.AsSelect()
			if op := sel.Operand(); op.Kind() == celast.IdentKind && dataVars[op.AsIdent()] {
				keyed[op.ID()] = sel.FieldName()
			}
		case celast.CallKind:
			c := e.AsCall()
			if args := c.Args(); c.FunctionName() == operators.Index && len(args) == 2 && args[0].Kind() == celast.IdentKind &&
				args[1].Kind() == celast.LiteralKind {
				if key, ok := args[1].AsLiteral().(types.String); ok {
					keyed[args[0].ID()] = string(key)
				}
			}
		case celast.IdentKind:
			name := e.AsIdent()
			if !dataVars[name] {
				return
			}
			key, ok := keyed[e.ID()]
			switch {
			case !ok:
				if bad == "" {
					bad = name
				}
			case !slices.Contains(refs, Ref{name, key}):
				refs = append(refs, Ref{name, key})
			}
		}
	}))
	if bad != "" {
		return nil, fmt.Errorf("%s is read only as %s.<name>", bad, bad)
	}
	return refs, nil
}

// Eval reports whether the condition holds for the call; inputs, vars and scenario are data, never part of the expression.
func (e *Evidence) Eval(call Call, inputs map[string]any, vars map[string]string, scenario map[string]any) (bool, error) {
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	commands := call.Commands
	if commands == nil {
		commands = []string{}
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	if vars == nil {
		vars = map[string]string{}
	}
	if scenario == nil {
		scenario = map[string]any{}
	}
	out, _, err := e.prg.Eval(map[string]any{
		"call":     map[string]any{"tool": call.Tool, "args": args, "commands": commands, "path": call.Path},
		"inputs":   inputs,
		"vars":     vars,
		"scenario": scenario,
	})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("the condition gave %v, not a bool", out.Value())
	}
	return b, nil
}
