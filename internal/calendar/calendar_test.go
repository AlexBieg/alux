package calendar

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// A Zoom location can carry a passcode headed for a transcript. The room-name case is here
// because over-eager scrubbing would be the other way to fail.
func TestScrubSecretsStripsZoomPasscodes(t *testing.T) {
	cases := map[string]string{
		"https://example.zoom.us/j/12345678901?pwd=FakePasscode123.1": "https://example.zoom.us/j/12345678901",
		"https://meet.example.com/x?foo=1&passcode=123456":            "https://meet.example.com/x?foo=1",
		"https://meet.example.com/x?foo=1&PASSWORD=hunter2":           "https://meet.example.com/x?foo=1",
		"Conference Room 3, 4th floor":                                "Conference Room 3, 4th floor",
		"":                                                            "",
	}
	for in, want := range cases {
		if got := scrubSecrets(in); got != want {
			t.Errorf("scrubSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}

func requireRealCalendar(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the calendar is macOS only")
	}
	if os.Getenv("ALUX_REAL_CALENDAR") == "" {
		t.Skip("set ALUX_REAL_CALENDAR=1 to write a real event to the local calendar")
	}
}

// The only check that this drives the real calendar store. It writes to the local calendar —
// never a synced one, where a stray event would reach other people — and deletes what it made.
func TestCreateAndDeleteRoundTrip(t *testing.T) {
	requireRealCalendar(t)
	// Far enough out that a failed cleanup cannot collide with anything real.
	start := time.Date(2030, 5, 14, 9, 0, 0, 0, time.Local)
	ev := Event{
		Calendar:     "Calendar",
		Title:        "alux round-trip test",
		Start:        start,
		End:          start.Add(45 * time.Minute),
		Location:     "test",
		Description:  "written by a test\nsecond line with \"quotes\"",
		AlarmMinutes: 10,
	}
	uid, err := Create(ev)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if uid == "" {
		t.Fatal("Create returned an empty uid")
	}
	t.Cleanup(func() { _, _ = Delete(ev.Calendar, uid) })

	found, err := Delete(ev.Calendar, uid)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !found {
		t.Error("Delete reported no matching event, so the created event was not found by its uid")
	}
	// Deleting the same uid twice reports nothing found rather than erroring, which is what lets
	// cleanup run unconditionally.
	again, err := Delete(ev.Calendar, uid)
	if err != nil {
		t.Fatalf("second Delete errored instead of reporting nothing found: %v", err)
	}
	if again {
		t.Error("the event survived its own deletion")
	}
}

// Writing and reading are separate paths into the store, so this is the check that the read one
// sees what the write one did: an event created here has to come back out of Events, and has to
// stop coming back once it is deleted.
func TestEventsSeesACreatedEvent(t *testing.T) {
	requireRealCalendar(t)
	start := time.Date(2030, 5, 15, 14, 0, 0, 0, time.Local)
	ev := Event{
		Calendar: "Calendar",
		Title:    "alux read-back test",
		Start:    start,
		End:      start.Add(30 * time.Minute),
		Location: "https://example.zoom.us/j/12345678901?pwd=FakePasscode123.1",
	}
	uid, err := Create(ev)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = Delete(ev.Calendar, uid) })

	from, to := start.Add(-time.Hour), start.Add(3*time.Hour)
	found, err := Events(ev.Calendar, from, to)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var got *Occurrence
	for i := range found {
		if found[i].UID == uid {
			got = &found[i]
		}
	}
	if got == nil {
		t.Fatalf("Events did not return the event just created; window held %d events", len(found))
	}
	if got.Title != ev.Title {
		t.Errorf("title read back as %q, want %q", got.Title, ev.Title)
	}
	if !got.Start.Equal(start) {
		t.Errorf("start read back as %s, want %s", got.Start, start)
	}
	if got.Location != "https://example.zoom.us/j/12345678901" {
		t.Errorf("location read back as %q, with the passcode still in it", got.Location)
	}

	if _, err := Delete(ev.Calendar, uid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	after, err := Events(ev.Calendar, from, to)
	if err != nil {
		t.Fatalf("Events after delete: %v", err)
	}
	for _, occ := range after {
		if occ.UID == uid {
			t.Error("the deleted event is still being read back")
		}
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	base := Event{Calendar: "Calendar", Title: "x", Start: time.Now(), End: time.Now().Add(time.Hour)}
	withTitle := func(s string) Event { e := base; e.Title = s; return e }
	withCal := func(s string) Event { e := base; e.Calendar = s; return e }

	cases := map[string]Event{
		"no title":          withTitle(""),
		"no calendar":       withCal(""),
		"end before start":  {Calendar: "Calendar", Title: "x", Start: time.Now(), End: time.Now().Add(-time.Hour)},
		"zero-length event": {Calendar: "Calendar", Title: "x", Start: base.Start, End: base.Start},
	}
	for name, ev := range cases {
		if _, err := Create(ev); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}
