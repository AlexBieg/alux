package ui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
)

// findingChat is the live agent behind one finding's thread; the thread itself is stored on the finding.
type findingChat struct {
	chat session.Chat
	// asking is the question waiting on a reply, typed what sits in the input while the chat is not focused.
	asking, typed string
	// unread is set when a reply lands and cleared once the finding is selected.
	unread bool
}

type findingChatMsg struct {
	repo           string
	number, at     int
	head, question string
	chat           session.Chat
	reply          string
	err            error
}

const fileGraphChatHint = "enter asks · esc stops typing"

// deco is what path's finding boxes show beyond the review: questions in flight, and the selected
// box's keys and, while chatting, its input. The same in both views.
func (p *fileGraphPane) deco(path string) findingDeco {
	d := findingDeco{asking: p.asking(path), hint: "c asks about it · x dismisses"}
	if p.chatting {
		d.hint, d.input = fileGraphChatHint, p.input
	}
	return d
}

// newFindingInput is the box's question input: the main input's keys, drawing its own cursor since
// the terminal's stays with the main input.
func newFindingInput() *textarea.Model {
	in := textarea.New()
	in.Prompt = ""
	in.ShowLineNumbers = false
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"))
	in.DynamicHeight = true
	in.MaxContentHeight = maxInputRows
	in.SetPromptFunc(lipgloss.Width(inputMarker), func(info textarea.PromptInfo) string {
		if info.LineNumber > 0 {
			return ""
		}
		return promptStyle.Render(inputMarker)
	})
	st := in.Styles()
	st.Focused.CursorLine = st.Focused.CursorLine.UnsetBackground()
	in.SetStyles(st)
	in.Focus()
	return &in
}

// selectedFinding indexes the review's finding under the selection in either view.
func (m *Model) selectedFinding() (int, bool) {
	p := m.fileGraph
	idx := p.findingIndexes(p.hovered().Path)
	switch {
	case p.diff:
		items := m.diffItems()
		if p.line < len(items) && items[p.line].finding >= 0 {
			return idx[items[p.line].finding], true
		}
	case p.inFindings && p.sel < len(idx):
		return idx[p.sel], true
	}
	return 0, false
}

// asking is each of findings' question in flight, parallel to findingsOn(path).
func (p *fileGraphPane) asking(path string) []string {
	idx := p.findingIndexes(path)
	out := make([]string, len(idx))
	for k, i := range idx {
		if fc := p.chats[i]; fc != nil {
			out[k] = fc.asking
		}
	}
	return out
}

// chatMark is the file list's sign that a finding on path has a question in flight, or an answer not yet seen.
func (m *Model) chatMark(path string) (string, lipgloss.Style) {
	p := m.fileGraph
	unread := false
	for _, i := range p.findingIndexes(path) {
		fc := p.chats[i]
		switch {
		case fc == nil:
		case fc.asking != "":
			frames := spinner.MiniDot.Frames
			return " " + frames[m.poseTick%len(frames)], lipgloss.NewStyle().Foreground(workingColor)
		case fc.unread:
			unread = true
		}
	}
	if unread {
		return " ●", lipgloss.NewStyle().Foreground(unseenColor)
	}
	return "", plainStyle
}

func (p *fileGraphPane) anyAsking() bool {
	for _, fc := range p.chats {
		if fc.asking != "" {
			return true
		}
	}
	return false
}

// focusFindingChat opens an input in the selected finding's box, leaving the main input as it was.
func (m *Model) focusFindingChat() {
	p := m.fileGraph
	i, ok := m.selectedFinding()
	if !ok {
		return
	}
	if p.chats == nil {
		p.chats = map[int]*findingChat{}
	}
	fc := p.chats[i]
	if fc == nil {
		fc = &findingChat{}
		p.chats[i] = fc
	}
	if p.input == nil {
		p.input = newFindingInput()
	}
	p.input.SetValue(fc.typed)
	p.chatting, p.chatAt, p.follow = true, i, true
	m.layout()
}

func (m *Model) unfocusFindingChat() {
	p := m.fileGraph
	if p == nil || !p.chatting {
		return
	}
	p.chats[p.chatAt].typed = p.input.Value()
	p.chatting = false
	m.layout()
}

func (m *Model) onFindingChatKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.unfocusFindingChat()
		return nil
	case "enter":
		return m.askFinding()
	}
	p := m.fileGraph
	in, cmd := p.input.Update(msg)
	*p.input = in
	p.follow = true
	m.layout()
	return cmd
}

// askFinding sends what is typed to the focused finding's chat, briefing a new chat with the stored thread.
func (m *Model) askFinding() tea.Cmd {
	p := m.fileGraph
	fc := p.chats[p.chatAt]
	text := strings.TrimSpace(p.input.Value())
	if fc.asking != "" || text == "" || p.currentReview() == nil {
		return nil
	}
	p.input.SetValue("")
	fc.asking = text
	p.follow = true
	m.layout()

	f := p.review.Findings[p.chatAt]
	brief := subagent.FindingBrief{
		PR: fmt.Sprintf("%s#%d", p.g.Repo, p.g.Number), Head: p.g.Head, Path: f.Path,
		Severity: f.Severity, Class: f.Class, StartLine: f.StartLine, EndLine: f.EndLine, Personas: f.Personas,
		Patch: m.patchOf(f.Path), Source: p.g.Sources[f.Path],
	}
	var body []string
	for _, s := range f.Sections() {
		body = append(body, sectionText(s))
	}
	brief.Text = strings.Join(body, "\n")
	for _, t := range f.Thread {
		brief.Thread = append(brief.Thread, subagent.FindingMessage{Role: t.Role, Text: t.Text})
	}
	msg := findingChatMsg{repo: p.g.Repo, number: p.g.Number, at: p.chatAt, head: p.g.Head, question: text, chat: fc.chat}
	newChat, dir := m.newFindingChat, p.g.Workspace
	return func() tea.Msg {
		if msg.chat == nil {
			if msg.chat, msg.err = newChat(dir, brief); msg.err != nil {
				return msg
			}
		}
		msg.reply, msg.err = msg.chat.Send(context.Background(), text)
		return msg
	}
}

// onFindingChatReply stores the question and reply on the finding, rereading the review so a dismiss in between is kept.
func (m *Model) onFindingChatReply(msg findingChatMsg) tea.Cmd {
	p := m.fileGraph
	same := p != nil && strings.EqualFold(p.g.Repo, msg.repo) && p.g.Number == msg.number && p.g.Head == msg.head
	var fc *findingChat
	if same {
		if fc = p.chats[msg.at]; fc != nil {
			fc.asking, fc.chat = "", msg.chat
		}
	}
	if msg.err != nil {
		if fc != nil && fc.typed == "" {
			fc.typed = msg.question
			if p.chatting && p.chatAt == msg.at && p.input.Value() == "" {
				p.input.SetValue(msg.question)
			}
		}
		m.layout()
		return m.Notify("finding chat: " + msg.err.Error())
	}
	r, ok, err := loadReview(msg.repo, msg.number)
	if err != nil || !ok || r.Head != msg.head || msg.at >= len(r.Findings) {
		m.layout()
		return m.Notify("finding chat: the review changed, so the answer was not stored")
	}
	r.Findings[msg.at].Thread = append(r.Findings[msg.at].Thread,
		reviewstore.Message{Role: "user", Text: msg.question},
		reviewstore.Message{Role: "assistant", Text: strings.TrimSpace(msg.reply)})
	if err := saveReview(r); err != nil {
		m.layout()
		return m.Notify("finding chat: " + err.Error())
	}
	if same {
		p.review, p.follow = &r, true
		if fc != nil {
			fc.unread = true
		}
	}
	m.layout()
	return nil
}
