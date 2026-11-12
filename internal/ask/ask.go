// Package ask holds the questions a turn can park on. It is session state, not view state: the
// question being asked rides the IPC snapshot so a mirrored pane can answer too. Where the cursor
// sits and what has been typed into a note stays local to whichever pane is looking.
//
// A tool call carries a whole series rather than a single question, and the series is walked here
// — one question on the snapshot at a time, the model out of the loop until the last answer. A
// lone question is a series of one, and its result reads exactly as it did before series existed.
package ask

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxOptions      = 8
	maxContextChars = 4000
	// A series longer than this stops being a flow and becomes a form. The cap is enforced here
	// rather than trusted to the model, which is told the same number.
	maxQuestions = 6
	// How much of the recap line the panel gets: the answers already given, newest last.
	maxPriorShown = 2
	maxPriorChars = 24
)

type Option struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

type Question struct {
	Question   string   `json:"question"`
	Context    string   `json:"context,omitempty"`
	Options    []Option `json:"options"`
	AllowOther bool     `json:"allowOther"`

	// Step and Total place this question in its series, and are both 1 for a lone question.
	// Approx is set when a question still to come is gated on an answer, so Total is a ceiling
	// and not a count — the panel says "of up to 4" rather than lying about how much is left.
	// Prior holds the answers already given, so the panel can recap them: the earlier questions
	// have scrolled nowhere, since nothing reaches the transcript until the series finishes.
	Step   int      `json:"step,omitempty"`
	Total  int      `json:"total,omitempty"`
	Approx bool     `json:"approx,omitempty"`
	Prior  []string `json:"prior,omitempty"`
}

// Custom marks the free-text choice, where the note is the whole answer rather than an
// adjustment to a label.
type Answer struct {
	Label  string `json:"label"`
	Note   string `json:"note,omitempty"`
	Custom bool   `json:"custom,omitempty"`
}

// Condition gates a question on an earlier answer in the same series. Matching is on option
// labels, which is what the model wrote and so what it can branch on; a free-text answer matches
// nothing, and the skip reason says so plainly.
type Condition struct {
	Question string   `json:"question"` // the id of an earlier question
	Answered []string `json:"answered"` // any one of these labels opens the gate
}

// Item is one question in a series, with the identity other questions' conditions refer to. A
// parsed tool call is a slice of these, and a lone question is a slice of one.
type Item struct {
	ID       string
	When     *Condition
	Question Question
}

// Exchange is one question's outcome. A skipped question is still reported, carrying why it was
// not asked: the model needs to know the difference between "they said no" and "that never came
// up", and the reason is its cue to ask again in a follow-up call if it still matters.
type Exchange struct {
	Question Question
	Answer   *Answer
	Skipped  string
}

// rawQuestion mirrors the tool's JSON schema for one question. Everything is checked rather than
// trusted: the model's input is arbitrary, and a question with no usable options still works — it
// becomes an open one, which beats parking the turn on an empty list.
type rawQuestion struct {
	Question string `json:"question"`
	Context  string `json:"context"`
	Options  []struct {
		Label  string `json:"label"`
		Detail string `json:"detail"`
	} `json:"options"`
	AllowOther *bool  `json:"allow_other"`
	ID         string `json:"id"`
	When       *struct {
		Question string   `json:"question"`
		Answered []string `json:"answered"`
	} `json:"when"`
}

// rawInput accepts both shapes at once: the embedded fields are the single-question form, and
// questions is the series. A model that sends both gets the series, since that is the more
// specific thing to have asked for.
type rawInput struct {
	rawQuestion
	Questions []rawQuestion `json:"questions"`
}

func Parse(input json.RawMessage) []Item {
	var raw rawInput
	_ = json.Unmarshal(input, &raw)

	items := make([]Item, 0, maxQuestions)
	for _, rq := range raw.Questions {
		if len(items) == maxQuestions {
			break
		}
		// One empty entry in a list is a slip, and the rest of the series is still worth asking.
		// A lone empty question is different: dropping it would park the turn on nothing, so it
		// falls through to the placeholder below.
		if strings.TrimSpace(rq.Question) == "" {
			continue
		}
		items = append(items, parseItem(rq, len(items)+1))
	}
	if len(items) == 0 {
		items = append(items, parseItem(raw.rawQuestion, 1))
	}
	resolve(items)
	return items
}

func parseItem(raw rawQuestion, n int) Item {
	options := make([]Option, 0, len(raw.Options))
	for _, o := range raw.Options {
		label := strings.TrimSpace(o.Label)
		if label == "" {
			continue
		}
		if len(options) == maxOptions {
			break
		}
		options = append(options, Option{Label: label, Detail: strings.TrimSpace(o.Detail)})
	}

	context := raw.Context
	if strings.TrimSpace(context) == "" {
		context = ""
	} else if len(context) > maxContextChars {
		context = context[:maxContextChars]
	}

	question := strings.TrimSpace(raw.Question)
	if question == "" {
		question = "(the model asked an empty question)"
	}

	// An open question has nowhere to type an answer unless the free-text choice is there, so it
	// is forced on regardless of what the model asked for.
	allowOther := true
	if raw.AllowOther != nil && !*raw.AllowOther {
		allowOther = len(options) == 0
	}

	id := strings.TrimSpace(raw.ID)
	if id == "" {
		id = fmt.Sprintf("q%d", n)
	}

	var when *Condition
	if raw.When != nil {
		c := Condition{Question: strings.TrimSpace(raw.When.Question)}
		for _, label := range raw.When.Answered {
			if label = strings.TrimSpace(label); label != "" {
				c.Answered = append(c.Answered, label)
			}
		}
		// A condition naming nothing to match is not a condition.
		if c.Question != "" && len(c.Answered) > 0 {
			when = &c
		}
	}

	return Item{
		ID:   id,
		When: when,
		Question: Question{
			Question:   question,
			Context:    context,
			Options:    options,
			AllowOther: allowOther,
		},
	}
}

// resolve drops a condition that names no earlier question, which leaves that question
// unconditional. Asking it is the safe failure: over-asking costs the user one keystroke, where
// honouring an unresolvable gate would silently swallow a question the model meant to put.
func resolve(items []Item) {
	for i := range items {
		if items[i].When == nil {
			continue
		}
		earlier := false
		for j := 0; j < i; j++ {
			if strings.EqualFold(items[j].ID, items[i].When.Question) {
				earlier = true
				break
			}
		}
		if !earlier {
			items[i].When = nil
		}
	}
}

// given is an answer already collected, kept with its question's text so a skip reason can name
// what was asked rather than an id the user never saw.
type given struct {
	text   string
	answer Answer
}

// Run walks the series, putting each live question to askFn and collecting what comes back. A nil
// answer is the user backing out: the walk stops there and says so, keeping the answers already
// given rather than throwing away a half-finished flow.
func Run(items []Item, askFn func(Question) *Answer) (exchanges []Exchange, cancelled bool) {
	answers := map[string]given{}
	asked := 0
	var prior []string

	for i, item := range items {
		if item.When != nil {
			if reason := gate(*item.When, answers); reason != "" {
				exchanges = append(exchanges, Exchange{Question: item.Question, Skipped: reason})
				continue
			}
		}

		ahead, approx := outlook(items[i+1:], answers)
		q := item.Question
		q.Step = asked + 1
		q.Total = asked + 1 + ahead
		q.Approx = approx
		q.Prior = prior

		answer := askFn(q)
		if answer == nil {
			return exchanges, true
		}
		exchanges = append(exchanges, Exchange{Question: q, Answer: answer})
		answers[strings.ToLower(item.ID)] = given{text: q.Question, answer: *answer}
		asked++
		prior = append(prior, summarise(*answer))
		if len(prior) > maxPriorShown {
			prior = prior[len(prior)-maxPriorShown:]
		}
	}
	return exchanges, false
}

// outlook sizes up what is left to ask. A gate the answers already settle is counted honestly —
// either it will be asked or it will not. A gate waiting on an answer still to come is counted as
// askable and sets approx, which is what turns the panel's "of 4" into "of up to 4": the only
// alternative is to pick one of the two numbers and be wrong half the time.
func outlook(rest []Item, answers map[string]given) (count int, approx bool) {
	for _, item := range rest {
		if item.When == nil {
			count++
			continue
		}
		if _, settled := answers[strings.ToLower(item.When.Question)]; settled {
			if gate(*item.When, answers) == "" {
				count++
			}
			continue
		}
		count++
		approx = true
	}
	return count, approx
}

// gate reports why a question is skipped, or "" when it should be asked.
func gate(c Condition, answers map[string]given) string {
	g, ok := answers[strings.ToLower(c.Question)]
	if !ok {
		return "not asked: it was gated on an answer to a question that was itself not asked"
	}
	if g.answer.Custom {
		return fmt.Sprintf("not asked: it was gated on %q being answered %s, and the user answered in their own words instead",
			g.text, quoteAny(c.Answered))
	}
	for _, label := range c.Answered {
		if strings.EqualFold(label, g.answer.Label) {
			return ""
		}
	}
	return fmt.Sprintf("not asked: it was gated on %q being answered %s, and the user answered %q",
		g.text, quoteAny(c.Answered), g.answer.Label)
}

func quoteAny(labels []string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, fmt.Sprintf("%q", l))
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " or " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
}

// summarise is the one-line form the panel recaps an answer with, where only the gist fits.
func summarise(a Answer) string {
	text := a.Label
	if a.Custom {
		text = a.Note
	}
	text = strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len(text) > maxPriorChars {
		text = strings.TrimSpace(text[:maxPriorChars-1]) + "…"
	}
	return text
}

// Describe is the transcript's label for the whole exchange. A series is named by its size rather
// than by its first question, since no one question stands for it.
func Describe(exchanges []Exchange) string {
	asked := 0
	for _, e := range exchanges {
		if e.Answer != nil {
			asked++
		}
	}
	if asked == 1 && len(exchanges) == 1 {
		return "ask: " + strings.SplitN(exchanges[0].Question.Question, "\n", 2)[0]
	}
	return fmt.Sprintf("ask: %d questions", asked)
}

// Format is what the model gets back. A note on a predefined answer is the interesting case: the
// user picked that option but wants it adjusted, and saying so plainly beats gluing the two
// strings together and hoping the model separates them.
//
// A lone question answers exactly as it did before series existed — the common case should not
// pay for the feature in wrapping the model has to read past.
func Format(exchanges []Exchange, cancelled bool) string {
	if len(exchanges) == 1 && exchanges[0].Answer != nil && !cancelled {
		return formatOne(*exchanges[0].Answer)
	}

	var parts []string
	for i, e := range exchanges {
		head := fmt.Sprintf("%d. %s", i+1, e.Question.Question)
		if e.Answer == nil {
			parts = append(parts, head+"\n   ("+e.Skipped+")")
			continue
		}
		parts = append(parts, head+"\n   "+indent(formatOne(*e.Answer)))
	}
	if cancelled {
		parts = append(parts, fmt.Sprintf(
			"The user backed out at question %d, so that one and anything after it went unanswered. "+
				"Do not assume what they would have said.", len(exchanges)+1))
	}
	return strings.Join(parts, "\n\n")
}

func formatOne(a Answer) string {
	if a.Custom {
		return a.Note
	}
	if a.Note != "" {
		return a.Label + "\n\nThe user qualified that answer: " + a.Note
	}
	return a.Label
}

func indent(text string) string {
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n   ")
}

// Display keeps the whole exchange in the transcript, since a question scrolls away once answered
// and the reply on its own reads as a non-sequitur weeks later. For a series that means every
// question too: the label can only name one thing, and the questions themselves were only ever on
// screen one at a time.
func Display(exchanges []Exchange, cancelled bool) string {
	if len(exchanges) == 1 && exchanges[0].Answer != nil && !cancelled {
		return displayOne(exchanges[0].Question, *exchanges[0].Answer)
	}

	var parts []string
	for i, e := range exchanges {
		block := fmt.Sprintf("%d. %s", i+1, e.Question.Question)
		if e.Answer == nil {
			parts = append(parts, block+"\n  ("+e.Skipped+")")
			continue
		}
		parts = append(parts, block+"\n"+displayOne(e.Question, *e.Answer))
	}
	if cancelled {
		parts = append(parts, "↯ backed out of the rest")
	}
	return strings.Join(parts, "\n\n")
}

func displayOne(q Question, a Answer) string {
	var parts []string
	if q.Context != "" {
		parts = append(parts, q.Context)
	}
	for _, o := range q.Options {
		parts = append(parts, "· "+o.Label)
	}
	switch {
	case a.Custom:
		parts = append(parts, "→ "+a.Note)
	case a.Note != "":
		parts = append(parts, "→ "+a.Label+"\n  "+a.Note)
	default:
		parts = append(parts, "→ "+a.Label)
	}
	return strings.Join(parts, "\n")
}
