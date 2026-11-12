// Package mine records each turn's tool calls as short symbols and mines them for recurring workflows.
//
// The log holds symbols only, never arguments, paths or output, so it is safe to keep forever.
package mine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// Turn is one line of the sequence log: the symbols one user turn's tool calls became.
type Turn struct {
	Session      string    `json:"session"`
	Conversation int       `json:"conversation"`
	Turn         int       `json:"turn"`
	Time         time.Time `json:"time"`
	Skill        string    `json:"skill,omitempty"`
	Calls        []string  `json:"calls"`
	Model        string    `json:"model,omitempty"`
	Prompt       string    `json:"prompt,omitempty"`
}

// Key names the conversation a turn belongs to.
func (t Turn) Key() string {
	return t.Session + "#" + strconv.Itoa(t.Conversation)
}

func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "mine")
}

// SequencesFile is the append-only log in dir.
func SequencesFile(dir string) string { return filepath.Join(dir, "sequences.jsonl") }

// ReadLog is every turn in the log at path, skipping lines it cannot parse.
func ReadLog(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var turns []Turn
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var t Turn
		if json.Unmarshal(sc.Bytes(), &t) == nil {
			turns = append(turns, t)
		}
	}
	return turns, sc.Err()
}
