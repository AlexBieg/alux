package ask

import (
	"encoding/json"
	"strings"
	"testing"
)

// answerer replies to a walk with a canned answer per question, and records what it was shown.
type answerer struct {
	replies []*Answer
	seen    []Question
}

func (a *answerer) fn(q Question) *Answer {
	a.seen = append(a.seen, q)
	if len(a.seen) > len(a.replies) {
		return nil
	}
	return a.replies[len(a.seen)-1]
}

func pick(label string) *Answer { return &Answer{Label: label} }

// The single-question form is the common case, and its result is what the model has always been
// given. Wrapping it in series bookkeeping would be a silent change to every ordinary ask.
func TestLoneQuestionReadsAsItAlwaysDid(t *testing.T) {
	seq := Parse(json.RawMessage(`{
		"question": "Which way?",
		"options": [{"label": "Left"}, {"label": "Right"}]
	}`))
	if len(seq) != 1 {
		t.Fatalf("parsed %d items, want 1", len(seq))
	}

	a := &answerer{replies: []*Answer{{Label: "Left", Note: "but only at the junction"}}}
	exchanges, cancelled := Run(seq, a.fn)
	if cancelled {
		t.Fatal("walk reported a back-out")
	}

	if got, want := Format(exchanges, false), "Left\n\nThe user qualified that answer: but only at the junction"; got != want {
		t.Errorf("Format =\n%q\nwant\n%q", got, want)
	}
	if got, want := Display(exchanges, false), "· Left\n· Right\n→ Left\n  but only at the junction"; got != want {
		t.Errorf("Display =\n%q\nwant\n%q", got, want)
	}
	if got, want := Describe(exchanges), "ask: Which way?"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
	// Nothing in a lone question's presentation should hint at a series.
	if q := a.seen[0]; q.Total != 1 || q.Step != 1 || len(q.Prior) != 0 {
		t.Errorf("lone question presented as step %d of %d with prior %v", q.Step, q.Total, q.Prior)
	}
}

func TestSeriesAsksInOrderAndRecapsAsItGoes(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"question": "Where do answers live?", "options": [{"label": "Session state"}]},
		{"question": "Which format?", "options": [{"label": "JSON"}]},
		{"question": "Which key?", "options": [{"label": "answers"}]}
	]}`))

	a := &answerer{replies: []*Answer{pick("Session state"), pick("JSON"), pick("answers")}}
	exchanges, cancelled := Run(seq, a.fn)
	if cancelled || len(exchanges) != 3 {
		t.Fatalf("walk gave %d exchanges, cancelled=%v", len(exchanges), cancelled)
	}

	for i, want := range []struct {
		step, total int
		prior       []string
	}{
		{1, 3, nil},
		{2, 3, []string{"Session state"}},
		{3, 3, []string{"Session state", "JSON"}},
	} {
		q := a.seen[i]
		if q.Step != want.step || q.Total != want.total || q.Approx {
			t.Errorf("question %d presented as step %d of %d (approx %v), want %d of %d exact",
				i+1, q.Step, q.Total, q.Approx, want.step, want.total)
		}
		if strings.Join(q.Prior, "|") != strings.Join(want.prior, "|") {
			t.Errorf("question %d recapped %v, want %v", i+1, q.Prior, want.prior)
		}
	}

	result := Format(exchanges, false)
	for _, want := range []string{"1. Where do answers live?\n   Session state", "2. Which format?\n   JSON", "3. Which key?\n   answers"} {
		if !strings.Contains(result, want) {
			t.Errorf("result missing %q:\n%s", want, result)
		}
	}
	if got, want := Describe(exchanges), "ask: 3 questions"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}

// The branch the user does not take is never put to them, and the model is told that it was
// skipped rather than left to read the gap as an answer.
func TestGatedQuestionIsSkippedWithItsReason(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"id": "store", "question": "Where do answers live?",
		 "options": [{"label": "Session state"}, {"label": "The snapshot"}]},
		{"id": "key", "question": "Which snapshot key?",
		 "when": {"question": "store", "answered": ["The snapshot"]},
		 "options": [{"label": "answers"}]},
		{"question": "Anything else?", "options": [{"label": "No"}]}
	]}`))

	a := &answerer{replies: []*Answer{pick("Session state"), pick("No")}}
	exchanges, _ := Run(seq, a.fn)

	if len(a.seen) != 2 {
		t.Fatalf("asked %d questions, want 2 — the gated one should not have been put", len(a.seen))
	}
	if a.seen[0].Total != 3 || !a.seen[0].Approx {
		t.Errorf("first question presented as %d of %d (approx %v), want an inexact 3",
			a.seen[0].Step, a.seen[0].Total, a.seen[0].Approx)
	}
	// Once the gate has failed the count is known exactly, so the ceiling resolves to 2.
	if a.seen[1].Total != 2 || a.seen[1].Approx {
		t.Errorf("second question presented as %d of %d (approx %v), want an exact 2",
			a.seen[1].Step, a.seen[1].Total, a.seen[1].Approx)
	}

	if len(exchanges) != 3 || exchanges[1].Answer != nil {
		t.Fatalf("exchanges = %+v, want the gated question reported unanswered", exchanges)
	}
	reason := exchanges[1].Skipped
	for _, want := range []string{"not asked", `"Where do answers live?"`, `"The snapshot"`, `"Session state"`} {
		if !strings.Contains(reason, want) {
			t.Errorf("skip reason %q missing %q", reason, want)
		}
	}
	if result := Format(exchanges, false); !strings.Contains(result, "2. Which snapshot key?\n   (not asked") {
		t.Errorf("result does not report the skipped question:\n%s", result)
	}
}

// A gate still ahead of us is only a maybe, so the count is a ceiling; once the answer it hangs
// on is in, the count is knowable and the panel should stop hedging.
func TestCountStopsHedgingOnceTheGateIsSettled(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"id": "store", "question": "Where do answers live?",
		 "options": [{"label": "Session state"}, {"label": "The snapshot"}]},
		{"question": "Unrelated?", "options": [{"label": "Sure"}]},
		{"question": "Which snapshot key?", "when": {"question": "store", "answered": ["The snapshot"]}},
		{"question": "Last?", "options": [{"label": "Done"}]}
	]}`))

	a := &answerer{replies: []*Answer{pick("Session state"), pick("Sure"), pick("Done")}}
	Run(seq, a.fn)

	if len(a.seen) != 3 {
		t.Fatalf("asked %d questions, want 3", len(a.seen))
	}
	// Nothing is answered yet, so the gated question might still be asked: 4 is a ceiling.
	if a.seen[0].Total != 4 || !a.seen[0].Approx {
		t.Errorf("first question presented as %d of %d (approx %v), want an inexact 4",
			a.seen[0].Step, a.seen[0].Total, a.seen[0].Approx)
	}
	// The gating answer is in and it failed, so the gated question is off the board: exactly 3.
	if a.seen[1].Total != 3 || a.seen[1].Approx {
		t.Errorf("second question presented as %d of %d (approx %v), want an exact 3",
			a.seen[1].Step, a.seen[1].Total, a.seen[1].Approx)
	}
}

// A gate matches labels, so free text cannot open one. Saying which way it failed is what lets
// the model decide whether to ask the branch in a follow-up call.
func TestFreeTextAnswerFailsALabelGateAndSaysSo(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"id": "store", "question": "Where do answers live?", "options": [{"label": "The snapshot"}]},
		{"question": "Which key?", "when": {"question": "store", "answered": ["The snapshot"]}}
	]}`))

	a := &answerer{replies: []*Answer{{Label: "Other", Note: "in a side file, actually", Custom: true}}}
	exchanges, _ := Run(seq, a.fn)

	if len(a.seen) != 1 {
		t.Fatalf("asked %d questions, want 1", len(a.seen))
	}
	if !strings.Contains(exchanges[1].Skipped, "in their own words") {
		t.Errorf("skip reason does not say the answer was free text: %q", exchanges[1].Skipped)
	}
}

// Over-asking costs a keystroke; honouring a gate that can never be evaluated would lose the
// question entirely, and silently.
func TestUnresolvableGateAsksTheQuestionAnyway(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"id": "first", "question": "One?", "when": {"question": "later", "answered": ["yes"]}},
		{"id": "later", "question": "Two?"}
	]}`))

	a := &answerer{replies: []*Answer{pick("a"), pick("b")}}
	if _, _ = Run(seq, a.fn); len(a.seen) != 2 {
		t.Fatalf("asked %d questions, want both — a forward reference cannot gate anything", len(a.seen))
	}
}

// Backing out half way is not a declined call: the answers already given are real, and asking
// for them again is the one thing the user has already told us they are done with.
func TestBackingOutKeepsTheAnswersAlreadyGiven(t *testing.T) {
	seq := Parse(json.RawMessage(`{"questions": [
		{"question": "One?", "options": [{"label": "Yes"}]},
		{"question": "Two?", "options": [{"label": "No"}]},
		{"question": "Three?", "options": [{"label": "Maybe"}]}
	]}`))

	a := &answerer{replies: []*Answer{pick("Yes")}} // nil for everything after the first
	exchanges, cancelled := Run(seq, a.fn)

	if !cancelled {
		t.Fatal("walk did not report the back-out")
	}
	if len(exchanges) != 1 || exchanges[0].Answer.Label != "Yes" {
		t.Fatalf("exchanges = %+v, want the first answer kept", exchanges)
	}
	result := Format(exchanges, true)
	if !strings.Contains(result, "Yes") || !strings.Contains(result, "backed out at question 2") {
		t.Errorf("result does not carry both the answer and the back-out:\n%s", result)
	}
}

func TestParseGuardsAgainstWhatAModelMightSend(t *testing.T) {
	// Empty entries in a series are dropped, but a lone empty question still parks the turn on
	// something rather than on nothing.
	if seq := Parse(json.RawMessage(`{"questions": [{"question": "  "}, {"question": "Real?"}]}`)); len(seq) != 1 ||
		seq[0].Question.Question != "Real?" {
		t.Errorf("empty entry not dropped: %+v", seq)
	}
	if seq := Parse(json.RawMessage(`{}`)); len(seq) != 1 || !strings.Contains(seq[0].Question.Question, "empty question") {
		t.Errorf("empty input did not become a placeholder question: %+v", seq)
	}

	// An open question has nowhere to type unless the free-text choice is there.
	seq := Parse(json.RawMessage(`{"questions": [{"question": "Open?", "allow_other": false}]}`))
	if !seq[0].Question.AllowOther {
		t.Error("an optionless question refused free text, leaving nowhere to answer")
	}

	// A series longer than the cap is truncated rather than asked in full.
	var long strings.Builder
	long.WriteString(`{"questions": [`)
	for i := 0; i < maxQuestions+4; i++ {
		if i > 0 {
			long.WriteString(",")
		}
		long.WriteString(`{"question": "Q?"}`)
	}
	long.WriteString(`]}`)
	if seq := Parse(json.RawMessage(long.String())); len(seq) != maxQuestions {
		t.Errorf("parsed %d questions, want the cap of %d", len(seq), maxQuestions)
	}

	// Ids are the model's own strings; matching them case-sensitively would be a trap.
	cased := Parse(json.RawMessage(`{"questions": [
		{"id": "Store", "question": "One?", "options": [{"label": "Yes"}]},
		{"question": "Two?", "when": {"question": "store", "answered": ["yes"]}}
	]}`))
	a := &answerer{replies: []*Answer{pick("Yes"), pick("x")}}
	if _, _ = Run(cased, a.fn); len(a.seen) != 2 {
		t.Errorf("asked %d questions, want 2 — id and label matching are both case-insensitive", len(a.seen))
	}
}
