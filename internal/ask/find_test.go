package ask

import (
	"encoding/json"
	"reflect"
	"testing"
)

func found(t *testing.T, reply string) map[string][]string {
	t.Helper()
	input := Find(reply)
	if input == nil {
		return nil
	}
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, q := range in.Questions {
		labels := []string{}
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		out[q.Question] = labels
	}
	return out
}

func TestFindKeepsOnlyQuestionsForTheUser(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  map[string][]string
	}{
		{"offer before a closing note", "Done.\n\nWant me to build it?\n\nStored rec_1.",
			map[string][]string{"Want me to build it?": {"Yes", "No"}}},
		{"inline choice", "Should I drop it, or keep it?",
			map[string][]string{"Should I drop it, or keep it?": {"Drop it", "Keep it"}}},
		{"listed choice after a label", "Which do you want: A, B, or the mix?",
			map[string][]string{"Which do you want: A, B, or the mix?": {"A", "B", "The mix"}}},
		{"choice whose parts are clauses is left open", "When he is away, should the fallback be the status line, or the card?",
			map[string][]string{"When he is away, should the fallback be the status line, or the card?": {}}},
		{"modal with a third-party subject", "Should violation pointers be kept?",
			map[string][]string{"Should violation pointers be kept?": {}}},
		{"main clause after a subordinate one", "Once it's up, should I run Bugbot?",
			map[string][]string{"Once it's up, should I run Bugbot?": {"Yes", "No"}}},
		{"open fact question", "Does the server send exposures?", nil},
		{"rhetorical wh question", "Why does it fail? Because the cache is cold.", nil},
		{"questions for the author", "**Questions for the author:**\n- Should a failed Replace keep the summary?", nil},
		{"review comment", "`[question]:` Should this say why the list is empty?", nil},
		{"code", "```go\n// should we?\n```", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := found(t, tc.reply); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Find = %v, want %v", got, tc.want)
			}
		})
	}
}
