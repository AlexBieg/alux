package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxReplaceChars caps one replacement. Exported because the filing subagent's tool schema has
// to state the limit, and a described limit that drifts from the enforced one is worse than none.
const MaxReplaceChars = 2000

// Edit is one exact replacement within a note.
type Edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// The write surface is deliberately narrow: create, append, and exact replacement. There is no
// delete and no wholesale rewrite, because the vault has no version control and the user edits
// it by hand.

func Create(vault, rel, content string) (string, error) {
	full, err := Resolve(vault, rel)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(full, ".md") {
		return "", fmt.Errorf("notes must end in .md")
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	// O_EXCL so a concurrent create loses rather than silently overwriting a note.
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return "Created " + rel + ".", nil
}

func Append(vault, rel, content string) (string, error) {
	full, err := Resolve(vault, rel)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("note does not exist; use create")
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + content); err != nil {
		return "", err
	}
	return "Appended to " + rel + ".", nil
}

// Replace applies every edit against the in-memory text and writes once, so a bad edit late in
// the list cannot leave a note half-updated.
func Replace(vault, rel string, edits []Edit) (string, error) {
	full, err := Resolve(vault, rel)
	if err != nil {
		return "", err
	}
	if len(edits) == 0 {
		return "", fmt.Errorf("edits must not be empty")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	text := string(data)

	for i, edit := range edits {
		where := ""
		if len(edits) > 1 {
			where = fmt.Sprintf("edit %d of %d: ", i+1, len(edits))
		}
		if len(edit.OldText) > MaxReplaceChars {
			return "", fmt.Errorf("%sold_text is limited to %d chars; notes are never rewritten wholesale. Nothing was written", where, MaxReplaceChars)
		}
		if n := strings.Count(text, edit.OldText); n != 1 {
			return "", fmt.Errorf("%sold_text must occur exactly once; found %d. Nothing was written", where, n)
		}
		text = strings.Replace(text, edit.OldText, edit.NewText, 1)
	}

	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return "", err
	}
	plural := "s"
	if len(edits) == 1 {
		plural = ""
	}
	return fmt.Sprintf("Updated %s (%d edit%s).", rel, len(edits), plural), nil
}
