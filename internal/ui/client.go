package ui

import (
	"context"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
)

// SessionClient is everything the TUI asks of the session it is attached to.
// Prompts are answered by seq so an answer can never land on a prompt the user did not see.
type SessionClient interface {
	Snapshot() session.Snapshot
	LiveProcs() []procs.Info
	FileDiff(path string) (string, error)

	Submit(text string)
	Show(text string)
	Steer(id int)
	Unqueue(id int) string
	DropQueued() int
	Cancel()
	SetMode(mode session.Mode)
	RunSkill(name string, inputs map[string]any)

	ApproveID(seq int, okay bool) bool
	AnswerID(seq int, a *ask.Answer) bool

	Recap(ctx context.Context) error
	FindingChat(dir string, brief subagent.FindingBrief) (session.Chat, error)
}

var _ SessionClient = (*session.Session)(nil)
