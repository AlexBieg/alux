package ui

import (
	"os"
	"testing"

	"github.com/alexbieg/alux/internal/reviewstore"
)

// Every agent loads rules, which would otherwise read the user's own record store.
func TestMain(m *testing.M) {
	os.Setenv("ALUX_RECORDS", "off")
	logDismissal = func(reviewstore.Dismissal) error { return nil }
	os.Exit(m.Run())
}
