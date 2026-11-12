package session

import "testing"

func TestATopicReplyThatAnswersThePromptsIsRefused(t *testing.T) {
	for _, reply := range []string{
		"I don't have access to external resour…",
		"I can't open links but here is a name",
	} {
		if name, err := topicName(reply); err == nil {
			t.Errorf("topicName(%q) = %q, want an error", reply, name)
		}
	}
	if name, err := topicName(`"Status Border Tweaks."`); err != nil || name != "status border tweaks" {
		t.Errorf("topicName of a real name = %q, %v; want status border tweaks", name, err)
	}
}
