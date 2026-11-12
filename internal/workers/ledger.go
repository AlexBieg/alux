package workers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/report"
)

// Worker is one headless worker as the ledger reports it.
type Worker struct {
	Key, Source, Origin string
	PID                 int
	Session, Slot, Mode string
	Stage, State        string
	Waiting             bool
	LastActivity        time.Time
	Transcript, Log     string
	// Ended and Message are final.json's how and closing message, once the worker exited on its skill run closing.
	Ended, Message string
}

// Meta is worker.json, written once at start so a worker whose process died can still be placed in its repo.
type Meta struct {
	V          int    `json:"v"`
	Origin     string `json:"origin"`
	PID        int    `json:"pid"`
	Session    string `json:"session,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Log        string `json:"log,omitempty"`
}

const (
	Crashed = "crashed"
	Failed  = "failed"
	Done    = "done"
)

var ticketKey = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)

// Source is "ticket" for a key shaped like a tracker issue, else "adhoc".
func Source(key string) string {
	if ticketKey.MatchString(key) && !strings.HasPrefix(key, "WORKER-") {
		return "ticket"
	}
	return "adhoc"
}

// LogPath is where a worker's audit log of unprompted escalations goes.
func LogPath(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.State, "workers", key+".log"), nil
}

// Read lists the workers of origin: every live one, each that exited with a final.json, each whose process died without
// one and without being stopped, and each that died before it started.
func Read(origin string) []Worker {
	var out []Worker
	live := map[string]bool{}
	for _, info := range registry.Default("", "").Others() {
		if info.Worker == "" {
			continue
		}
		live[info.Worker] = true
		if info.Origin != origin {
			continue
		}
		t := Worker{Key: info.Worker, Source: Source(info.Worker), Origin: info.Origin, PID: info.PID, Session: info.Session,
			Slot: info.Workspace, Mode: info.Mode, Stage: info.Stage, State: info.WorkerState, Waiting: info.PromptID != "",
			Transcript: info.Transcript, Log: info.Log}
		if t.State == "" {
			t.State = "running"
		}
		if info.LastActivity > 0 {
			t.LastActivity = time.UnixMilli(info.LastActivity)
		}
		out = append(out, t)
	}
	out = append(out, crashed(origin, live)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func crashed(origin string, live map[string]bool) []Worker {
	root, err := Dir("")
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Worker
	for _, e := range entries {
		key := e.Name()
		if !e.IsDir() || live[key] {
			continue
		}
		dir := filepath.Join(root, key)
		var m Meta
		if ReadJSON(filepath.Join(dir, "worker.json"), &m) != nil || m.Origin != origin {
			continue
		}
		var final Final
		finalPath := finalFile(dir)
		if ReadJSON(finalPath, &final) == nil && !registry.Alive(m.PID) {
			t := Worker{Key: key, Source: Source(key), Origin: m.Origin, PID: m.PID, Session: m.Session, Slot: m.Workspace,
				Mode: m.Mode, State: final.State, Ended: final.How, Message: final.Message, Transcript: m.Transcript, Log: m.Log}
			if info, err := os.Stat(finalPath); err == nil {
				t.LastActivity = info.ModTime()
			}
			out = append(out, t)
			continue
		}
		if registry.Alive(m.PID) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, stoppedFile)); err == nil {
			continue
		}
		// Only worker_start's worker.json lacks a session: the process died before it got going.
		state := Crashed
		if m.Session == "" {
			state = Failed
		}
		t := Worker{Key: key, Source: Source(key), Origin: m.Origin, PID: m.PID, Session: m.Session, Slot: m.Workspace,
			Mode: m.Mode, State: state, Transcript: m.Transcript, Log: m.Log}
		if info, err := os.Stat(filepath.Join(dir, "worker.json")); err == nil {
			t.LastActivity = info.ModTime()
		}
		out = append(out, t)
	}
	return out
}

// finalFile is dir's final.json, or the final.delivered.<pid>.json a TUI renamed it to.
func finalFile(dir string) string {
	path := filepath.Join(dir, "final.json")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "final.delivered.*.json")); len(matches) > 0 {
		return matches[0]
	}
	return path
}

func age(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

const none = "no workers in this repo"

// Lines is one line per worker for the workers tool, its empty fields left out.
func Lines(ts []Worker, now time.Time) string {
	if len(ts) == 0 {
		return none
	}
	var lines []string
	for _, t := range ts {
		flag := ""
		if t.Waiting {
			flag = "⚑"
		}
		var parts []string
		state := t.State
		if t.Ended != "" && t.Ended != state {
			state = t.Ended
		}
		for _, p := range []string{t.Key, t.Stage, state, flag, t.Mode, age(t.LastActivity, now), t.Slot, t.Log} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		lines = append(lines, strings.Join(parts, " · "))
	}
	return strings.Join(lines, "\n")
}

// Table is /workers.
func Table(ts []Worker, now time.Time) report.Table {
	t := report.Table{Head: []string{"worker", "stage", "state", "mode", "active", "slot"}, Empty: none}
	for _, tk := range ts {
		key, tone := tk.Key, report.Default
		switch {
		case tk.Waiting:
			key, tone = key+" ⚑", report.Warn
		case tk.State == Crashed || tk.State == Failed:
			tone = report.Bad
		case tk.State == "stalled":
			tone = report.Warn
		}
		t.Rows = append(t.Rows, report.Row{
			Cells: []report.Cell{{Text: key, Tone: tone}, {Text: tk.Stage}, {Text: tk.State, Tone: tone}, {Text: tk.Mode},
				{Text: age(tk.LastActivity, now), Tone: report.Muted}, {Text: tk.Slot, Tone: report.Muted}},
			Detail: tk.Log, DetailTone: report.Muted,
		})
	}
	return t
}

// Detail is /workers KEY: where the worker's transcript, audit log and channel live.
func Detail(ts []Worker, key string) (string, bool) {
	for _, t := range ts {
		if !strings.EqualFold(t.Key, key) {
			continue
		}
		dir, _ := Dir(t.Key)
		out := fmt.Sprintf("%s · %s\ntranscript: %s\nlog: %s\nchannel: %s", t.Key, t.State, t.Transcript, t.Log, dir)
		if t.Ended != "" {
			out += "\nended: " + t.Ended + "\nlast message: " + t.Message
		}
		return out, true
	}
	return "", false
}
