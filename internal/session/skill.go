package session

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/skillrun"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/anthropics/anthropic-sdk-go"
)

// SkillView is the open skill run as the pane draws it.
type SkillView = skillrun.View

func (s *Session) newSkillHost() *skillrun.Host {
	return &skillrun.Host{
		Changed: func(v *SkillView) { s.update(func(snap *Snapshot) { snap.Skill = v }) },
		Closed: func(skill *skills.Skill, res skills.Result, how string) {
			s.flush()
			s.Show(skills.Diagram(res) + "\n\n" + skills.Summary(skill.Name, how, res))
			if s.OnSkillClosed != nil {
				s.OnSkillClosed(how)
			}
		},
		PollOf: func(call string) int {
			if s.Procs == nil {
				return 0
			}
			return s.Procs.PollOf(call)
		},
	}
}

var pollEnds = map[procs.Stop]skills.PollEnd{
	procs.StopMet:      skills.PollMet,
	procs.StopTimedOut: skills.PollTimedOut,
	procs.StopFailed:   skills.PollFailed,
	procs.StopKilled:   skills.PollKilled,
}

// PollEnded logs a poll's end in the open skill run, for poll_stopped.
func (s *Session) PollEnded(id int, how procs.Stop) {
	if end, ok := pollEnds[how]; ok {
		s.skill.PollEnded(id, end)
	}
}

// WireSkills offers the agent start_skill, finish_skill and fail_run for the whole session.
func (s *Session) WireSkills() {
	if s.agent != nil {
		s.skill.Wire(s.agent)
	}
}

// SkillStage names the open flow's current stage, else the open skill, else "".
func (s *Session) SkillStage() string { return s.skill.Stage() }

// WireDispatches logs a dispatch in the open skill run when its subagent run ends rather than when the call returns.
func (s *Session) WireDispatches() {
	if s.Agents == nil {
		return
	}
	s.skill.Lands = s.Agents.Dispatches
	s.Agents.Landed = s.skill.Land
	s.skill.CheckDispatch = s.Agents.CheckSkillAgent
	s.skill.Dispatch = func(_ context.Context, call agent.Call, name, skill string, inputs map[string]any, change, task string, cb agent.Callbacks) (int, error) {
		return s.Agents.DispatchSkill(call, name, skill, inputs, change, task, cb)
	}
	s.skill.Stop = s.Agents.Roster.Stop
}

// runSkill is /skills run: it opens the run and sends the agent every step at once.
func (s *Session) runSkill(name, text string) {
	brief, err := s.skill.StartText(name, text)
	if err != nil {
		s.push(KindError, err.Error(), "", "")
		return
	}
	s.Submit(brief)
}

// RunSkill opens a run for the user with its inputs bound, as a key in the UI does, and sends the agent its steps.
func (s *Session) RunSkill(name string, inputs map[string]any) {
	brief, err := s.skill.StartInputs(name, inputs)
	if err != nil {
		s.push(KindError, err.Error(), "", "")
		return
	}
	s.Submit(brief)
}

func (s *Session) endSkill() {
	if !s.skill.End("ended with /skills end") {
		s.Show("no skill is running")
	}
}

// skillBox gives the skill tools a transcript box, since a plain handler tool gets none of its own.
func (s *Session) skillBox(call agent.Call, res anthropic.ContentBlockParamUnion) {
	var in struct {
		Skill  string `json:"skill"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(call.Input, &in)
	label := call.Name
	switch call.Name {
	case skillrun.StartTool:
		label += " " + strings.TrimSpace(in.Skill)
	case skillrun.FailTool:
		label += " " + strings.TrimSpace(in.Reason)
	case skillrun.FinishTool, skillrun.FinishStageTool:
	default:
		return
	}
	var body []string
	if r := res.OfToolResult; r != nil {
		for _, c := range r.Content {
			if c.OfText != nil {
				body = append(body, c.OfText.Text)
			}
		}
	}
	s.flush()
	s.push(KindTool, label, strings.Join(body, "\n"), agent.KindSkill)
}
