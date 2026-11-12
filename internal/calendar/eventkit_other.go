//go:build !darwin

package calendar

import (
	"errors"
	"time"
)

var errNotMacOS = errors.New("the calendar is macOS only")

func ekWritable() ([]string, error) { return nil, errNotMacOS }

func ekDefaultCalendar() (string, error) { return "", errNotMacOS }

func ekEvents(string, time.Time, time.Time) ([]Occurrence, error) { return nil, errNotMacOS }

func ekCreate(Event) (string, error) { return "", errNotMacOS }

func ekFind(string, string) (Occurrence, bool, error) { return Occurrence{}, false, errNotMacOS }

func ekDelete(string, string) (bool, error) { return false, errNotMacOS }
