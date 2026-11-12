package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/tools"
)

// baselines are copies of each file the edit tool changes, as it read before the session's first edit to it.
type baselines struct {
	mu     sync.Mutex
	dir    string
	copies map[string]string
}

// keepBaseline copies an edit's file the first time this session is about to write it; a missing file copies as empty.
func (s *Session) keepBaseline(label string, kind agent.ToolKind) {
	if kind != agent.KindEdit {
		return
	}
	switch tools.EditVerb(label) {
	case "create", "edit", "insert":
	default:
		return
	}
	path := tools.EditPath(label)
	if path == "" {
		return
	}
	b := &s.baselines
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.copies[path]; ok {
		return
	}
	data, err := os.ReadFile(absolute(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	if b.dir == "" {
		if b.dir, err = os.MkdirTemp("", "alux-baselines-"); err != nil {
			b.dir = ""
			return
		}
	}
	if b.copies == nil {
		b.copies = map[string]string{}
	}
	f, err := os.CreateTemp(b.dir, "")
	if err != nil {
		return
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		return
	}
	b.copies[path] = f.Name()
}

func (b *baselines) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
	b.dir, b.copies = "", nil
}

// FileDiff is the unified patch, from its first hunk on, of a file in Files from before the session first edited it to now;
// empty when the two match.
func (s *Session) FileDiff(path string) (string, error) {
	s.baselines.mu.Lock()
	saved, ok := s.baselines.copies[path]
	s.baselines.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no copy of %s from before this session edited it", path)
	}
	live := absolute(path)
	if _, err := os.Stat(live); errors.Is(err, os.ErrNotExist) {
		live = os.DevNull
	}
	out, err := exec.Command("git", "diff", "--no-index", "--no-color", "--no-ext-diff", "--", saved, live).Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", fmt.Errorf("git diff: %w", err)
	}
	text := string(out)
	if strings.HasPrefix(text, "@@") {
		return text, nil
	}
	if i := strings.Index(text, "\n@@"); i >= 0 {
		return text[i+1:], nil
	}
	if strings.Contains(text, "Binary files") {
		return "", fmt.Errorf("%s is binary; there is no text diff to show", path)
	}
	return "", nil
}

func absolute(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(project.Cwd(), path)
}
