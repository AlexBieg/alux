package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/attach"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/session"
)

func TestTheMailboxDeliversInOrderAndNeverBlocksTheSender(t *testing.T) {
	ready := make(chan struct{})
	got := make(chan tea.Msg, 100)
	send := mailbox(ready, func(msg tea.Msg) { got <- msg })
	for i := range 100 {
		send(i)
	}
	close(ready)
	for want := range 100 {
		select {
		case msg := <-got:
			if msg != want {
				t.Fatalf("message %d arrived as %v", want, msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d never arrived", want)
		}
	}
}

func TestAFinishWithNoTUIAttachedIsLeftUnseen(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "serve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := session.New(&agent.Agent{}, nil)
	srv, err := attach.Listen(filepath.Join(dir, "s.sock"), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	reg := registry.New(filepath.Join(dir, "sessions"), "served", dir)
	publish := servedState(s, srv, reg)

	s.SetBusyForTest(true)
	publish()
	if got := reg.Own(); got.State != registry.Working || got.Unseen {
		t.Fatalf("while working: %+v", got)
	}
	s.SetBusyForTest(false)
	publish()
	if got := reg.Own(); got.State != registry.Idle || !got.Unseen {
		t.Fatalf("a finish with nobody attached was not left unseen: %+v", got)
	}
}
