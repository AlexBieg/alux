package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// Undescribed is what a write is refused with while the slot's change has no description.
const Undescribed = "this session's change has no description, so edits are refused: run jj describe -m \"<what this session is doing>\" " +
	"first, then edit. Shell commands cannot write in the workspace until then either."

// SlotGuard keeps a session in a pool slot out of the default workspace, and out of its own slot until its change is described.
type SlotGuard struct {
	Slot, Origin string
}

// Guard, when set, is checked before every write the edit tool makes, the main agent's and every subagent's alike.
var Guard *SlotGuard

// Check refuses a write to path that the guard does not allow.
func (g *SlotGuard) Check(ctx context.Context, path string) error {
	target, err := realPath(path)
	if err != nil {
		return err
	}
	switch {
	case under(target, g.Slot):
		ok, err := Described(ctx, g.Slot)
		if err != nil {
			return fmt.Errorf("checking this session's change for a description failed: %w", err)
		}
		if !ok {
			return errors.New(Undescribed)
		}
	case under(target, g.Origin):
		return fmt.Errorf("%s is in the default workspace at %s, which is read-only to sessions; edit the same path under %s instead",
			path, g.Origin, g.Slot)
	}
	return nil
}

// Writable reports whether a shell call running commands may write in the slot now: once its change is described,
// or before then when every command is jj, whose writes the op log records, so moving to a bookmark needs no description.
func (g *SlotGuard) Writable(ctx context.Context, commands [][]string) bool {
	if jjOnly(commands) {
		return true
	}
	ok, err := Described(ctx, g.Slot)
	return ok && err == nil
}

// jjOnly reports a non-empty list of commands that are all jj, none of them jj util, which can run anything.
func jjOnly(commands [][]string) bool {
	for _, words := range commands {
		if len(words) == 0 || words[0] != "jj" || slices.Contains(words, "util") {
			return false
		}
	}
	return len(commands) > 0
}

// Described reports whether the working-copy change of the jj workspace at dir has a description.
func Described(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "description")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
