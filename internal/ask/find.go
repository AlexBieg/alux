package ask

import (
	"encoding/json"
	"regexp"
	"strings"
)

// A question is the user's to answer when its main clause is a deontic modal or an offer
// ("Should D3 cover…?", "Want me to…?") or puts an auxiliary to "you"; a does/is/did question
// about anything else is an open fact, and one under a lead naming an author or reviewer is theirs.
var (
	decideRx = regexp.MustCompile(`(?i)^(?:(?:and|or|so|but|then),?\s+)?(?:` +
		`(?:should|shall)\b` +
		`|(?:do\s+you\s+)?want\s+(?:me|those|this|that|it|them|the)\b` +
		`|would\s+you\s+(?:like|rather|prefer)\b` +
		`|(?:can|could|may)\s+(?:i|we)\b` +
		`|ok(?:ay)?\b|sound\s+good\b|go\s+ahead\b|agreed\b` +
		`|(?:which|what)\b[^?]*\b(?:do|would)\s+you\s+(?:want|prefer|like|pick|choose)\b` +
		`|(?:which|what)\s+(?:one|option|way|order)\b` +
		`|(?:which|what|how|when)\b[^?]{0,40}\b(?:should|shall)\s+(?:i|we)\b)`)
	askYouRx = regexp.MustCompile(`(?i)^(?:(?:and|or|so|but),?\s+)?(?:` + aux + `\s+(?:you|your)\b` +
		`|(?:which|what|how|when|where|who|why)\b[^?]*?\b` + aux + `\s+(?:you|your)\b)`)
	factRx        = regexp.MustCompile(`(?i)^(?:` + aux + `|should|shall)\b`)
	whRx          = regexp.MustCompile(`(?i)^(?:why|how|what|which|where|when|who)\b`)
	choiceRx      = regexp.MustCompile(`(?i)\bor\b[^?]{0,80}\?$`)
	otherPartyRx  = regexp.MustCompile(`(?i)\b(?:author|reviewers?|the agent|it will|they|for the team|to answer|answered by|questions? (?:for|to) (?:the|them|him|her)\b)`)
	reviewLabelRx = regexp.MustCompile(`(?i)^\[?\s*(?:question|nit|suggestion|issue|todo|praise|thought|chore|discussion)\s*\]?:`)
	markupRx      = regexp.MustCompile("^[\\s>*_`\\-\\d.)\\[\\]]*")
	bulletRx      = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s`)
	labelRx       = regexp.MustCompile(`^(.{1,70}?):\**\s+(.*)$`)
	clauseRx      = regexp.MustCompile(`[,;—]\s+|\s+so\s+`)
	offerLeadRx   = regexp.MustCompile(`(?i)^(?:(?:and|or|so|then),?\s+)?(?:do\s+you\s+want\s+(?:me\s+to\s+)?|want\s+me\s+to\s+|would\s+you\s+(?:like|rather|prefer)\s+(?:me\s+to\s+)?|(?:should|shall|can)\s+(?:i|we)\s+)`)
	youRx         = regexp.MustCompile(`(?i)\byou\b`)
)

const aux = `(?:do|does|did|is|are|was|were|has|have|will|can|could|would|may|might)`

type kind int

const (
	notAsk kind = iota
	offer
	asked
)

// Find returns an ask_user input holding the questions reply puts to the user, or nil when it puts none.
func Find(reply string) json.RawMessage {
	type option struct {
		Label string `json:"label"`
	}
	type question struct {
		Question string   `json:"question"`
		Options  []option `json:"options,omitempty"`
	}
	var qs []question
	for _, s := range askSentences(reply) {
		if len(qs) == maxQuestions {
			break
		}
		q := question{Question: s.text}
		for _, o := range options(s) {
			q.Options = append(q.Options, option{Label: o})
		}
		qs = append(qs, q)
	}
	if len(qs) == 0 {
		return nil
	}
	input, _ := json.Marshal(map[string]any{"questions": qs})
	return input
}

type sentence struct {
	text   string // as written, minus leading markup
	clause string // the clause the grammar matched
	kind   kind
}

func askSentences(reply string) []sentence {
	var found []sentence
	lead := ""
	for _, line := range prose(reply) {
		if strings.TrimSpace(line) == "" {
			lead = ""
			continue
		}
		governing := lead
		if !bulletRx.MatchString(line) {
			lead, governing = line, ""
		}
		for _, s := range sentences(line) {
			if !strings.HasSuffix(strings.TrimRight(s, "*_`)\"' "), "?") {
				continue
			}
			if got := parse(s, governing); got.kind != notAsk {
				found = append(found, got)
			}
		}
	}
	return found
}

// prose is reply's lines with fenced code blanked, so a "?" in code never reads as a question.
func prose(reply string) []string {
	lines := strings.Split(reply, "\n")
	fence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			fence = !fence
			lines[i] = ""
		} else if fence {
			lines[i] = ""
		}
	}
	return lines
}

// sentences splits after ? . or ! when a capital, bracket or markup opens the next one.
func sentences(line string) []string {
	var out []string
	start := 0
	for i := 0; i < len(line); i++ {
		if c := line[i]; c != '?' && c != '.' && c != '!' {
			continue
		}
		j := i + 1
		for j < len(line) && strings.IndexByte("*_`\"')", line[j]) >= 0 {
			j++
		}
		if j >= len(line) || line[j] != ' ' {
			continue
		}
		k := j
		for k < len(line) && line[k] == ' ' {
			k++
		}
		if k < len(line) && (line[k] >= 'A' && line[k] <= 'Z' || strings.IndexByte("(\"'*`[", line[k]) >= 0) {
			out = append(out, line[start:j])
			start, i = k, k-1
		}
	}
	return append(out, line[start:])
}

func parse(raw, lead string) sentence {
	s := strings.TrimSpace(markupRx.ReplaceAllString(raw, ""))
	text := strings.Trim(s, "*_ ")
	if reviewLabelRx.MatchString(s) || strings.Count(s, `"`)%2 == 1 || strings.HasPrefix(s, `"`) || strings.HasPrefix(s, "“") {
		return sentence{}
	}
	theirs := otherPartyRx.MatchString(lead)
	body := s
	if m := labelRx.FindStringSubmatch(s); m != nil {
		head := strings.Trim(m[1], "*_` ")
		if decideRx.MatchString(head) {
			return mine(theirs, sentence{text: text, clause: s, kind: kindOf(head)})
		}
		if !strings.Contains(m[1], "?") {
			if otherPartyRx.MatchString(m[1]) {
				theirs = true
			} else if youRx.MatchString(m[1]) {
				theirs = false
			}
			body = m[2]
		}
	}
	body = strings.Trim(body, "*_` ")
	clauses := []string{body}
	for _, c := range clauseRx.Split(body, -1)[1:] {
		clauses = append(clauses, strings.TrimSpace(c))
	}
	for _, c := range clauses {
		if decideRx.MatchString(c) {
			return mine(theirs, sentence{text: text, clause: body, kind: kindOf(c)})
		}
	}
	for _, c := range clauses {
		if askYouRx.MatchString(c) {
			return mine(theirs, sentence{text: text, clause: body, kind: asked})
		}
	}
	if factRx.MatchString(body) || whRx.MatchString(body) {
		return sentence{}
	}
	if choiceRx.MatchString(body) || len(strings.Fields(body)) <= 6 {
		return mine(theirs, sentence{text: text, clause: body, kind: asked})
	}
	return sentence{}
}

func mine(theirs bool, s sentence) sentence {
	if theirs {
		return sentence{}
	}
	return s
}

func kindOf(clause string) kind {
	if offerLeadRx.MatchString(clause) {
		return offer
	}
	return asked
}

// options are the choices written into the question, or Yes and No for a plain offer; none leaves it open.
func options(s sentence) []string {
	body := strings.TrimRight(strings.Trim(s.clause, "*_` "), "?")
	if m := labelRx.FindStringSubmatch(body); m != nil && strings.Contains(m[2], " or ") {
		if parts := split(listRx, m[2]); parts != nil {
			return parts
		}
	}
	if strings.Contains(body, " or ") && !strings.Contains(strings.ToLower(body), " or not") {
		if parts := split(orRx, offerLeadRx.ReplaceAllString(body, "")); len(parts) == 2 {
			return parts
		}
		return nil
	}
	if s.kind == offer {
		return []string{"Yes", "No"}
	}
	return nil
}

var (
	// A list after a label separates by commas; an inline choice only by "or", since its commas set off clauses.
	listRx = regexp.MustCompile(`,\s*(?:or\s+)?|\s+or\s+`)
	orRx   = regexp.MustCompile(`,?\s+or\s+`)
	// A part still holding a modal or a comma is a clause, not a choice, and makes a label that misleads.
	clausyRx  = regexp.MustCompile(`(?i)\b(?:should|shall)\b|,`)
	altLeadRx = regexp.MustCompile(`(?i)^(?:(?:would|will)\s+you\s+(?:rather\s+)?|to\s+)`)
)

// split returns the choices in list, or nil when any of them would make a poor label.
func split(sep *regexp.Regexp, list string) []string {
	var parts []string
	for _, p := range sep.Split(list, -1) {
		p = altLeadRx.ReplaceAllString(strings.Trim(p, " *"), "")
		if p == "" {
			continue
		}
		if n := len(strings.Fields(p)); n > 14 || n < 2 && sep == orRx || clausyRx.MatchString(p) {
			return nil
		}
		parts = append(parts, strings.ToUpper(p[:1])+p[1:])
	}
	if len(parts) < 2 || len(parts) > 4 {
		return nil
	}
	return parts
}
