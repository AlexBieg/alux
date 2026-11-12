package glob

import "testing"

func TestMatchTreatsDoubleStarAsAnyDepth(t *testing.T) {
	for _, c := range []struct {
		glob, file string
		want       bool
	}{
		{"**/models/**", "api/modules/orders/models/order.py", true},
		{"**/models/**", "api/modules/orders/models.py", false},
		{"db/migrations/**", "db/migrations/20261013-validate.py", true},
		{"db/migrations/**", "web/db/migrations/x.py", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "pkg/core/docs/a.md", true},
		{"**/*upload*", "api/modules/orders/services/report_upload.py", true},
		{"apps/**", "apps/web/src/Page.tsx", true},
		{"apps/**/*.tsx", "apps/web/src/A.tsx", true},
		{"internal/**/*.go", "internal/records/migrations/x.go", true},
		{"internal/*.go", "internal/ui/ui.go", false},
	} {
		if got := Match(c.glob, c.file); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.glob, c.file, got, c.want)
		}
	}
}

func TestValidRefusesEmptyAndMalformedSegments(t *testing.T) {
	for glob, want := range map[string]bool{
		"":             false,
		"web/[*.tsx":   false,
		"web/**/*.tsx": true,
		"**":           true,
	} {
		if got := Valid(glob); got != want {
			t.Errorf("Valid(%q) = %v, want %v", glob, got, want)
		}
	}
}
