package orchestrator

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

// cloneTree clones from to to in one clonefile call, several times faster than cp -c -R on a large tree;
// where clonefile cannot (another volume, a filesystem without clones) it falls back to cp.
func cloneTree(ctx context.Context, from, to string) error {
	if err := unix.Clonefile(from, to, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	return copyTree(ctx, from, to, "-c", "-R")
}
