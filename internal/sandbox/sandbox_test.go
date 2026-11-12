package sandbox

import (
	"path/filepath"
	"testing"
)

func TestBuildRefusesWithoutSandboxExec(t *testing.T) {
	saved := exe
	exe = filepath.Join(t.TempDir(), "missing")
	defer func() { exe = saved }()
	if _, err := Build(Profile{Root: t.TempDir()}); err == nil {
		t.Error("built a profile with no sandbox-exec to run it")
	}
}
