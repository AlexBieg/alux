package diff

import (
	"reflect"
	"testing"
)

func TestNewLinesCountsTheNewSideAcrossHunks(t *testing.T) {
	patch := "@@ -1,3 +1,3 @@\n keep\n-old\n+new\n keep\n@@ -40,2 +40,3 @@ func F() {\n a\n+b\n\\ No newline at end of file\n c\n"
	want := []int{0, 1, 0, 2, 3, 0, 40, 41, 0, 42}
	if got := NewLines(patch); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
