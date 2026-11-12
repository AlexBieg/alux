package agent

import (
	"strings"

	"github.com/alexbieg/alux/internal/paths"
)

// touchesSessionDir returns why a shell call must go to the person, or "" when it need not.
// Every plain spelling of the dir (absolute, ~, $HOME, ${HOME}, relative) contains paths.Cache + "/sessions".
func touchesSessionDir(in Intent, raw string) string {
	if in.Class != ClassBash || !strings.Contains(raw, paths.Cache+"/sessions") {
		return ""
	}
	return "it touches alux's session registry or worker channel, which only you may write"
}
