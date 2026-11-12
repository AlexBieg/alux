package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// slotRepo is a fresh jj repo with a second workspace, slot, beside it.
func slotRepo(t *testing.T) (origin, slot string) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not on PATH")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin, slot = filepath.Join(dir, "repo"), filepath.Join(dir, "slot")
	jjIn(t, dir, "git", "init", "repo")
	jjIn(t, origin, "workspace", "add", slot)
	return origin, slot
}

func jjIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
}

func TestSlotGuardRefusesTheDefaultWorkspaceAndAnUndescribedSlot(t *testing.T) {
	ctx := context.Background()
	origin, slot := slotRepo(t)
	g := &SlotGuard{Slot: slot, Origin: origin}

	if err := g.Check(ctx, filepath.Join(slot, "new", "file.go")); err == nil || !strings.Contains(err.Error(), "jj describe") {
		t.Errorf("a write in an undescribed slot gave %v, want a refusal naming jj describe", err)
	}
	for _, command := range []string{"make build", "jj new main && make build", "jj util exec -- make build", "jj log > out.txt"} {
		if commands, _ := decompose(command); g.Writable(ctx, commands) {
			t.Errorf("%q is writable in an undescribed slot", command)
		}
	}
	for _, command := range []string{"jj new main", "jj git fetch && jj new feature"} {
		if commands, _ := decompose(command); !g.Writable(ctx, commands) {
			t.Errorf("%q is not writable in an undescribed slot", command)
		}
	}
	jjIn(t, slot, "describe", "-m", "session work")
	if err := g.Check(ctx, filepath.Join(slot, "file.go")); err != nil {
		t.Errorf("a write in a described slot gave %v", err)
	}
	if commands, _ := decompose("make build"); !g.Writable(ctx, commands) {
		t.Error("a described slot is not writable to the shell")
	}
	if err := g.Check(ctx, filepath.Join(origin, "file.go")); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a write in the default workspace gave %v, want a refusal", err)
	}
	if err := g.Check(ctx, filepath.Join(t.TempDir(), "scratch.txt")); err != nil {
		t.Errorf("a write outside both gave %v", err)
	}
}
