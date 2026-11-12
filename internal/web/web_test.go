package web

import (
	"errors"
	"strings"
	"testing"
)

// The name comes from configuration, so a typo has to arrive as an answerable question rather
// than as a nil provider.
func TestNewNamesTheProvidersItKnows(t *testing.T) {
	_, err := New("bravve", Config{Key: "test-key"})
	if err == nil {
		t.Fatal("an unknown provider was built")
	}
	for _, want := range []string{`"bravve"`, "brave"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

// Without a key a search can only fail at the far end, having spent a round trip to learn what
// was knowable here.
func TestNewRefusesAProviderWithNoKey(t *testing.T) {
	_, err := New("brave", Config{})
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want %v", err, ErrNoKey)
	}
}

// Needing a key is the engine's property and not the package's, so an engine that needs none must
// not be refused for the absence of one.
func TestNewBuildsAKeylessProviderFromNothing(t *testing.T) {
	p, err := New("searxng", Config{})
	if err != nil {
		t.Fatalf("a keyless provider was refused: %v", err)
	}
	if p.Name() != "searxng" {
		t.Errorf("name = %q, want searxng", p.Name())
	}
}
