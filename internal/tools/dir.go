package tools

import (
	"context"

	"github.com/alexbieg/alux/internal/project"
)

type dirKey struct{}

// WithDir scopes the file and shell tools called under ctx to dir instead of the process's working directory.
func WithDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, dirKey{}, dir)
}

// Scoped is the directory WithDir put on ctx, if any.
func Scoped(ctx context.Context) (string, bool) {
	dir, ok := ctx.Value(dirKey{}).(string)
	return dir, ok && dir != ""
}

// Dir is the directory tools resolve against under ctx: WithDir's, or the process's working directory.
func Dir(ctx context.Context) string {
	if dir, ok := ctx.Value(dirKey{}).(string); ok && dir != "" {
		return dir
	}
	return project.Cwd()
}
