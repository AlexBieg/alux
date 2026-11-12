// Package vcs reads the working copy: the jj status line, and snapshots a run's changes are diffed against.
package vcs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/project"
)

const maxBookmarkChars = 32

type Status struct {
	ChangeID string
	Bookmark string
	Distance int
	Counts
	// Description is the first line of the working-copy change's description.
	Description string

	// Slot is the pool workspace, empty outside one; Pending is then what it has not landed or pushed.
	Slot    string
	Pending Counts
}

// Scope narrows the status to a pool slot's own work.
type Scope struct {
	Slot string
	// Base is the revset the slot's work is measured from: where it forks from what is already landed.
	Base string
	// Remote is set only in the pull-request flow.
	Remote string
}

// Counts is a change's files added, modified and removed, and its lines added and removed.
type Counts struct {
	Added        int
	Modified     int
	Removed      int
	LinesAdded   int
	LinesRemoved int
}

// CountsTemplate is the jj template fields ParseCounts reads, joined by sep.
func CountsTemplate(sep string) string {
	return `self.diff().files().map(|f| f.status()).join(",") ++ "` + sep + `" ++ self.diff().stat().total_added() ++ "` + sep + `" ++ self.diff().stat().total_removed()`
}

// ParseCounts reads CountsTemplate's three fields.
func ParseCounts(files, added, removed string) Counts {
	var c Counts
	if files != "" {
		for _, status := range strings.Split(files, ",") {
			switch status {
			case "added":
				c.Added++
			case "removed":
				c.Removed++
			default:
				c.Modified++
			}
		}
	}
	c.LinesAdded, _ = strconv.Atoi(added)
	c.LinesRemoved, _ = strconv.Atoi(removed)
	return c
}

func (c Counts) Empty() bool { return c.Added+c.Modified+c.Removed == 0 }

// Render is the file counts in brackets and then the lines, as "[+1 ~2] +85/-249"; empty for no files.
func (c Counts) Render() string {
	if c.Empty() {
		return ""
	}
	green := lipgloss.NewStyle().Foreground(lipgloss.Green)
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	red := lipgloss.NewStyle().Foreground(lipgloss.Red)
	dim := lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	var counts []string
	if c.Added > 0 {
		counts = append(counts, green.Render(fmt.Sprintf("+%d", c.Added)))
	}
	if c.Modified > 0 {
		counts = append(counts, yellow.Render(fmt.Sprintf("~%d", c.Modified)))
	}
	if c.Removed > 0 {
		counts = append(counts, red.Render(fmt.Sprintf("-%d", c.Removed)))
	}
	return "[" + strings.Join(counts, " ") + "] " + green.Render(fmt.Sprintf("+%d", c.LinesAdded)) + dim.Render("/") +
		red.Render(fmt.Sprintf("-%d", c.LinesRemoved))
}

var workingCopyTemplate = `change_id.shortest(8) ++ "|" ++ ` + CountsTemplate("|") + ` ++ "|" ++ description.first_line()`

func jj(args ...string) (string, error) {
	return command(project.JJRoot(), nil, "jj", args...)
}

func command(dir string, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.Output()
	return string(out), err
}

// Read returns nil outside a jj repo or on any failure: the status line is decoration, and a
// broken jj must never take the pane down.
//
// Only call this between agent steps. The first query snapshots the working copy, and a snapshot
// taken while the agent runs its own jj command makes that command fail outright — measured as
// `Internal error: Failed to check out commit` out of `jj new`, with divergent operations to
// reconcile afterwards. Mid-turn, call Peek instead.
func Read(scope Scope) *Status {
	return read(scope, true)
}

// Peek is Read without the working-copy snapshot, so it is safe to run while the agent is
// working. The change id and bookmark are exact; the diff counts are as of the last snapshot
// anyone took, so edits made since a turn's last jj command are not in them yet.
func Peek(scope Scope) *Status {
	return read(scope, false)
}

func read(scope Scope, snapshot bool) *Status {
	if project.JJRoot() == "" {
		return nil
	}
	args := []string{"log", "-r", "@", "--no-graph", "-T", workingCopyTemplate}
	if !snapshot {
		args = append(args, "--ignore-working-copy")
	}
	out, err := jj(args...)
	if err != nil {
		return nil
	}
	fields := strings.SplitN(strings.TrimSpace(out), "|", 5)
	if len(fields) != 5 {
		return nil
	}
	if scope.Slot != "" {
		return readSlot(project.JJRoot(), scope, fields)
	}

	chainOut, err := jj("log", "--ignore-working-copy", "-r", "latest(::@ & bookmarks())::@",
		"--no-graph", "-T", `bookmarks.map(|b| b.name()).join(", ") ++ "\n"`)
	if err != nil {
		return nil
	}
	chain := strings.Split(strings.TrimRight(chainOut, "\n"), "\n")

	s := &Status{ChangeID: fields[0], Description: fields[4]}
	// Newest first, so the bookmarked commit is the last line; an empty last line means no
	// bookmark anywhere in history.
	if last := chain[len(chain)-1]; last != "" {
		s.Bookmark = last
		s.Distance = len(chain) - 1
	}
	s.Counts = ParseCounts(fields[1], fields[2], fields[3])
	return s
}

// readSlot measures the slot's work from scope.Base, or in the pull-request flow from the
// remote's copy of the newest bookmark on that work, so the counts are what /land or a push would carry.
func readSlot(dir string, scope Scope, fields []string) *Status {
	jj := func(args ...string) (string, error) { return command(dir, nil, "jj", args...) }
	s := &Status{ChangeID: fields[0], Description: fields[4], Slot: scope.Slot}
	s.Counts = ParseCounts(fields[1], fields[2], fields[3])
	base := "heads(::@ & ::(" + scope.Base + "))"
	from := base
	if scope.Remote != "" {
		names, err := jj("log", "--ignore-working-copy", "-r", "latest((::@ ~ ::"+base+") & bookmarks())",
			"--no-graph", "-T", `local_bookmarks.map(|b| b.name()).join(",")`)
		if err != nil {
			return nil
		}
		if name, _, _ := strings.Cut(strings.TrimSpace(names), ","); name != "" {
			s.Bookmark = name
			from = "coalesce(present(remote_bookmarks(exact:" + strconv.Quote(name) + ", remote=exact:" + strconv.Quote(scope.Remote) + ")), " + base + ")"
		}
	}
	summary, err := jj("diff", "--ignore-working-copy", "--from", from, "--to", "@", "--summary")
	if err != nil {
		return nil
	}
	stat, err := jj("diff", "--ignore-working-copy", "--from", from, "--to", "@", "--stat")
	if err != nil {
		return nil
	}
	s.Pending = parseDiff(summary, stat)
	return s
}

// parseDiff reads jj diff --summary's status letters and the totals on --stat's last line.
func parseDiff(summary, stat string) Counts {
	var c Counts
	for _, line := range strings.Split(strings.TrimSpace(summary), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "A "):
			c.Added++
		case strings.HasPrefix(line, "D "):
			c.Removed++
		default:
			c.Modified++
		}
	}
	lines := strings.Split(strings.TrimSpace(stat), "\n")
	for _, part := range strings.Split(lines[len(lines)-1], ", ") {
		n, rest, _ := strings.Cut(strings.TrimSpace(part), " ")
		switch v, _ := strconv.Atoi(n); {
		case strings.HasPrefix(rest, "insertion"):
			c.LinesAdded = v
		case strings.HasPrefix(rest, "deletion"):
			c.LinesRemoved = v
		}
	}
	return c
}

// Render takes the number of the PR the slot's bookmark heads, zero when none is known.
func (s *Status) Render(pr int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Blue).Render("󱗆 "))
	if s.Slot != "" {
		return b.String() + s.renderSlot(pr)
	}

	if s.Bookmark != "" {
		name := s.Bookmark
		if len(name) > maxBookmarkChars {
			name = name[:maxBookmarkChars-1] + "…"
		}
		if s.Distance > 0 {
			name += fmt.Sprintf("~%d", s.Distance)
		}
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Magenta).Render(name) + " ")
	}
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.BrightBlack).Render(s.ChangeID))
	if !s.Empty() {
		b.WriteString(" " + s.Counts.Render())
	}
	return b.String()
}

// renderSlot is the slot, then the PR its work is for (or the branch, before a PR is known), then the pending counts.
func (s *Status) renderSlot(pr int) string {
	parts := []string{lipgloss.NewStyle().Foreground(lipgloss.Magenta).Render(s.Slot)}
	switch {
	case pr > 0:
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Cyan).Render(fmt.Sprintf("#%d", pr)))
	case s.Bookmark != "":
		name := s.Bookmark
		if len(name) > maxBookmarkChars {
			name = name[:maxBookmarkChars-1] + "…"
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Cyan).Render(name))
	}
	if !s.Pending.Empty() {
		parts = append(parts, s.Pending.Render())
	}
	return strings.Join(parts, " ")
}
