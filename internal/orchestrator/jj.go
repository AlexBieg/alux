package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// outputCap bounds how much command output an error carries; the tail is kept, since failures print last.
const outputCap = 4096

// JJ is the VCS on the jj command line, run inside each node's workspace.
type JJ struct{}

// Resolve lists the commits revset names in repo.
func (JJ) Resolve(ctx context.Context, repo, revset string) ([]string, error) {
	out, err := jjStdout(ctx, repo, "--ignore-working-copy", "log", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func commitsOf(ctx context.Context, dir, change string) ([]string, error) {
	out, err := jjStdout(ctx, dir, "--ignore-working-copy", "log", "-r", "change_id("+change+")", "--no-graph", "-T", `commit_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// ChangeDiff is rev's git-format diff in repo, read without snapshotting so it cannot race an agent's own jj commands.
func ChangeDiff(ctx context.Context, repo, rev string) (string, error) {
	return jjStdout(ctx, repo, "--ignore-working-copy", "diff", "--git", "-r", rev)
}

// DivergentError is a change with more than one visible commit.
type DivergentError struct {
	Change  string
	Commits []string
}

func (e *DivergentError) Error() string {
	return fmt.Sprintf("change %s is divergent: commits %s", e.Change, strings.Join(e.Commits, ", "))
}

// jjStdout is jj with only its standard output returned, so warnings jj prints cannot be mistaken for output.
func jjStdout(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("jj %s in %s: %w\n%s", strings.Join(args, " "), dir, err, tail(stderr.String()+string(out), outputCap))
	}
	return strings.TrimSpace(string(out)), nil
}

func jj(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("jj %s in %s: %w\n%s", strings.Join(args, " "), dir, err, tail(string(out), outputCap))
	}
	return strings.TrimSpace(string(out)), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[…" + fmt.Sprint(len(s)-n) + " bytes cut]\n" + s[len(s)-n:]
}
