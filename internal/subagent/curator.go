package subagent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
)

// No web tools: an unfiled entry or a record it reads may be phi.
const (
	curatorName  = "curator"
	curatorSkill = "record-cleanup"
)

// Curator is nil without a record store, so it is never offered half-equipped.
func Curator() *Definition {
	if len(agent.RecordTools()) == 0 || len(agent.RecordReadTools()) == 0 {
		return nil
	}
	return &Definition{
		Name: curatorName,
		Description: "runs the " + curatorSkill + " skill in the background: works through the record store's slot conflicts, " +
			"open unfiled entries and unjudged look-alike record pairs, gists ended sessions whose transcripts will soon be pruned, fixes only what it is sure of, and reports what it changed and what it left for the user. " +
			"alux starts one on a schedule when one is due and the store has a backlog, so dispatch it only when the user asks for a cleanup now, rather than " +
			"working the conflicts yourself. It reads the vault and the repo but cannot edit files.",
		Upkeep:     true,
		System:     curatorPrompt,
		Tools:      curatorTools,
		Handlers:   curatorHandlers(),
		Model:      migratorModelName,
		Policy:     readOnlyShell(curatorName),
		Background: true,
		Commands:   &guardian.Criteria{Agent: curatorName, Allowed: []guardian.Category{guardian.Read}, Threshold: guardian.ReadOnlyThreshold},
		Trail:      func(_ agent.ToolKind, label, _ string) string { return label },
		Skill:      curatorSkill,
		Backlog:    curatorBacklog,
	}
}

func curatorBacklog(session string) (int, error) {
	store, err := agent.OpenRecordStore()
	if err != nil {
		return 0, err
	}
	conflicts, unfiled, err := store.Backlog()
	if err != nil {
		return 0, err
	}
	pairs, err := store.PairBacklog()
	if err != nil {
		return 0, err
	}
	gistable, err := agent.GistableSessions(store, agent.SessionDir(), session, registry.RunningSessions())
	return conflicts + unfiled + pairs + len(gistable), err
}

func curatorPrompt() string {
	return strings.Join([]string{
		"You are a curator subagent. You keep the user's record store tidy by running one skill, whose steps the task gives you. You never talk to the user.",
		"Working directory: " + project.Cwd(),
		"Vault: " + vault.Root(),
		"",
		"Act only when you are sure. Sure means the records themselves settle it: one reading is plainly newer, or two entities are plainly the same thing, or an unfiled entry plainly fits a kind and a subject that exist. When two readings disagree and nothing says which is right, or a fix would need a judgement only the user can make, change nothing and list it in your report with what would settle it. Entity merges and unfiled resolutions are final, so the bar for those is highest.",
		"",
		"The markdown vault is read-only to you. " + readOnlyShellPrompt,
		"",
		agent.BatchCalls,
		"",
		"Never gist the session that dispatched you, or any session op ungisted does not list: one still running would lose its later turns.",
		"A transcript can hold phi that no record marks: never quote sensitive personal detail, such as health or patient details, from one into a gist, a record or your report, and never mark a gist normal when its transcript holds any.",
		"",
		"Report in a few lines: each fix with the record or entry ids it touched, then \"Left for you:\" and each item you did not fix with why. Never quote content from a record or entry marked phi.",
	}, "\n")
}

func curatorTools() []anthropic.ToolUnionParam {
	tools := append(readOnlyTools(), agent.VaultTools()...)
	tools = append(tools, agent.RecordTools()...)
	return append(tools, agent.RecordReadTools()...)
}

func curatorHandlers() map[string]agent.Handler {
	handlers := readOnlyHandlers()
	for _, set := range []map[string]agent.Handler{
		agent.VaultHandlers(),
		agent.RecordHandlersAs(curatorName, migratorModelName),
		agent.RecordReadHandlers(),
	} {
		for name, handler := range set {
			handlers[name] = handler
		}
	}
	return handlers
}

// RunIfDue starts a quiet run of the agent's skill when none started in the last every and the agent has work, stamping first so two sessions starting together start one run.
func (m *Manager) RunIfDue(job, agentName string, cb agent.Callbacks, stamp string, every time.Duration, now time.Time, onEnd func(report string, err error)) (int, bool, error) {
	def, ok := m.defs[agentName]
	if !ok {
		return 0, false, fmt.Errorf("no %s agent this session", agentName)
	}
	if def.Skill == "" {
		return 0, false, fmt.Errorf("agent %s has no skill to run", agentName)
	}
	if !jobSafe(def) {
		return 0, false, fmt.Errorf("agent %s can change files outside %s, and a job may not", agentName, paths.JobsOut)
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		return 0, false, err
	}
	f, err := os.OpenFile(stamp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0, false, nil
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	data, err := os.ReadFile(stamp)
	if err != nil {
		return 0, false, err
	}
	if last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err == nil && now.Sub(last) < every {
		return 0, false, nil
	}
	if def.Backlog != nil {
		n, err := def.Backlog(m.Session)
		if err != nil {
			return 0, false, fmt.Errorf("backlog: %w", err)
		}
		if n == 0 {
			return 0, false, nil
		}
	}
	dir := skills.Dir
	if def.SkillDir != nil {
		dir = def.SkillDir
	}
	skillDir, err := dir()
	if err != nil {
		return 0, false, err
	}
	if _, err := skills.Load(filepath.Join(skillDir, def.Skill+".yaml")); err != nil {
		return 0, false, fmt.Errorf("skill %s: %w", def.Skill, err)
	}
	if err := f.Truncate(0); err != nil {
		return 0, false, err
	}
	if _, err := f.WriteAt([]byte(now.UTC().Format(time.RFC3339)+"\n"), 0); err != nil {
		return 0, false, err
	}
	task := "Run the " + def.Skill + " skill. It was started by the job scheduler as job " + job + ", and nobody is waiting on it."
	h := m.Roster.start(def.Name, job, task, def.steps(), true)
	h.quiet = true
	if cb.OnRun != nil {
		cb.OnRun(h.run.ID)
	}
	m.detach(m.Roster.Context(), def, task, "", h, cb, nil, onEnd)
	return h.run.ID, true, nil
}

// jobSafe holds only for an upkeep agent limited to read-only commands whose editor, if any, writes only under paths.JobsOut, so a job can never start another job.
func jobSafe(def *Definition) bool {
	if !def.Upkeep || def.Commands == nil || len(def.Commands.Allowed) == 0 {
		return false
	}
	for _, c := range def.Commands.Allowed {
		if c != guardian.Read {
			return false
		}
	}
	edit := reflect.ValueOf(agent.RunEdit).Pointer()
	for _, h := range def.Handlers {
		if reflect.ValueOf(h).Pointer() == edit {
			return writesOnlyJobsOut(def.Policy)
		}
	}
	return true
}

// writesOnlyJobsOut holds when the policy denies every write and allows back only paths under paths.JobsOut.
func writesOnlyJobsOut(p *agent.Rules) bool {
	if p == nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	out := filepath.Join(home, paths.JobsOut)
	fenced := false
	for _, r := range p.Edit {
		switch {
		case r.Verdict == agent.VerdictDeny && r.Pattern == string(filepath.Separator):
			fenced = true
		case r.Verdict != agent.VerdictDeny && r.Pattern != out && !strings.HasPrefix(r.Pattern, out+string(filepath.Separator)):
			return false
		}
	}
	return fenced
}
