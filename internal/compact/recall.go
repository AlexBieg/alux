package compact

import (
	"fmt"
	"strconv"
	"strings"
)

// RecallTool is the tool a compacted session reads its history back through.
const RecallTool = "recall"

// recallCap is how many characters one recall returns; past it the reply names the step to continue from.
const recallCap = 40_000

// Recall returns steps from..to of ev in full, as the view numbers them, given a range like "120-135" or "7".
func Recall(ev []Event, steps string) (string, error) {
	from, to, err := parseRange(steps)
	if err != nil {
		return "", err
	}
	if from >= len(ev) {
		return "", fmt.Errorf("the compacted history has steps 0-%d", len(ev)-1)
	}
	to = min(to, len(ev)-1)
	var b strings.Builder
	for i := from; i <= to; i++ {
		step := fmt.Sprintf("@%d %s\n\n", i, fullText(ev[i]))
		if b.Len()+len(step) > recallCap {
			if b.Len() == 0 {
				fmt.Fprintf(&b, "%s…[step %d cut at %d characters]\n", step[:recallCap], i, recallCap)
				i++
			}
			if i <= to {
				fmt.Fprintf(&b, "[stopped before step %d to stay under %d characters; recall %d-%d for the rest]\n", i, recallCap, i, to)
			}
			return b.String(), nil
		}
		b.WriteString(step)
	}
	return b.String(), nil
}

func parseRange(s string) (int, int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	a, b, ranged := strings.Cut(s, "-")
	from, err1 := strconv.Atoi(strings.TrimSpace(a))
	to, err2 := from, error(nil)
	if ranged {
		to, err2 = strconv.Atoi(strings.TrimSpace(b))
	}
	if err1 != nil || err2 != nil || from < 0 || to < from {
		return 0, 0, fmt.Errorf("steps must be a step number or a range like 120-135, got %q", s)
	}
	return from, to, nil
}
