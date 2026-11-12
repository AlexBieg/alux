//go:build !darwin

package orchestrator

import "context"

func cloneTree(ctx context.Context, from, to string) error {
	return copyTree(ctx, from, to, "-R", "--reflink=auto")
}
