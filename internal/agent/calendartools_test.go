package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/calendar"
	"github.com/alexbieg/alux/internal/paths"
)

func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, time.Local)
}

func row(cal, title string, day, hour int) scheduleRow {
	return scheduleRow{calendar: cal, occurrence: calendar.Occurrence{
		Title: title, Start: at(day, hour, 0), End: at(day, hour+1, 0),
	}}
}

func TestCalendarToolsNeedTheSetting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if tools := CalendarTools(); tools != nil {
		t.Errorf("calendar tools offered without settings.json: %d", len(tools))
	}
	dir := filepath.Join(home, paths.Config)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"calendar": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tools := CalendarTools(); tools != nil {
		t.Errorf("calendar tools offered with calendar false: %d", len(tools))
	}
	if runtime.GOOS != "darwin" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"calendar": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tools := CalendarTools(); len(tools) == 0 {
		t.Error("calendar tools missing with calendar true")
	}
}

// Reading every writable calendar is the normal case and most of them are empty, so the calendar
// name is worth a line's width only when the events actually came from more than one.
func TestTheCalendarIsNamedOnlyWhenTheEventsSpanMoreThanOne(t *testing.T) {
	from, to := at(8, 0, 0), at(9, 0, 0)
	read := []string{"Calendar", "person@example.com", "Birthdays"}

	oneSource := formatSchedule([]scheduleRow{
		row("person@example.com", "Standup", 8, 9),
		row("person@example.com", "Retro", 8, 11),
	}, read, from, to)
	if strings.Contains(oneSource, "[person@example.com]") {
		t.Errorf("three calendars were read but only one had events, so no line should name it:\n%s", oneSource)
	}

	twoSources := formatSchedule([]scheduleRow{
		row("person@example.com", "Standup", 8, 9),
		row("Calendar", "Dentist", 8, 11),
	}, read, from, to)
	for _, want := range []string{"[person@example.com]", "[Calendar]"} {
		if !strings.Contains(twoSources, want) {
			t.Errorf("events came from two calendars, so %s should appear:\n%s", want, twoSources)
		}
	}
}

// An empty window has to say so in words: a bare empty string reads as a failure, and a day with
// nothing on it is a real answer.
func TestAnEmptyWindowSaysSoRatherThanReturningNothing(t *testing.T) {
	out := formatSchedule(nil, []string{"Calendar"}, at(8, 0, 0), at(9, 0, 0))
	if strings.TrimSpace(out) == "" {
		t.Fatal("an empty window returned an empty string")
	}
	if !strings.Contains(out, "No events") {
		t.Errorf("an empty window should say there are none, got: %q", out)
	}
}
