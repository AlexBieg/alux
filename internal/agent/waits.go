package agent

import (
	"regexp"
	"strings"

	"github.com/alexbieg/alux/internal/guardian"
	"github.com/anthropics/anthropic-sdk-go"
)

// commandStart is where a word is in command position: the line's start, after a separator, or after a keyword that opens a body.
const commandStart = `(?:^|[;&|(){\n]|\b(?:do|then|else)\s)\s*`

var (
	loopHead = regexp.MustCompile(commandStart + `(?:while|until|for)\s`)
	loopDo   = regexp.MustCompile(`\bdo\b`)
	loopDone = regexp.MustCompile(`\bdone\b`)
	sleepCmd = regexp.MustCompile(commandStart + `sleep\s`)
	// leadingSleep is a command that opens by sleeping, after at most a cd: a wait before whatever follows.
	leadingSleep = regexp.MustCompile(`^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?sleep\s`)
)

// sleepLoop reports a command that sleeps inside a shell loop, ignoring quoted text such as a commit message.
func sleepLoop(command string) bool {
	text := unquoted(command)
	head := loopHead.FindStringIndex(text)
	if head == nil {
		return false
	}
	rest := text[head[1]:]
	do := loopDo.FindStringIndex(rest)
	if do == nil {
		return false
	}
	body := rest[do[1]:]
	sleep := sleepCmd.FindStringIndex(body)
	return sleep != nil && loopDone.MatchString(body[sleep[1]:])
}

// unquoted blanks the contents of single- and double-quoted strings, keeping the quotes.
func unquoted(command string) string {
	var b strings.Builder
	var quote rune
	escaped := false
	for _, c := range command {
		switch {
		case escaped:
			escaped = false
			if quote == 0 {
				b.WriteRune(c)
			}
			continue
		case c == '\\' && quote != '\'':
			escaped = true
			if quote == 0 {
				b.WriteRune(c)
			}
			continue
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
			b.WriteRune(c)
			continue
		case c == quote:
			quote = 0
			b.WriteRune(c)
			continue
		}
		if quote == 0 {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// holdSleep holds a command that waits by sleeping when alux can wait instead; decline_suggestion can still put it to the person.
func (a *Agent) holdSleep(call Call, cb Callbacks, in Intent) *anthropic.ContentBlockParamUnion {
	if call.Name == pollTool || cb.suggestions == nil || !a.has(pollTool) {
		return nil
	}
	command := commandOf(call)
	var reason, advice string
	switch {
	case sleepLoop(command):
		reason = "it waits in a sleep loop, which alux does for you"
		advice = "To wait on something outside the session (CI, a review bot, a container, a URL), call poll with the " +
			"check command and an until or while pattern. To wait on a command's own output, start it with " +
			"run_background and give it until and check_in."
	case a.Backgrounded != nil && a.Backgrounded() && leadingSleep.MatchString(unquoted(command)):
		reason = "it sleeps while a background command you started is still running, and its news will wake you"
		advice = "Carry on with other work, or end your turn if there is none: you are woken when it exits, " +
			"matches its until, or reaches its check-in. To wait on something outside the session, call poll."
	default:
		return nil
	}
	id := cb.suggestions.add(pendingSuggestion{
		command: command, label: in.Label, body: in.Body, reason: reason, call: call, local: true,
		suggestion: guardian.Suggestion{Tools: []string{pollTool, backgroundTool}},
	})
	return block(anthropic.NewToolResultBlock(call.ID, "Not run: "+reason+". "+advice+" If that does not fit, "+
		"call decline_suggestion with id \""+id+"\" and a reason, which puts this exact command to the user to approve.", true))
}

// has reports whether this agent lists the named tool.
func (a *Agent) has(name string) bool {
	if a.Tools == nil {
		return false
	}
	for _, t := range a.Tools() {
		if n := t.GetName(); n != nil && *n == name {
			return true
		}
	}
	return false
}
