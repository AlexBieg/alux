package clip

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClippingDoesNotSplitAMultiByteRune(t *testing.T) {
	// The leading byte pushes both cuts off the even boundary they would otherwise land on.
	got := Middle("x"+strings.Repeat("é", 30_000), 30_000)
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("a cut landed inside a rune")
	}
}

func TestTheMarkerCountsTheCharactersActuallyCut(t *testing.T) {
	text := strings.Repeat("ab é\n", 10_000)
	got := Middle(text, 3_000)
	head, rest, ok := strings.Cut(got, "\n[… ")
	if !ok {
		t.Fatalf("no marker in %q", got)
	}
	num, tail, ok := strings.Cut(rest, " characters cut from the middle …]\n")
	if !ok {
		t.Fatalf("malformed marker in %q", rest)
	}
	cut, err := strconv.Atoi(num)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, head) || !strings.HasSuffix(text, tail) {
		t.Fatal("output is not a head and tail of the input")
	}
	want := utf8.RuneCountInString(text) - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)
	if cut != want {
		t.Errorf("marker says %d characters cut, want %d", cut, want)
	}
}

func TestCollapsingLeavesOnlyTheLastFrameOfALine(t *testing.T) {
	got := CollapseCRs("downloading 1%\rdownloading 2%\rdownloading 3%\nnext line\r\nlast\n")
	want := "downloading 3%\nnext line\nlast\n"
	if got != want {
		t.Errorf("CollapseCRs = %q, want %q", got, want)
	}
}
