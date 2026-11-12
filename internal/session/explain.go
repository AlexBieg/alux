package session

import (
	"context"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/anthropics/anthropic-sdk-go"
)

const explainMaxChars = 300

const explainSystem = `You explain a shell command to a user deciding whether to allow it to run.
Answer with one plain sentence under 200 characters saying what it does, naming anything it deletes, overwrites, pushes or sends over the network. No preamble.`

// ExplainCommand asks a cheap model for one sentence on what a shell command does.
func ExplainCommand(c anthropic.Client) func(context.Context, string) (string, error) {
	return func(ctx context.Context, command string) (string, error) {
		msg, err := c.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(llm.DefaultAliasModel),
			MaxTokens: 150,
			System:    []anthropic.TextBlockParam{{Text: explainSystem}},
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(command))},
		})
		if err != nil {
			return "", err
		}
		return replyLine(msg, explainMaxChars)
	}
}

// explain fills in the summary of a parked command, unless the approval has moved on by then.
func (s *Session) explain(ctx context.Context, label, command string) {
	line, err := s.Explain(ctx, command)
	if err != nil {
		return
	}
	s.update(func(snap *Snapshot) {
		if snap.Pending == label {
			snap.PendingSummary = line
		}
	})
}
