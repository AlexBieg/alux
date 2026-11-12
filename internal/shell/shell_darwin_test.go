package shell

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/sandbox"
)

func TestTheShellRunsUnderAReadOnlyProfileAndKeepsItsStateAcrossAProfileChange(t *testing.T) {
	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)\n(allow default)", "true").CombinedOutput()
	if err != nil && strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		t.Skip("already inside a sandbox, and Seatbelt profiles cannot nest")
	}
	s := newShell(t)
	readOnly, err := sandbox.Build(sandbox.Profile{Root: s.Root()})
	if err != nil {
		t.Skip(err)
	}
	if out, res := run(t, s, "cd /tmp; export KEPT=1; echo ran", &readOnly); out != "ran" || res.Lost {
		t.Fatalf("got %q %+v", out, res)
	}
	writable, err := sandbox.Build(sandbox.Profile{Root: s.Root(), Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := run(t, s, `pwd -P; echo "${KEPT:-}"`, &writable); out != "/private/tmp\n1" {
		t.Fatalf("got %q", out)
	}
}
