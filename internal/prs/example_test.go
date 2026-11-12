package prs

import (
	"os"
	"testing"
)

func TestTheExampleSettingsFileParses(t *testing.T) {
	data, err := os.ReadFile("../../settings.example.json")
	if err != nil {
		t.Fatal(err)
	}
	conf, ok, err := Parse(data)
	if err != nil || !ok {
		t.Fatalf("Parse = %v, %v", ok, err)
	}
	if len(conf.Repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(conf.Repos))
	}
}
