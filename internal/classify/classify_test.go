package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/paths"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  string
		ok    bool
	}{
		{"$ go build ./...", "go build ./...", true},
		{"$ tail -f server.log &", "tail -f server.log", true},
		{"$ npm run dev & · check-in 5m or /Ready/", "npm run dev", true},
		{"poll every 30s until exit 0, gives up after 30m: curl -s localhost", "", false},
		{"$ cat > f <<EOF\nx\nEOF", "cat > f <<EOF\nx\nEOF", true},
		{"output of proc 3", "", false},
		{"kill proc 3", "", false},
		{"$ ", "", false},
		{"", "", false},
	} {
		got, ok := Command(tc.label)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Command(%q) = %q, %v; want %q, %v", tc.label, got, ok, tc.want, tc.ok)
		}
	}
}

// A nil client is what alux holds when the feature is unconfigured, and every bash
// command still goes through Classify.
func TestNilClientClassifies(t *testing.T) {
	var c *Client
	if got := c.Classify(context.Background(), "rm -rf /"); got != Unknown {
		t.Fatalf("nil client returned %v, want Unknown", got)
	}
	c.Close()
}

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	port, err := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parsing test server port: %v", err)
	}
	c := &Client{
		conf:  Conf{Port: port},
		http:  srv.Client(),
		ready: make(chan struct{}),
	}
	close(c.ready)
	return c
}

func TestClassifyVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Verdict
	}{
		{"write", `{"writes": 0.97}`, Writes},
		{"read", `{"writes": 0.02}`, Reads},
		{"boundary is a write", `{"writes": 0.5}`, Writes},
		{"missing field", `{"ms": 12}`, Unknown},
		{"malformed", `not json`, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Command string `json:"command"`
				}
				if json.NewDecoder(r.Body).Decode(&in) != nil || in.Command == "" {
					t.Errorf("sidecar got no command")
				}
				w.Write([]byte(tc.body))
			})
			if got := c.Classify(context.Background(), "gofmt -w x.go"); got != tc.want {
				t.Errorf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// One failure is one condition, not one per command: the notice fires once however many
// commands run afterwards, or the transcript fills with the same line.
func TestFailureNotifiesOnce(t *testing.T) {
	notices := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c.onError = func(string) { notices++ }
	for range 3 {
		if got := c.Classify(context.Background(), "ls"); got != Unknown {
			t.Fatalf("verdict = %v, want Unknown", got)
		}
	}
	if notices != 1 {
		t.Errorf("notified %d times, want 1", notices)
	}
}

func TestClassifyRespectsCancelledContext(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"writes": 0.99}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.Classify(ctx, "rm -rf build"); got != Unknown {
		t.Errorf("verdict = %v, want Unknown", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, paths.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		path := filepath.Join(dir, paths.Config, "settings.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`{}`)
	if _, ok := Config(); ok {
		t.Error("no classify block should read as off")
	}

	write(`{"classify": {"python": "/p/python", "model": "/m"}}`)
	conf, ok := Config()
	if !ok {
		t.Fatal("configured block should read as on")
	}
	if conf.Port != defaultPort || conf.Device != "mps" {
		t.Errorf("defaults not applied: %+v", conf)
	}

	write(`{"classify": {"model": "/m"}}`)
	if _, ok := Config(); ok {
		t.Error("a block with no python should read as off")
	}
}
