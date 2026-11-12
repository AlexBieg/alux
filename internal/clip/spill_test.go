package clip

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spillTo(t *testing.T) string {
	t.Helper()
	was := SpillDir
	SpillDir = t.TempDir()
	t.Cleanup(func() { SpillDir = was })
	return SpillDir
}

func numbered(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func TestOutputThatFitsIsReturnedWholeAndNeverTouchesDisk(t *testing.T) {
	dir := spillTo(t)
	s := NewSpill(1000)
	fmt.Fprint(s, numbered(1, 8))
	s.Close()
	if got := s.Whole(4, 4); got != numbered(1, 8) {
		t.Errorf("Whole = %q, want all eight lines", got)
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Errorf("output that fit left %d spill files", len(files))
	}
}

func TestTooManyLinesKeepsTheEndsAndSavesTheRest(t *testing.T) {
	spillTo(t)
	s := NewSpill(1000)
	fmt.Fprint(s, numbered(1, 9))
	s.Close()
	got := s.Whole(2, 3)
	if !strings.HasPrefix(got, "1\n2\n[… 4 lines cut") || !strings.HasSuffix(got, "…]\n7\n8\n9\n") {
		t.Fatalf("Whole = %q, want lines 1-2 and 7-9 around a notice", got)
	}
	if !strings.Contains(got, "lines 3–6 cut") {
		t.Errorf("the notice does not name lines 3–6: %q", got)
	}
}

// A held command is read in pieces, and each piece's notice counts lines in the whole file.
func TestEachTakeIsShapedOnItsOwnWithFileLineNumbers(t *testing.T) {
	spillTo(t)
	s := NewSpill(1000)
	fmt.Fprint(s, numbered(1, 3))
	if got := s.Take(1, 1); !strings.HasPrefix(got, "1\n[… 1 lines cut") {
		t.Fatalf("first Take = %q", got)
	}
	fmt.Fprint(s, numbered(4, 10))
	got := s.Take(1, 1)
	if !strings.HasPrefix(got, "4\n") || !strings.HasSuffix(got, "\n10\n") || !strings.Contains(got, "lines 5–9 cut") {
		t.Errorf("second Take = %q, want 4 and 10 around lines 5–9 cut", got)
	}
	if got := s.Take(1, 1); got != "" {
		t.Errorf("a Take with nothing new = %q", got)
	}
}

func TestOneEndlessLineStaysWithinTheBudget(t *testing.T) {
	spillTo(t)
	s := NewSpill(3000)
	fmt.Fprint(s, strings.Repeat("é", 50_000))
	s.Close()
	got := s.Whole(DefaultHead, DefaultTail)
	if len(got) > 3500 {
		t.Errorf("a single long line came back as %d bytes, want about 3000", len(got))
	}
	if !strings.Contains(got, "the full output is in "+SpillDir) {
		t.Errorf("the notice does not name the spill file: %q", got[:200])
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("a cut split a rune")
	}
}

func TestOldSpillFilesArePrunedWhenANewOneIsMade(t *testing.T) {
	dir := spillTo(t)
	old := filepath.Join(dir, "bash-old.log")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ago := time.Now().Add(-2 * spillAge)
	if err := os.Chtimes(old, ago, ago); err != nil {
		t.Fatal(err)
	}
	s := NewSpill(100)
	fmt.Fprint(s, strings.Repeat("y\n", 200))
	s.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a spill file older than a day was left behind")
	}
}
