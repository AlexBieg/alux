// Package calendar reads and writes macOS calendar events through EventKit.
//
// An event written to a Google-backed calendar does reach Google. Nothing on the machine can
// confirm that for an individual write — an identifier is a local one either way and the CalDAV
// store under ~/Library is not readable — so confirming one means a human looking at Google
// Calendar.
package calendar

import (
	"errors"
	"regexp"
	"sort"
	"time"
)

// Event is one timed entry. End is required rather than inferred from a duration: a model that
// has to write both is less likely to leave the length to chance.
type Event struct {
	Calendar    string
	Title       string
	Start       time.Time
	End         time.Time
	Location    string
	Description string
	AllDay      bool
	// AlarmMinutes fires an alert that many minutes before the start. Zero means no alarm.
	AlarmMinutes int
}

// Occurrence is one event as read back. Description is deliberately absent: invite bodies carry
// Zoom passcodes and pwd= tokens, which are live credentials, and the cheapest way not to leak
// one into a transcript is never to fetch it.
type Occurrence struct {
	UID      string
	Title    string
	Start    time.Time
	End      time.Time
	Location string
	AllDay   bool
}

// Writable lists the calendars that accept a new event, by name. Subscribed calendars such as
// holidays come back read-only and are dropped.
func Writable() ([]string, error) {
	titles, err := ekWritable()
	if err != nil {
		return nil, err
	}
	sort.Strings(titles)
	return titles, nil
}

// Default names the calendar a new event lands on when nobody says otherwise, which is the one
// the user set in Calendar's settings. It is empty when the store names none, or names one that
// cannot be written to.
func Default() (string, error) {
	return ekDefaultCalendar()
}

// Events reads one window of one calendar, earliest first.
//
// An event is listed when it overlaps the window rather than when it starts inside it, so a
// meeting already under way at `from` is reported — and so is the all-day event that a window
// opening at noon would otherwise miss. Recurrence is expanded, so a weekly meeting yields the
// occurrence that actually falls in the window, carrying whatever title that one occurrence was
// edited to rather than the series title.
func Events(calendarName string, from, to time.Time) ([]Occurrence, error) {
	if calendarName == "" {
		return nil, errors.New("reading needs a calendar")
	}
	if !to.After(from) {
		return nil, errors.New("the end of the window must be after its start")
	}
	out, err := ekEvents(calendarName, from, to)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// Create adds the event and returns its uid, which is what a later delete selects on.
func Create(ev Event) (string, error) {
	if ev.Title == "" {
		return "", errors.New("an event needs a title")
	}
	if ev.Calendar == "" {
		return "", errors.New("an event needs a calendar")
	}
	if !ev.End.After(ev.Start) {
		return "", errors.New("the end time must be after the start time")
	}
	return ekCreate(ev)
}

// Find reads one event by uid; false when it names nothing on that calendar.
func Find(calendarName, uid string) (Occurrence, bool, error) {
	if calendarName == "" || uid == "" {
		return Occurrence{}, false, errors.New("finding needs a calendar and a uid")
	}
	return ekFind(calendarName, uid)
}

// Delete removes an event by uid, and reports whether anything matched. A uid that is already
// gone is not an error, which is what lets a cleanup path run unconditionally.
func Delete(calendarName, uid string) (bool, error) {
	if calendarName == "" || uid == "" {
		return false, errors.New("deleting needs a calendar and a uid")
	}
	return ekDelete(calendarName, uid)
}

// A location field is usually a room, but a pasted Zoom link brings a pwd= token with it, and a
// meeting passcode is a live credential rather than a detail worth reporting.
var secretPattern = regexp.MustCompile(`(?i)[?&](pwd|password|passcode)=[^&\s]*`)

func scrubSecrets(text string) string {
	return secretPattern.ReplaceAllString(text, "")
}
