package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexbieg/alux/internal/session"
	"github.com/charmbracelet/x/ansi"
)

func TestWelcomeShowsOnAnEmptyChatAndGoesWithTheFirstPrompt(t *testing.T) {
	m, s := harness(t, 80, 40)
	m.Setup = &Setup{Project: "acme"}
	m.layout()
	if !strings.Contains(screen(m), "shift+tab mode") {
		t.Fatalf("no welcome on an empty chat:\n%s", screen(m))
	}

	s.PushUserForTest("hello")
	settle(m)
	if strings.Contains(screen(m), "shift+tab mode") {
		t.Errorf("welcome still shown after the first prompt:\n%s", screen(m))
	}

	s.NewSession()
	settle(m)
	if !strings.Contains(screen(m), "shift+tab mode") {
		t.Errorf("no welcome after a new session:\n%s", screen(m))
	}
}

func TestTheWelcomeCalloutDescribesTheCurrentMode(t *testing.T) {
	m, s := harness(t, 80, 40)
	m.Setup = &Setup{Project: "acme"}
	m.layout()
	for _, mode := range []session.Mode{session.ModeAuto, session.ModeLocal, session.ModeReadOnly} {
		s.SetMode(mode)
		settle(m)
		for _, other := range session.Modes {
			shown := strings.Contains(screen(m), string(other)+" mode:")
			if shown != (other == mode) {
				t.Errorf("in %s mode the %s callout shown = %v:\n%s", mode, other, shown, screen(m))
			}
		}
	}
}

// He greets from the sidebar's foot, where he stays, so a notice during the welcome is his to say.
func TestTheWelcomeMascotStandsInTheSidebarAndSaysNotices(t *testing.T) {
	m, _ := harness(t, 140, 40)
	m.Setup = &Setup{}
	m.layout()
	if n := strings.Count(screen(m), aluxHatRow); n != 1 || aluxAt(m) < m.mainWidth() {
		t.Fatalf("want him alone in the sidebar, found %d hats, his at column %d:\n%s", n, aluxAt(m), screen(m))
	}
	m.Notify("mcp: linear connected")
	row := rowContaining(t, m, "mcp: linear connected")
	line := strings.Split(screen(m), "\n")[row]
	if col := ansi.StringWidth(line[:strings.Index(line, "mcp:")]); col < m.mainWidth() {
		t.Errorf("the welcome's notice is at column %d, want his bubble in the sidebar:\n%s", col, screen(m))
	}
}

func TestTheWelcomeMascotWavesThreeTimesThenRests(t *testing.T) {
	m, s := harness(t, 140, 40)
	m.Setup = &Setup{}
	m.layout()

	waves, prev := 0, false
	for range welcomeWaveTicks + aluxStepEvery {
		up := strings.Contains(screen(m), aluxGreetUp)
		if up && !prev {
			waves++
		}
		prev = up
		tick(m)
	}
	if waves != 3 {
		t.Errorf("he waved %d times, want 3", waves)
	}
	if out := screen(m); strings.Contains(out, aluxGreetUp) || strings.Contains(out, aluxGreetDown) || !strings.Contains(out, aluxEyesUp) {
		t.Errorf("he is not at rest after waving:\n%s", out)
	}

	s.PushUserForTest("hello")
	settle(m)
	s.NewSession()
	settle(m)
	if !strings.Contains(screen(m), aluxGreetUp) {
		t.Errorf("he did not wave again after a new session:\n%s", screen(m))
	}
}

func TestTheGreetingNamesTheUserOnlyWhenTheAccountHasAName(t *testing.T) {
	at := func(hour int) func() time.Time {
		return func() time.Time { return time.Date(2026, 10, 29, hour, 0, 0, 0, time.Local) }
	}
	cases := []struct {
		hour int
		name string
		want string
	}{
		{9, "", "good morning"},
		{14, "Alex Smith", "good afternoon, Alex"},
		{20, "", "good evening"},
		{20, "   ", "good evening"},
	}
	for _, c := range cases {
		m, _ := harness(t, 80, 40)
		m.Setup = &Setup{}
		m.now, m.account = at(c.hour), func() string { return c.name }
		m.layout()
		found := false
		for _, line := range strings.Split(screen(m), "\n") {
			if _, rest, ok := strings.Cut(line, c.want); ok && (rest == "" || rest[0] == ' ') {
				found = true
			}
		}
		if !found {
			t.Errorf("hour %d, name %q: greeting is not %q:\n%s", c.hour, c.name, c.want, screen(m))
		}
	}
}

func TestTheChecklistCollapsesWhenNothingFailsAndExpandsOnACross(t *testing.T) {
	m, _ := harness(t, 120, 40)
	m.Setup = &Setup{Records: "/tmp/records.db"}
	send(m, GuardianMsg{})
	want := "✓ guardian  ✓ rg and fd  ✓ API key  ✓ records  · vault, PRs, calendar not set up"
	if out := screen(m); !strings.Contains(out, want) || strings.Contains(out, "Ollama is up") {
		t.Errorf("checklist did not collapse to one line:\n%s", out)
	}

	m.Setup.AuthWarning = "no API key; run `alux login`"
	m.layout()
	out := screen(m)
	for _, want := range []string{"✗ API key", "run `alux login`", "✓ guardian", "Ollama is up and the model is pulled"} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded checklist missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not set up  ") || strings.Contains(out, "✓ guardian  ✓") {
		t.Errorf("checklist stayed collapsed beside a failing row:\n%s", out)
	}
}

func TestAFailingChecklistRowShowsItsFix(t *testing.T) {
	m, _ := harness(t, 80, 40)
	m.Setup = &Setup{Missing: []string{"fd"}}
	m.layout()
	if !strings.Contains(screen(m), "checking…") {
		t.Errorf("guardian row does not say it is checking:\n%s", screen(m))
	}

	send(m, GuardianMsg{Err: errors.New("guardian: model m is not pulled; run `ollama pull m`")})
	out := screen(m)
	for _, want := range []string{"✗ guardian", "run `ollama pull m`", "✗ rg and fd", "fd not found on PATH; run `brew install ripgrep fd`"} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "checking…") {
		t.Errorf("guardian row still checking after its answer:\n%s", out)
	}
}
