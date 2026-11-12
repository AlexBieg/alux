//go:build darwin

package calendar

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework EventKit -framework Foundation
#include <stdlib.h>
#include "ek.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"math"
	"time"
	"unsafe"
)

// take consumes a string the Objective-C side allocated, which is the whole of the ownership
// rule across this boundary: every char * handed back is freed here, once.
func take(s *C.char) string {
	if s == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s)
}

func takeError(cerr *C.char, fallback string) error {
	if msg := take(cerr); msg != "" {
		return errors.New(msg)
	}
	return errors.New(fallback)
}

func ekWritable() ([]string, error) {
	var cerr *C.char
	raw := C.ek_writable(&cerr)
	if raw == nil {
		return nil, takeError(cerr, "the writable calendars could not be read")
	}
	var titles []string
	if err := json.Unmarshal([]byte(take(raw)), &titles); err != nil {
		return nil, err
	}
	return titles, nil
}

func ekDefaultCalendar() (string, error) {
	var cerr *C.char
	raw := C.ek_default_calendar(&cerr)
	if raw == nil {
		return "", takeError(cerr, "the default calendar could not be read")
	}
	return take(raw), nil
}

type ekRow struct {
	UID      string  `json:"uid"`
	Title    string  `json:"title"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Location string  `json:"location"`
	AllDay   bool    `json:"allDay"`
}

func ekEvents(calendarName string, from, to time.Time) ([]Occurrence, error) {
	name := C.CString(calendarName)
	defer C.free(unsafe.Pointer(name))

	var cerr *C.char
	raw := C.ek_events(name, C.double(float64(from.UnixNano())/1e9), C.double(float64(to.UnixNano())/1e9), &cerr)
	if raw == nil {
		return nil, takeError(cerr, "the calendar could not be read")
	}
	var rows []ekRow
	if err := json.Unmarshal([]byte(take(raw)), &rows); err != nil {
		return nil, err
	}
	out := make([]Occurrence, 0, len(rows))
	for _, row := range rows {
		out = append(out, Occurrence{
			UID:      row.UID,
			Title:    row.Title,
			Start:    epoch(row.Start),
			End:      epoch(row.End),
			Location: scrubSecrets(row.Location),
			AllDay:   row.AllDay,
		})
	}
	return out, nil
}

func epoch(seconds float64) time.Time {
	return time.Unix(int64(math.Round(seconds)), 0).In(time.Local)
}

func ekCreate(ev Event) (string, error) {
	name := C.CString(ev.Calendar)
	defer C.free(unsafe.Pointer(name))
	title := C.CString(ev.Title)
	defer C.free(unsafe.Pointer(title))
	location := C.CString(ev.Location)
	defer C.free(unsafe.Pointer(location))
	notes := C.CString(ev.Description)
	defer C.free(unsafe.Pointer(notes))

	allDay := C.int(0)
	if ev.AllDay {
		allDay = 1
	}
	var cerr *C.char
	raw := C.ek_create(name, title, location, notes,
		C.double(float64(ev.Start.UnixNano())/1e9), C.double(float64(ev.End.UnixNano())/1e9),
		allDay, C.int(ev.AlarmMinutes), &cerr)
	if raw == nil {
		return "", takeError(cerr, "the event could not be created")
	}
	return take(raw), nil
}

func ekFind(calendarName, uid string) (Occurrence, bool, error) {
	name := C.CString(calendarName)
	defer C.free(unsafe.Pointer(name))
	id := C.CString(uid)
	defer C.free(unsafe.Pointer(id))

	var cerr *C.char
	raw := C.ek_find(name, id, &cerr)
	if raw == nil {
		return Occurrence{}, false, takeError(cerr, "the event could not be read")
	}
	var row *ekRow
	if err := json.Unmarshal([]byte(take(raw)), &row); err != nil {
		return Occurrence{}, false, err
	}
	if row == nil {
		return Occurrence{}, false, nil
	}
	return Occurrence{
		UID:      row.UID,
		Title:    row.Title,
		Start:    epoch(row.Start),
		End:      epoch(row.End),
		Location: scrubSecrets(row.Location),
		AllDay:   row.AllDay,
	}, true, nil
}

func ekDelete(calendarName, uid string) (bool, error) {
	name := C.CString(calendarName)
	defer C.free(unsafe.Pointer(name))
	id := C.CString(uid)
	defer C.free(unsafe.Pointer(id))

	var cerr *C.char
	switch C.ek_delete(name, id, &cerr) {
	case 1:
		return true, nil
	case 0:
		return false, nil
	default:
		return false, takeError(cerr, "the event could not be deleted")
	}
}
