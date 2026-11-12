// Package jobs starts background subagent runs on a schedule set in settings.json, and logs each one.
package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/report"
)

const (
	defaultEvery = 3 * time.Hour
	minEvery     = 10 * time.Minute
)

type Job struct {
	Name, Agent string
	Every       time.Duration
}

// Entry is one line of the run log.
type Entry struct {
	Job     string    `json:"job"`
	Run     int       `json:"run,omitempty"`
	Session string    `json:"session,omitempty"`
	PID     int       `json:"pid,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	Outcome string    `json:"outcome"`
	Summary string    `json:"summary,omitempty"`
	Err     string    `json:"err,omitempty"`
}

// Load reads the "jobs" block of settings.json; with no block or no file it returns the curator alone.
func Load() ([]Job, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaults(), nil
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return defaults(), nil
	}
	return Parse(data)
}

func Parse(settings []byte) ([]Job, error) {
	var file struct {
		Jobs map[string]struct {
			Agent   string `json:"agent"`
			Every   string `json:"every"`
			Enabled *bool  `json:"enabled"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(settings, &file); err != nil {
		return nil, fmt.Errorf("jobs: settings.json: %w", err)
	}
	if file.Jobs == nil {
		return defaults(), nil
	}
	var list []Job
	for name, conf := range file.Jobs {
		every, err := time.ParseDuration(conf.Every)
		if err != nil || every < minEvery {
			return nil, fmt.Errorf("jobs: %s: every %q must be a duration of at least %s", name, conf.Every, minEvery)
		}
		if conf.Enabled != nil && !*conf.Enabled {
			continue
		}
		agent := conf.Agent
		if agent == "" {
			agent = name
		}
		list = append(list, Job{Name: name, Agent: agent, Every: every})
	}
	slices.SortFunc(list, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

func defaults() []Job {
	return []Job{{Name: "curator", Agent: "curator", Every: defaultEvery}}
}

func dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, paths.State, "jobs")
}

// Stamp is the file holding when the job's last run started.
func Stamp(name string) string {
	return filepath.Join(dir(), name+".last")
}

func runLog() string {
	return filepath.Join(dir(), "runs.jsonl")
}

// Append adds e to the run log in a single write, so sessions appending together never interleave a line.
func Append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(runLog(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Loop calls start for every job now and on each tick until ctx is done, reporting an error only when it differs from that job's last one.
func Loop(ctx context.Context, list []Job, tick <-chan time.Time, start func(j Job, stamp string) error, notify func(string)) {
	last := map[string]string{}
	round := func() {
		for _, j := range list {
			err := start(j, Stamp(j.Name))
			if err == nil {
				delete(last, j.Name)
				continue
			}
			if last[j.Name] == err.Error() {
				continue
			}
			last[j.Name] = err.Error()
			notify("job " + j.Name + ": not started — " + err.Error())
			now := time.Now()
			if err := Append(Entry{Job: j.Name, Started: now, Ended: now, Outcome: "not started", Err: last[j.Name]}); err != nil {
				notify("jobs: run log: " + err.Error())
			}
		}
	}
	round()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			round()
		}
	}
}

// Table lays out each job's schedule and its last logged run, for /jobs.
func Table(list []Job, now time.Time) report.Table {
	t := report.Table{
		Head:  []string{"job", "agent", "every", "last run", "started", "next due"},
		Empty: `No jobs are configured. Add one to settings.json: {"jobs": {"<name>": {"agent": "<agent>", "every": "3h"}}}`,
	}
	if len(list) == 0 {
		return t
	}
	entries := lastEntries()
	for _, j := range list {
		muted := func(text string) report.Cell { return report.Cell{Text: text, Tone: report.Muted} }
		r := report.Row{Cells: []report.Cell{{Text: j.Name}, muted(j.Agent), muted(short(j.Every)), muted("never"), muted("—"), muted("now")}}
		if started, ok := lastStart(j.Name); ok {
			r.Cells[4].Text = when(started, now)
			if due := started.Add(j.Every); due.After(now) {
				r.Cells[5].Text = when(due, now)
			}
		}
		if e, ok := entries[j.Name]; ok {
			switch {
			case e.Outcome == Running && alive(e.PID):
				r.Cells[3] = report.Cell{Text: fmt.Sprintf("running · run %d", e.Run), Tone: report.Active}
			case e.Outcome == Running:
				r.Cells[3] = report.Cell{Text: "unknown", Tone: report.Warn}
				r.Detail = "the harness running it exited before it finished"
			default:
				r.Cells[3] = report.Cell{Text: e.Outcome, Tone: outcomeTone(e.Outcome)}
				r.Detail = e.Summary
				if !e.Ended.IsZero() {
					r.Cells[3].Text += " · " + short(e.Ended.Sub(e.Started))
				}
				if e.Err != "" {
					r.Detail, r.DetailTone = e.Err, report.Bad
				}
			}
		}
		t.Rows = append(t.Rows, r)
	}
	plural := "s"
	if len(t.Rows) == 1 {
		plural = ""
	}
	t.Footer = fmt.Sprintf("%d job%s · every harness checks for due ones every 5m", len(t.Rows), plural)
	return t
}

func outcomeTone(outcome string) report.Tone {
	switch outcome {
	case "ok":
		return report.OK
	case "failed":
		return report.Bad
	}
	return report.Warn
}

// when is a clock time and how far off it is, with the date when it is not today.
func when(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	clock := t.Format("15:04")
	if t.Format("2006-01-02") != now.Format("2006-01-02") {
		clock = t.Format("Jan 2 15:04")
	}
	switch {
	case t.After(now):
		return clock + ", in " + short(t.Sub(now))
	case now.Sub(t) < time.Minute:
		return clock + ", just now"
	}
	return clock + ", " + short(now.Sub(t)) + " ago"
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 10*time.Minute && int(d.Seconds())%60 != 0:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour && int(d.Minutes())%60 != 0:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Running is the outcome logged when a run starts; its ok or failed entry follows when it ends.
const Running = "running"

// alive counts EPERM as alive: the process exists but belongs to someone else.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func lastStart(name string) (time.Time, bool) {
	data, err := os.ReadFile(Stamp(name))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return t, err == nil
}

func lastEntries() map[string]Entry {
	out := map[string]Entry{}
	f, err := os.Open(runLog())
	if err != nil {
		return out
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(nil, 1<<20)
	for scan.Scan() {
		var e Entry
		if json.Unmarshal(scan.Bytes(), &e) == nil && e.Job != "" {
			out[e.Job] = e
		}
	}
	return out
}
