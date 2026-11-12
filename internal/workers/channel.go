// Package workers is what outlives a headless worker's process in its channel directory, and the
// prompt it is parked on as another pane reads it.
package workers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/paths"
)

const Version = 1

// Dir is the worker's channel directory, which it does not create.
func Dir(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.Cache, "sessions", "workers", key), nil
}

// Prompt is the approval or question a session is parked on, as another pane reads it; ID is the registry's PromptID.
type Prompt struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Label    string        `json:"label,omitempty"`
	Body     string        `json:"body,omitempty"`
	Question *ask.Question `json:"question,omitempty"`
}

// Complete reports whether the prompt has what the panel needs to show it.
func (p Prompt) Complete() bool {
	if p.Kind == "question" {
		return p.Question != nil
	}
	return p.Label != ""
}

// stoppedFile marks a worker stopped on purpose, so its dead process does not read as a crash.
const stoppedFile = "stopped"

// MarkStopped records that key's worker was stopped rather than crashed.
func MarkStopped(key string) error {
	dir, err := Dir(key)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stoppedFile), nil, 0o600)
}

// Final is final.json, written as a worker exits because its skill run closed.
type Final struct {
	V       int    `json:"v"`
	State   string `json:"state"`
	How     string `json:"how"`
	Message string `json:"message,omitempty"`
}

// ReadJSON fails on a missing file, bad JSON, or a version this build does not read.
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	if head.V != Version {
		return fmt.Errorf("%s: version %d", path, head.V)
	}
	return nil
}

// WriteJSON writes through a temporary file and a rename, so a reader never sees half of one.
func WriteJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
