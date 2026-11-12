package ui

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/resolve"
)

const prTitle = "Make export rows optional"

func TestAKnownIDIsUnderlinedWithoutChangingTheText(t *testing.T) {
	m, s := harness(t, 80, 20)
	m.Names = &resolve.Resolver{}
	m.Names.Learn("#4321", prTitle)
	s.Show("| pr | what |\n|---|---|\n| #4321 | rows |")
	settle(m)
	plain := screen(m)
	send(m, NamesMsg{})
	if screen(m) != plain {
		t.Fatalf("a name changed the text:\n%s\nwas\n%s", screen(m), plain)
	}
	if !strings.Contains(strings.Join(m.content.lines, "\n"), "\x1b[4m#4321") {
		t.Fatalf("id is not underlined: %q", m.content.lines)
	}
}

func TestClickingANamedIDShowsItsName(t *testing.T) {
	m, s := harness(t, 80, 20)
	m.Names = &resolve.Resolver{}
	m.Names.Learn("#4321", prTitle)
	s.Show("see #4321 here")
	settle(m)
	send(m, NamesMsg{})
	rows := strings.Split(screen(m), "\n")
	row := slices.IndexFunc(rows, func(r string) bool { return strings.Contains(r, "see #4321") })
	x := strings.Index(rows[row], "#4321") + 2
	clickAt(m, x, row)
	releaseAt(m, x, row)
	settle(m)
	if !strings.Contains(screen(m), "#4321 — "+prTitle) {
		t.Fatalf("click did not show the name:\n%s", screen(m))
	}
}

func TestAMissIsDrawnAsBeforeAndLookedUpOnce(t *testing.T) {
	var calls atomic.Int32
	m, s := harness(t, 80, 20)
	m.Names = &resolve.Resolver{Repo: "acme/web", Gh: func(context.Context, ...string) (string, error) {
		calls.Add(1)
		return "", errors.New("not found")
	}}
	s.Show("see #4242 now")
	settle(m)
	settle(m)
	m.Names.Wait()
	send(m, NamesMsg{})
	if calls.Load() != 1 {
		t.Fatalf("looked up %d times", calls.Load())
	}
	if !strings.Contains(screen(m), "see #4242 now") {
		t.Fatalf("a miss changed the text:\n%s", screen(m))
	}
}

func TestANameRedrawsOnlyTheItemsThatMentionIt(t *testing.T) {
	m, s := harness(t, 80, 20)
	m.Names = &resolve.Resolver{}
	s.Show("about #4321")
	s.Show("nothing to name here")
	settle(m)
	mentions, other := m.snap.Items[0].ID, m.snap.Items[1].ID
	before := keysFor(m, other)
	m.Names.Learn("#4321", prTitle)
	send(m, NamesMsg{})
	if after := keysFor(m, other); !slices.Equal(before, after) {
		t.Fatalf("an item that does not mention the id was re-rendered: %v then %v", before, after)
	}
	if n := len(keysFor(m, mentions)); n != 2 {
		t.Fatalf("the mentioning item has %d blocks, want a second one for the name", n)
	}
	if !strings.Contains(strings.Join(m.content.lines, "\n"), "\x1b[4m#4321") {
		t.Fatalf("id not underlined: %q", m.content.lines)
	}
}

func keysFor(m *Model, id int) []blockKey {
	var keys []blockKey
	for k := range m.blocks {
		if k.id == id {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b blockKey) int { return cmp.Compare(a.names, b.names) })
	return keys
}

func TestANameArrivingMidSelectionWaitsForRelease(t *testing.T) {
	m, s := harness(t, 80, 20)
	m.Names = &resolve.Resolver{}
	s.Show("see #4321 here")
	settle(m)
	row := slices.IndexFunc(strings.Split(screen(m), "\n"), func(r string) bool { return strings.Contains(r, "see #4321") })
	clickAt(m, 2, row)
	send(m, tea.MouseMotionMsg{Button: tea.MouseLeft, X: 8, Y: row})
	held := slices.Clone(m.content.lines)

	m.Names.Learn("#4321", prTitle)
	send(m, NamesMsg{})
	settle(m)
	releaseAt(m, 8, row)
	if !slices.Equal(held, m.content.lines) {
		t.Fatalf("lines moved under the selection:\n%q\n%q", held, m.content.lines)
	}
	press(m, "esc")
	if !strings.Contains(strings.Join(m.content.lines, "\n"), "\x1b[4m#4321") {
		t.Fatalf("held name not applied after the selection closed: %q", m.content.lines)
	}
}

func TestAStreamingReplyStartsItsLookups(t *testing.T) {
	var calls atomic.Int32
	m, _ := harness(t, 80, 20)
	m.Names = &resolve.Resolver{Repo: "acme/web", Gh: func(context.Context, ...string) (string, error) {
		calls.Add(1)
		return `{"title":"x"}`, nil
	}}
	m.snap.Live = "looking at #4242"
	m.layout()
	m.Names.Wait()
	if calls.Load() != 1 || m.Names.Known()["#4242"].State != resolve.Known {
		t.Fatalf("%d lookups, entry %+v", calls.Load(), m.Names.Known()["#4242"])
	}
}
