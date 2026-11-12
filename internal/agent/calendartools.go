package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/calendar"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
)

// Writing to the calendar is the one builtin whose effect leaves the machine: an event on a
// work calendar is visible to colleagues and cannot be taken back by undoing a file. So both
// write tools stop for approval, and the preview shows the event as it will appear; reading
// changes nothing and does not.

const calendarTimeLayout = "2006-01-02 15:04"

// CalendarOn reads settings.json's {"calendar": true} on each call.
func CalendarOn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		Calendar bool `json:"calendar"`
	}
	return json.Unmarshal(data, &settings) == nil && settings.Calendar
}

// CalendarTools is empty off macOS, so the feature disappears rather than offering a tool that
// always fails.
func CalendarTools() []anthropic.ToolUnionParam {
	if runtime.GOOS != "darwin" || !CalendarOn() {
		return nil
	}
	return []anthropic.ToolUnionParam{
		{OfTool: &calendarListTool},
		{OfTool: &calendarEventsTool},
		{OfTool: &calendarCreateTool},
		{OfTool: &calendarDeleteTool},
	}
}

func CalendarHandlers() map[string]Handler {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return map[string]Handler{
		"calendar_list_writable": runCalendarList,
		"calendar_list_events":   runCalendarEvents,
		"calendar_create_event":  runCalendarCreate,
		"calendar_delete_event":  runCalendarDelete,
	}
}

var calendarListTool = anthropic.ToolParam{
	Name: "calendar_list_writable",
	Description: anthropic.String("List the calendars that can be written to, by name, and say which of them a " +
		"new event defaults to. Read-only and quick. Use it before creating an event when which calendar to use " +
		"is in question — a work calendar others can see is a different thing from a personal one."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties:  map[string]any{},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}

var calendarEventsTool = anthropic.ToolParam{
	Name: "calendar_list_events",
	Description: anthropic.String("Read what is on the user's calendar between two times. It reads the calendar " +
		"store on this machine directly — the same events the user sees in the Calendar app, with a recurring " +
		"meeting resolved to the occurrence that actually falls in the window — so it is the authoritative answer " +
		"to what the schedule holds. Use it for any question about the schedule, and do not reach for another " +
		"calendar integration in its place: one wired to a different account answers an empty day for a full one, " +
		"and an empty answer reads exactly like a free day.\n\n" +
		"Times are local wall-clock, written as 'YYYY-MM-DD HH:MM' in 24-hour form. An event is listed when it " +
		"overlaps the window, so a meeting already under way at the start of it is included. Omitting calendar " +
		"reads every calendar the user can write to, which is what 'my calendar' usually means; holidays and " +
		"birthdays are not among them, and naming a calendar narrows the read to that one.\n\n" +
		"Read-only, quick, and no approval is asked for it, so widening the window and reading again costs little. " +
		"Event notes are never returned: invite bodies carry meeting passcodes, which are live credentials."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"start": map[string]any{"type": "string", "description": "Start of the window, local, as 'YYYY-MM-DD HH:MM', 24-hour."},
			"end":   map[string]any{"type": "string", "description": "End of the window, local, as 'YYYY-MM-DD HH:MM', 24-hour. Must be after start."},
			"calendar": map[string]any{
				"type":        "string",
				"description": "Read only this calendar, by name. Omitting it reads every writable calendar at once.",
			},
		},
		Required:    []string{"start", "end"},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}

var calendarCreateTool = anthropic.ToolParam{
	Name: "calendar_create_event",
	Description: anthropic.String("Create an event in the macOS Calendar app. An event on a synced calendar " +
		"(a Google work calendar, say) propagates to the server and becomes visible to anyone that calendar is " +
		"shared with, so treat it as a real-world action rather than a local note. Outside auto mode the user is " +
		"asked to approve each one; in auto mode it runs unasked.\n\n" +
		"Times are local wall-clock, written as 'YYYY-MM-DD HH:MM' in 24-hour form. Call it once per event; there " +
		"is no recurrence support, so a weekly series is either several calls or a job for the Calendar UI.\n\n" +
		"Name the calendar. Omitting it falls back to the calendar new events default to, which says where the " +
		"user puts an event they made themselves and nothing about where this one belongs — a work event landing " +
		"on a personal calendar is invisible to everyone who needed to see it. Call calendar_list_writable when " +
		"you are not certain. The returned uid is what calendar_delete_event takes."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"title": map[string]any{"type": "string", "description": "The event's title, as it will read in the calendar."},
			"start": map[string]any{"type": "string", "description": "Local start time as 'YYYY-MM-DD HH:MM', 24-hour."},
			"end":   map[string]any{"type": "string", "description": "Local end time as 'YYYY-MM-DD HH:MM', 24-hour. Must be after start."},
			"calendar": map[string]any{
				"type":        "string",
				"description": "Which calendar to write to, by name. Omitting it falls back to the default calendar, which is a guess.",
			},
			"location":    map[string]any{"type": "string", "description": "Optional location."},
			"description": map[string]any{"type": "string", "description": "Optional notes on the event."},
			"all_day":     map[string]any{"type": "boolean", "description": "Make it an all-day event. start and end are still required and end must be after start (00:00 to 23:59 for one day); only their dates are kept."},
			"alarm_minutes": map[string]any{
				"type":        "integer",
				"description": "Alert this many minutes before the start. Omit or 0 for no alert.",
			},
		},
		Required:    []string{"title", "start", "end"},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}

var calendarDeleteTool = anthropic.ToolParam{
	Name: "calendar_delete_event",
	Description: anthropic.String("Delete a calendar event by uid, as returned by calendar_create_event. Deleting " +
		"propagates to the server the same way creating does. It is meant for undoing an event this session " +
		"created: calendar_list_events reports the schedule but not uids, so a uid you were not given is not " +
		"something you can discover. A uid belonging to a recurring meeting is refused rather than deleted, " +
		"because the occurrence that would go is not the one anyone meant."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"uid":      map[string]any{"type": "string", "description": "The event's uid, as returned when it was created."},
			"calendar": map[string]any{"type": "string", "description": "The calendar holding it — the same one it was created on."},
		},
		Required:    []string{"uid"},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}

// An omitted name falls back to the calendar new events default to, and to the first writable
// one when the store names none. Both are a guess at which calendar was meant: the default is
// the one the user writes personal events on as often as not, and a work event landing on a
// personal calendar is invisible to everyone who needed to see it. The approval preview names
// the calendar for exactly this reason, and the tool descriptions push the model to choose
// rather than lean on the fallback.
func resolveCalendar(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	if preferred, err := calendar.Default(); err == nil && preferred != "" {
		return preferred, nil
	}
	writable, err := calendar.Writable()
	if err != nil {
		return "", err
	}
	if len(writable) == 0 {
		return "", fmt.Errorf("no writable calendar found")
	}
	return writable[0], nil
}

func parseLocal(value string) (time.Time, error) {
	return time.ParseInLocation(calendarTimeLayout, strings.TrimSpace(value), time.Local)
}

// Listing changes nothing, so like the other read-only tools it skips the approval prompt.
func runCalendarList(_ context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	label := "calendar: list writable"
	cb.toolStart(call.ID, label, KindCalendar)
	writable, err := calendar.Writable()
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindCalendar)
		return failed(call.ID, err)
	}
	if len(writable) == 0 {
		output := "No writable calendars."
		cb.toolResult(call.ID, label, output, KindCalendar)
		return ok(call.ID, output)
	}
	output := strings.Join(writable, "\n")
	if preferred, err := calendar.Default(); err == nil && preferred != "" {
		output += "\n\nA new event defaults to " + preferred + "."
	}
	cb.toolResult(call.ID, label, output, KindCalendar)
	return ok(call.ID, output)
}

// Reading changes nothing, so it skips the approval prompt the write tools stop for.
func runCalendarEvents(_ context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		Calendar string `json:"calendar"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	start, err := parseLocal(in.Start)
	if err != nil {
		return failed(call.ID, fmt.Errorf("start must look like 2026-03-04 14:00: %w", err))
	}
	end, err := parseLocal(in.End)
	if err != nil {
		return failed(call.ID, fmt.Errorf("end must look like 2026-03-04 18:00: %w", err))
	}

	names := []string{in.Calendar}
	if in.Calendar == "" {
		// An omitted calendar means the user's schedule as a whole, and the first writable one is a
		// guess that is as likely to be an empty local calendar as the work one.
		names, err = calendar.Writable()
		if err != nil {
			return failed(call.ID, err)
		}
	}

	label := "calendar: events " + describeWhen(start, end, false)
	cb.toolStart(call.ID, label, KindCalendar)
	var rows []scheduleRow
	for _, name := range names {
		occurrences, err := calendar.Events(name, start, end)
		if err != nil {
			cb.toolResult(call.ID, label, err.Error(), KindCalendar)
			return failed(call.ID, err)
		}
		for _, occ := range occurrences {
			rows = append(rows, scheduleRow{calendar: name, occurrence: occ})
		}
	}

	output := formatSchedule(rows, names, start, end)
	cb.toolResult(call.ID, label, output, KindCalendar)
	return ok(call.ID, output)
}

type scheduleRow struct {
	calendar   string
	occurrence calendar.Occurrence
}

// A line names its calendar only when more than one was read: an Occurrence does not carry one,
// and a merged day where a work meeting and a personal one look alike is a worse answer than a
// slightly longer line.
func formatSchedule(rows []scheduleRow, names []string, from, to time.Time) string {
	where := strings.Join(names, ", ")
	if len(rows) == 0 {
		return fmt.Sprintf("No events on %s between %s and %s.",
			where, from.Format("Mon 2 Jan 2006, 15:04"), to.Format("Mon 2 Jan 2006, 15:04"))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].occurrence.Start.Before(rows[j].occurrence.Start)
	})

	// Naming the calendar on every line is only worth its width when the rows actually come from
	// more than one. Reading four calendars of which one has events is the common case.
	sources := map[string]bool{}
	for _, row := range rows {
		sources[row.calendar] = true
	}
	labelled := len(sources) > 1

	var out []string
	day := ""
	for _, row := range rows {
		occ := row.occurrence
		if header := occ.Start.Format("Mon 2 Jan 2006"); header != day {
			day = header
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, day)
		}
		line := "  " + padWhen(occurrenceWhen(occ)) + "  " + occ.Title
		if occ.Location != "" {
			line += " — " + occ.Location
		}
		if labelled {
			line += " [" + row.calendar + "]"
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func occurrenceWhen(occ calendar.Occurrence) string {
	if !occ.AllDay {
		return occ.Start.Format("15:04") + "–" + occ.End.Format("15:04")
	}
	// An all-day event ends a second before midnight, so a single day measures just under 24 hours
	// and only a genuinely multi-day one needs its last day spelled out.
	if occ.End.Sub(occ.Start) >= 24*time.Hour {
		return "all day, to " + occ.End.Format("2 Jan")
	}
	return "all day"
}

// Titles line up only if the time column is padded by runes: the en dash in a range is three
// bytes, so a %-11s verb would leave the column two columns short.
func padWhen(when string) string {
	if n := len("09:15-09:30") - utf8.RuneCountInString(when); n > 0 {
		return when + strings.Repeat(" ", n)
	}
	return when
}

func runCalendarCreate(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Title        string `json:"title"`
		Start        string `json:"start"`
		End          string `json:"end"`
		Calendar     string `json:"calendar"`
		Location     string `json:"location"`
		Description  string `json:"description"`
		AllDay       bool   `json:"all_day"`
		AlarmMinutes int    `json:"alarm_minutes"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	start, err := parseLocal(in.Start)
	if err != nil {
		return failed(call.ID, fmt.Errorf("start must look like 2026-03-04 14:00: %w", err))
	}
	end, err := parseLocal(in.End)
	if err != nil {
		return failed(call.ID, fmt.Errorf("end must look like 2026-03-04 14:30: %w", err))
	}
	calName, err := resolveCalendar(in.Calendar)
	if err != nil {
		return failed(call.ID, err)
	}

	ev := calendar.Event{
		Calendar:     calName,
		Title:        in.Title,
		Start:        start,
		End:          end,
		Location:     in.Location,
		Description:  in.Description,
		AllDay:       in.AllDay,
		AlarmMinutes: in.AlarmMinutes,
	}

	label := "calendar: " + in.Title + " — " + describeWhen(start, end, in.AllDay)
	if !cb.Approve(ctx, label, previewEvent(ev)) {
		return declined(call.ID, "calendar event")
	}
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	cb.toolStart(call.ID, label, KindCalendar)

	uid, err := calendar.Create(ev)
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindCalendar)
		return failed(call.ID, err)
	}
	// The uid is said back because it is the only handle on the event afterwards, and nothing here
	// can look one up.
	output := fmt.Sprintf("Created %q on %s, %s.\nuid: %s", in.Title, calName, describeWhen(start, end, in.AllDay), uid)
	cb.toolResult(call.ID, label, output, KindCalendar)
	return ok(call.ID, output)
}

func runCalendarDelete(ctx context.Context, call Call, cb Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		UID      string `json:"uid"`
		Calendar string `json:"calendar"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return failed(call.ID, err)
	}
	calName, err := resolveCalendar(in.Calendar)
	if err != nil {
		return failed(call.ID, err)
	}
	label := "calendar: delete event " + in.UID + " on " + calName
	if occ, found, err := calendar.Find(calName, in.UID); err == nil && found {
		label = "calendar: delete " + occ.Title + " — " + describeWhen(occ.Start, occ.End, occ.AllDay) + " on " + calName
	}
	if !cb.Approve(ctx, label, "uid: "+in.UID) {
		return declined(call.ID, "calendar deletion")
	}
	if ctx.Err() != nil {
		return interruptedResult(call.ID)
	}
	cb.toolStart(call.ID, label, KindCalendar)

	found, err := calendar.Delete(calName, in.UID)
	if err != nil {
		cb.toolResult(call.ID, label, err.Error(), KindCalendar)
		return failed(call.ID, err)
	}
	output := "Deleted."
	if !found {
		output = "No event with that uid on " + calName + " — nothing deleted."
	}
	cb.toolResult(call.ID, label, output, KindCalendar)
	return ok(call.ID, output)
}

func describeWhen(start, end time.Time, allDay bool) string {
	if allDay {
		if start.Format("2006-01-02") == end.Format("2006-01-02") {
			return start.Format("Mon 2 Jan 2006") + ", all day"
		}
		return start.Format("Mon 2 Jan 2006") + " to " + end.Format("Mon 2 Jan 2006") + ", all day"
	}
	when := start.Format("Mon 2 Jan 2006, 15:04") + "–" + end.Format("15:04")
	if start.Format("2006-01-02") != end.Format("2006-01-02") {
		when = start.Format("Mon 2 Jan 2006, 15:04") + " to " + end.Format("Mon 2 Jan 2006, 15:04")
	}
	return when
}

// The preview is what the user approves against, so it shows every field that will be written —
// an event is easiest to check as the thing it will become, not as a line of arguments.
func previewEvent(ev calendar.Event) string {
	lines := []string{
		"Calendar:    " + ev.Calendar,
		"Title:       " + ev.Title,
		"When:        " + describeWhen(ev.Start, ev.End, ev.AllDay),
	}
	if ev.Location != "" {
		lines = append(lines, "Location:    "+ev.Location)
	}
	if ev.AlarmMinutes > 0 {
		lines = append(lines, fmt.Sprintf("Alert:       %d minutes before", ev.AlarmMinutes))
	}
	if ev.Description != "" {
		lines = append(lines, "Description: "+ev.Description)
	}
	return strings.Join(lines, "\n")
}
