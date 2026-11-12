package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	recapMaxChars  = 400
	recapItemChars = 1500
	recapTail      = 6
)

const recapSystem = `You write a one-line recap of a coding session for a user returning to it after a break.
Say what the session is for and where it stopped, in under 300 characters, plain text, no preamble.`

// Recap asks a cheap model for a one-line summary and shows it in the transcript. The line is
// never added to the conversation history, so the agent does not see it.
func (s *Session) Recap(ctx context.Context) error {
	s.mu.Lock()
	excerpt := recapExcerpt(s.snap.Items)
	s.mu.Unlock()
	if excerpt == "" {
		return nil
	}
	c := s.agent.Client
	if c == nil {
		shared := llm.Client()
		c = &shared
	}
	msg, err := c.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(llm.DefaultAliasModel),
		MaxTokens: 200,
		System:    []anthropic.TextBlockParam{{Text: recapSystem}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(excerpt))},
	})
	if err != nil {
		return err
	}
	line, err := replyLine(msg, recapMaxChars)
	if err != nil {
		return err
	}
	s.push(KindInfo, "※ recap: "+line, "", "")
	return nil
}

// replyLine is a reply's text on one line, cut to limit characters.
func replyLine(msg *anthropic.Message, limit int) (string, error) {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	line := strings.Join(strings.Fields(b.String()), " ")
	if line == "" {
		return "", fmt.Errorf("no text in the reply (stop: %s)", msg.StopReason)
	}
	if r := []rune(line); len(r) > limit {
		line = string(r[:limit-1]) + "…"
	}
	return line, nil
}

// recapExcerpt is the first prompt, which usually says what the session is for, and the last few
// exchanges, which say where it stopped.
func recapExcerpt(items []Item) string {
	var said []Item
	for _, it := range items {
		if it.Kind == KindUser || it.Kind == KindAssistant {
			said = append(said, it)
		}
	}
	if len(said) == 0 {
		return ""
	}
	picked := said
	if len(said) > recapTail+1 {
		picked = append([]Item{said[0]}, said[len(said)-recapTail:]...)
	}
	var b strings.Builder
	for _, it := range picked {
		text := it.Text
		if r := []rune(text); len(r) > recapItemChars {
			text = string(r[:recapItemChars]) + "…"
		}
		fmt.Fprintf(&b, "[%s]\n%s\n\n", it.Kind, text)
	}
	return b.String()
}
