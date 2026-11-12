// Command alux is a terminal coding agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/attach"
	"github.com/alexbieg/alux/internal/awake"
	"github.com/alexbieg/alux/internal/classify"
	"github.com/alexbieg/alux/internal/commands"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/jobs"
	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/mcpclient"
	"github.com/alexbieg/alux/internal/mine"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/procs"
	"github.com/alexbieg/alux/internal/programstatus"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/records"
	"github.com/alexbieg/alux/internal/registry"
	"github.com/alexbieg/alux/internal/report"
	"github.com/alexbieg/alux/internal/resolve"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/shell"
	"github.com/alexbieg/alux/internal/skills"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/ui"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/alexbieg/alux/internal/workers"
	"github.com/anthropics/anthropic-sdk-go"
)

// Streaming deltas arrive far faster than a terminal can usefully redraw, so notifications are
// coalesced. The trailing edge matters more than the leading one: whatever the final state is,
// it must always get drawn.
func coalesce(d time.Duration, fn func()) func() {
	var mu sync.Mutex
	pending := false
	return func() {
		mu.Lock()
		if pending {
			mu.Unlock()
			return
		}
		pending = true
		mu.Unlock()
		time.AfterFunc(d, func() {
			mu.Lock()
			pending = false
			mu.Unlock()
			fn()
		})
	}
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "skills":
			os.Exit(skillsCommand(os.Args[2:]))
		case "attach":
			os.Exit(attachCommand(os.Args[2:]))
		case "stop":
			os.Exit(stopCommand(os.Args[2:]))
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "alux:", err)
		os.Exit(1)
	}
}

// skillsCommand returns the exit code: 0 all valid, 1 any invalid, 2 usage or I/O error.
func skillsCommand(args []string) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(os.Stderr, "usage: alux skills validate [file.yaml...]")
		return 2
	}
	dir, err := skills.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "alux:", err)
		return 2
	}
	ok, err := skills.Validate(os.Stdout, args[1:], dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "alux:", err)
		return 2
	}
	if !ok {
		return 1
	}
	return 0
}

// mainCommands is the main agent's threshold; the session's mode replaces Allowed on every command.
var mainCommands = guardian.Criteria{
	Agent:     "main",
	Allowed:   []guardian.Category{guardian.Read, guardian.Build, guardian.Edit, guardian.Record},
	Threshold: guardian.MainThreshold,
}

// claimSlot moves the process into a workspace of its own when it starts in the default workspace of a repo with a pool.
func claimSlot(here bool) (*orchestrator.Pool, *orchestrator.Slot, error) {
	if here {
		return nil, nil, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	if wd, err = filepath.EvalSymlinks(wd); err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	pool, err := orchestrator.PoolFor(ctx, wd)
	if err != nil || pool == nil {
		return nil, nil, err
	}
	pool.Log = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	pool.Merged = func(ctx context.Context, slug string, heads []string) (map[string][]prs.MergedPR, error) {
		return prs.Merged(ctx, prs.GhGraphQL, slug, heads)
	}
	slot, err := pool.Claim(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w; alux --here runs in this checkout instead", err)
	}
	dir := slot.Dir
	if rel, err := filepath.Rel(pool.Origin, wd); err == nil && rel != "." {
		if info, err := os.Stat(filepath.Join(slot.Dir, rel)); err == nil && info.IsDir() {
			dir = filepath.Join(slot.Dir, rel)
		}
	}
	if err := os.Chdir(dir); err != nil {
		pool.Release(ctx, slot)
		return nil, nil, err
	}
	pool.Log = nil
	agent.Guard = &agent.SlotGuard{Slot: slot.Dir, Origin: pool.Origin}
	return pool, slot, nil
}

func run() error {
	here := flag.Bool("here", false, "run in this checkout instead of a pool workspace, with edits allowed and no description needed first")
	worker := flag.String("worker", "", "with --serve, the worker key the session works on until its skill run closes")
	modeFlag := flag.String("mode", "", "a worker's mode: read-only, local or auto")
	prompt := flag.String("prompt", "", "a worker's first message")
	baseFlag := flag.String("base", orchestrator.BaseMain, "where a worker starts: main, the freshly fetched trunk, or a commit id")
	serve := flag.Bool("serve", false, "run with no UI, serving the session to whichever TUI runs alux attach")
	inline := flag.Bool("inline", false, "run the session inside this TUI, so it ends when the TUI quits")
	flag.Parse()
	if *serve && *inline {
		return fmt.Errorf("--serve and --inline are separate ways to run; pick one")
	}

	if err := llm.KeyError(); err != nil {
		return err
	}
	isWorker := *worker != ""
	// Bare alux: the session runs as its own process, which claims the slot; this one only attaches.
	if !*serve && !*inline && !isWorker {
		pid, err := startServer(*here, "", os.Stderr)
		if err != nil {
			return err
		}
		return attachTo(pid)
	}
	var key string
	var mode session.Mode
	if isWorker {
		var err error
		if key, mode, err = checkWorker(*here, *serve, *worker, *modeFlag); err != nil {
			return err
		}
		if strings.TrimSpace(*baseFlag) == "" {
			return fmt.Errorf("--base is main or a commit id")
		}
	}

	// Before anything reads the working directory, so every tool, the sandbox and the registry see the slot.
	pool, slot, err := claimSlot(*here)
	if err != nil {
		return err
	}
	if slot != nil {
		defer pool.Release(context.Background(), slot)
	}
	if err := requireSlot(isWorker, slot); err != nil {
		return err
	}
	if isWorker {
		note, err := pool.StartAt(context.Background(), slot, strings.TrimSpace(*baseFlag))
		if err != nil {
			return fmt.Errorf("moving to base %s: %w", *baseFlag, err)
		}
		if note != "" {
			fmt.Fprintln(os.Stderr, "alux:", note)
		}
	}

	var program *tea.Program
	var server atomic.Pointer[attach.Server]
	sendNotice := func(text string) {
		if program != nil {
			program.Send(ui.NoticeMsg{Text: text})
		} else if *serve {
			fmt.Fprintln(os.Stderr, "alux:", text)
		}
		if srv := server.Load(); srv != nil {
			srv.Notice(text)
		}
	}
	// publishServed is set before server is stored, so a non-nil server means it is set too.
	var publishServed func()

	// The program has to exist before the session can notify it, and the session has to exist
	// before the model can be built, so the notifier is indirected through a pointer that is
	// filled in below.
	notify := coalesce(25*time.Millisecond, func() {
		if program != nil {
			program.Send(ui.Changed())
		}
		if srv := server.Load(); srv != nil {
			srv.Changed()
			publishServed()
		}
	})

	// Subagents are assembled here rather than inside the agent package, which is what keeps the
	// dependency one-way: subagent imports agent, never the reverse. Adding a kind of subagent is
	// a definition in this list.
	mcp := mcpclient.New(sendNotice)

	var prTools, draftTools []anthropic.ToolUnionParam
	prHandlers, draftHandlers := map[string]agent.Handler{}, map[string]agent.Handler{}
	var poller *prs.Poller
	prConf, prsOn, prErr := prs.Load()
	if prsOn {
		poller = prs.NewPoller(prConf, prs.GhGraphQL, prs.DefaultCache())
		prTools, prHandlers = poller.Tools(), poller.Handlers()
		draftTools, draftHandlers = poller.DraftTools(), poller.DraftHandlers()
		poller.OnUpdate = func() {
			if program != nil {
				program.Send(ui.PRsMsg{States: poller.Snapshot()})
			}
			if srv := server.Load(); srv != nil {
				srv.PRsChanged(poller.Snapshot())
			}
		}
		poller.OnReview = func(r reviewstore.Review) {
			if program != nil {
				program.Send(ui.ReviewMsg{Review: r})
			}
			if srv := server.Load(); srv != nil {
				srv.ReviewLoaded(r)
			}
		}
	}

	reviewerTools, reviewerHandlers := prTools, prHandlers
	if poller != nil {
		reviewerTools = append(slices.Clone(prTools), poller.SubmitTools()...)
		reviewerHandlers = merge(prHandlers, poller.SubmitHandlers())
	}

	keeper := awake.New()
	if keeper != nil {
		keeper.OnChange = notify
	}
	roster := subagent.NewRoster(notify)
	roster.Awake = keeper
	if poller != nil {
		poller.OnPush = func(change string) string {
			if run, ok := roster.Shipped(change); ok {
				return fmt.Sprintf("Run %d's change is pushed, so it no longer waits to land.", run)
			}
			return ""
		}
	}
	// A definition that cannot work stops alux here rather than at its first dispatch. The
	// failure is in the code, and a capability gate that was configured wrongly must not run in a
	// weakened form.
	defs := []*subagent.Definition{
		subagent.Research(), subagent.Coder(mcp), subagent.PairedCoder(mcp),
		subagent.Reviewer(reviewerTools, reviewerHandlers), subagent.Migrator(), subagent.Curator(), subagent.Digest(), subagent.Verifier(),
	}
	agents, err := subagent.NewManager(roster, defs...)
	if err != nil {
		return err
	}
	agents.Notify = sendNotice
	if repo := project.JJRoot(); repo != "" {
		prep, err := orchestrator.LoadPrepare(project.Origin())
		if err != nil {
			return err
		}
		if prep != nil && (prep.Coders || prep.Pool) {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			agents.Layers = &orchestrator.CoderLayers{Repo: repo, Root: orchestrator.LayersRoot(home, project.Origin()), Prepare: prep, Freeze: slot != nil}
		}
	}
	sessionID := subagent.NewID()
	agents.Session = sessionID
	if path := subagent.DefaultDispatchLog(); path != "" {
		agents.Log = subagent.FileLog(path)
	}
	if path := subagent.DefaultPeerLog(); path != "" {
		agents.PeerLog = subagent.PeerLog(path)
	}
	agents.Transcripts = subagent.DefaultTranscriptDir()
	agents.Routing = subagent.LoadRouting
	guardConf := guardian.Config()
	judge := guardian.New(guardConf, sendNotice)
	agents.Guardian = judge
	agents.Tier = judge.Tier
	if err := mainCommands.Validate(); err != nil {
		return err
	}

	shells := procs.NewRegistry()
	shells.Awake = keeper
	landHandlers := map[string]agent.Handler{}
	var landTools []anthropic.ToolUnionParam
	var lander *orchestrator.Lander
	if slot != nil {
		lander = &orchestrator.Lander{Pool: pool, Slot: slot}
		if !lander.PRFlow() {
			landHandlers, landTools = lander.Handlers(), lander.Tools()
		}
	}

	// Dispatches and the web tools, which are the calls that stop for nothing.
	concurrent := agents.Concurrent()
	maps.Copy(concurrent, agent.WebConcurrent())
	if poller != nil {
		maps.Copy(concurrent, poller.Concurrent())
	}

	concurrent[workersTool] = true

	handlers := merge(agent.BuiltinHandlers(), agents.Handlers(), shells.Handlers(), prHandlers, draftHandlers, landHandlers)
	handlers[workersTool] = workersHandler(project.Origin())
	if agents.Has("migrator") && agent.AutoMigrateOn() {
		handlers["vault_read"] = agent.AutoMigrate(handlers["vault_read"], agents.StartMigration)
	}

	// Read once at boot: the search tool is non-deferred, so adding it mid-session would cost a cache miss.
	deferred := mcpclient.LoadDeferred()
	toolSearch := anthropic.ToolUnionParam{OfToolSearchToolRegex20251119: &anthropic.ToolSearchToolRegex20251119Param{
		Type: anthropic.ToolSearchToolRegex20251119TypeToolSearchToolRegex20251119,
	}}

	a := &agent.Agent{
		System:   agent.SystemPrompt(agents.SystemNote()),
		Standing: agent.NewRuleInjector(""),
		// Recomputed per request rather than captured: MCP servers connect after the first
		// turn has already started, and a slow one should cost that turn, not the boot.
		Tools: func() []anthropic.ToolUnionParam {
			tools := append(withBash(agent.BuiltinTools(), shells.BashTool()), agents.Tools()...)
			tools = append(tools, shells.Tools()...)
			tools = append(tools, prTools...)
			tools = append(tools, draftTools...)
			tools = append(tools, landTools...)
			tools = append(tools, workersParam())
			if !isWorker {
				tools = append(tools, workerStartParam(), workerStopParam())
			}
			if len(deferred) > 0 {
				tools = append(tools, toolSearch)
			}
			return append(tools, mcp.MainTools(deferred)...)
		},
		Handlers: handlers,
		// The shell that hands a command over at the timeout instead of killing it.
		Fallback:     agent.McpFallback(mcp, shells.Bash()),
		BeforeTurn:   mcp.Ready,
		Backgrounded: shells.Backgrounded,
		// Everything else stops for approval, and the session has one channel to park on.
		Concurrent: concurrent,
	}

	var logPath string
	if isWorker {
		if logPath, err = workers.LogPath(key); err != nil {
			return err
		}
		a.OnUnprompted = auditLog(logPath)
	}
	s := session.New(a, notify)
	if isWorker {
		s.OnPrompt = promptLog(strings.TrimSuffix(logPath, ".log") + ".prompts.jsonl")
	}
	s.WireSkills()
	if !isWorker {
		handlers[workerStartTool] = workerStartHandler(func() string { return string(s.EffectiveMode()) })
		handlers[workerStopTool] = workerStopHandler(project.Origin())
	}
	a.Screen = guardian.Gate{Client: judge, Criteria: mainCommands, Allowed: s.Allowed}
	a.Allows = s.Allows
	if sandbox.Enabled() {
		agents.Sandbox = true
		a.Sandbox = func(ctx context.Context, commands [][]string) (sandbox.Wrap, error) {
			mode := s.EffectiveMode()
			p := sandbox.Profile{Root: project.Root(), Write: true, Network: mode == session.ModeAuto}
			switch {
			case mode == session.ModeReadOnly:
				p.Write, p.ReadOnly = false, "the session is in read-only mode"
			case agent.Guard != nil && !agent.Guard.Writable(ctx, commands):
				p.Write, p.ReadOnly = false, "this session's change had no description when the call started, and the "+
					"sandbox is set for the whole call, so a write chained after jj describe in the same call is refused; "+
					"run the write in the next call"
			}
			return sandbox.Build(p)
		}
	}
	// Set after the session exists, since the session is what collects steering and run outcomes.
	a.Pending = s.PendingBlock
	a.AskTrailing = true
	s.Roster = roster
	s.Agents = agents
	s.WireDispatches()
	s.Procs = shells
	s.Explain = session.ExplainCommand(llm.Client())
	s.Namer = session.NameSession(llm.Client())
	s.Awake = keeper
	s.ID = sessionID
	s.Transcript = subagent.NewTranscript(subagent.DefaultSessionDir(), sessionID)
	s.Sequences = mine.NewRecorder(mine.DefaultDir(), sessionID)
	if !isWorker {
		s.Mine = func() { _ = mine.Run(mine.DefaultDir()) }
		go s.Mine()
		s.ClaimWorkers = func() []workers.Finished { return workers.Claim(project.Origin(), os.Getpid()) }
	}
	if path := subagent.DefaultOutcomeLog(); path != "" {
		s.Followups = subagent.FollowupLog(path)
	}
	if poller != nil {
		s.PRs = poller.Command
	}
	if slot != nil {
		s.Restart = func() error { return pool.Restart(context.Background(), slot) }
	}
	if lander != nil {
		s.Land = lander.Command
		s.Resume = lander.Resume
	}
	jobList, jobsErr := jobs.Load()
	if jobsErr == nil {
		s.Jobs = func() report.Table { return jobs.Table(jobList, time.Now()) }
	}
	s.Workers = func() []workers.Worker { return workers.Read(project.Origin()) }
	if lander != nil && !lander.PRFlow() {
		commands.LandCandidates = landSuggestions(lander.Candidates)
	}
	// Off unless settings.json configures it: the sidecar is an 808 MB checkpoint in a Python
	// process, which is too much to boot for a feature nobody asked for.
	if conf, ok := classify.Config(); ok && !isWorker {
		classifier := classify.Start(context.Background(), conf, sendNotice)
		s.Classifier = classifier
		defer classifier.Close()
	}
	// What makes a detached run reach an idle agent: without it, a finding would sit on the
	// roster until the user happened to type something.
	roster.OnOutcome = s.Wake
	roster.OnCheckIn = s.Wake
	if sh, err := shell.New(project.Cwd()); err == nil {
		s.Shell = sh
		defer s.CloseShell()
	} else {
		sendNotice("the shell could not be kept between calls, so each runs fresh: " + err.Error())
	}
	shells.OnExit = s.Wake
	shells.OnFinish = s.ProcFinished
	shells.OnPollEnd = s.PollEnded
	// And what makes the dot row gain and lose a process without waiting for the next redraw.
	shells.OnChange = notify
	model := ui.New(s)
	if lander != nil {
		model.Land = lander
		model.VCSScope, model.VCSSlug = lander.StatusScope(), lander.Slug()
	}
	if poller != nil {
		model.PRs = poller
	}
	model.Names = &resolve.Resolver{
		Vault:        vault.Root(),
		TicketsDir:   vault.TicketsDir(),
		TicketServer: resolve.TicketServer(),
		MCP:          mcp,
		OnName: coalesce(25*time.Millisecond, func() {
			if program != nil {
				program.Send(ui.NamesMsg{})
			}
		}),
	}
	if records.Path() != "" {
		model.Names.Records = func() (resolve.Labels, error) {
			store, err := agent.OpenRecordStore()
			if err != nil {
				return nil, err
			}
			return store, nil
		}
	}
	if poller != nil {
		model.Names.Queue = poller.Snapshot
		poller.OnTitle = func(number int, title string) { model.Names.Learn(fmt.Sprintf("#%d", number), title) }
		// A bare #number can only mean a repo when exactly one is configured.
		if len(prConf.Repos) == 1 {
			model.Names.Gh, model.Names.Repo = prs.GhRun, prConf.Repos[0].Slug()
		}
	}

	// Cross-session awareness: a row of dots and a notice when another pane needs attention.
	name := project.Origin()
	if name == "" {
		name = project.Root()
	}
	reg := registry.Default(filepath.Base(name), project.Cwd())
	model.Registry = reg
	model.Origin = project.Origin()
	reachWorkers(model)
	model.Setup = &ui.Setup{
		Project:     filepath.Base(name),
		Missing:     missingTools("rg", "fd"),
		AuthWarning: llm.AuthFileWarning(),
		Records:     records.Path(),
		Vault:       vault.Root() != "",
		PRs:         poller != nil,
		Calendar:    agent.CalendarOn(),
	}
	if vault.Root() == "" {
		commands.Unconfigured["vault"] = true
	}
	if poller == nil {
		commands.Unconfigured["prs"] = true
	}
	if lander == nil {
		commands.Unconfigured["land"], commands.Unconfigured["resume"] = true, true
	}
	workspace := project.Workspace()
	reg.Publish(registry.Patch{Session: &sessionID, Workspace: &workspace})
	defer reg.Remove()

	var run *workerRun
	switch {
	case *serve:
		sock, err := attach.SocketPath(os.Getpid())
		if err != nil {
			return err
		}
		srv, err := attach.Listen(sock, s)
		if err != nil {
			return err
		}
		ctlPath, err := attach.ControlPath(os.Getpid())
		if err != nil {
			return err
		}
		ctl, err := attach.ListenControl(ctlPath, s)
		if err != nil {
			return err
		}
		ctl.Stop = func() error {
			if srv.Watched() {
				return errors.New("a UI is attached to it")
			}
			return syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
		ctl.Watched = srv.Watched
		go ctl.Serve()
		defer ctl.Close()
		origin := project.Origin()
		reg.Publish(registry.Patch{Origin: &origin})
		srv.OnPane = func(p attach.Pane) {
			reg.Publish(registry.Patch{Unseen: &p.Unseen, Bookmark: &p.Bookmark, Change: &p.Change})
		}
		detached := true
		reg.Publish(registry.Patch{Detached: &detached})
		srv.OnWatch = func(watched bool) {
			detached := !watched
			reg.Publish(registry.Patch{Detached: &detached})
		}
		if lander != nil {
			srv.Land = lander
			srv.Hello = attach.Hello{Land: true, PRFlow: lander.PRFlow(), VCSScope: lander.StatusScope(), VCSSlug: lander.Slug()}
		}
		if poller != nil {
			srv.PRs, srv.PRStates, srv.Hello.PRs = poller, poller.Snapshot, true
		}
		srv.Name = model.Names.Lookup
		srv.Hello.Setup = model.Setup
		publish := servedState(s, srv, reg)
		publishServed = publish
		if isWorker {
			if run, err = startWorkerRun(s, reg, key, mode, *prompt, logPath, sessionID); err != nil {
				return err
			}
			publishServed = func() { publish(); run.publish() }
		}
		server.Store(srv)
	default:
		program = tea.NewProgram(model)
	}
	s.Start()
	if pool != nil {
		go func() {
			if err := pool.Spare(context.Background()); err != nil {
				sendNotice("workspace pool: no spare prepared for the next session — " + err.Error())
			}
		}()
	}
	if jobsErr != nil {
		go sendNotice("jobs: " + jobsErr.Error())
	} else if agents != nil && !isWorker {
		jobsCtx, stopJobs := context.WithCancel(context.Background())
		defer stopJobs()
		start := func(j jobs.Job, stamp string) error {
			// Some agents exist only in some sessions (the digest needs web search); a session that has it runs the job.
			if !agents.Has(j.Agent) {
				return nil
			}
			started := time.Now()
			runID := make(chan int, 1)
			onEnd := func(report string, err error) {
				e := jobs.Entry{Job: j.Name, Run: <-runID, Session: sessionID, PID: os.Getpid(), Started: started, Ended: time.Now(), Outcome: "ok"}
				e.Summary, _, _ = strings.Cut(strings.TrimSpace(report), "\n")
				if err != nil {
					e.Outcome, e.Err = "failed", err.Error()
				}
				if err := jobs.Append(e); err != nil {
					sendNotice("jobs: run log: " + err.Error())
				}
			}
			id, ok, err := agents.RunIfDue(j.Name, j.Agent, agent.Callbacks{OnRun: s.ShowRun}, stamp, j.Every, started, onEnd)
			if ok {
				// Logged before onEnd is unblocked, so the end entry always follows it.
				if err := jobs.Append(jobs.Entry{Job: j.Name, Run: id, Session: sessionID, PID: os.Getpid(), Started: started, Outcome: jobs.Running}); err != nil {
					sendNotice("jobs: run log: " + err.Error())
				}
				runID <- id
			}
			return err
		}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		// Off the main goroutine: the box and any notice go through Send, which waits for the program.
		go jobs.Loop(jobsCtx, jobList, ticker.C, start, sendNotice)
	}
	if srv := server.Load(); program != nil || srv != nil {
		go func() {
			err := guardian.Check(context.Background(), guardConf)
			if program != nil {
				program.Send(ui.GuardianMsg{Err: err})
			}
			if srv != nil {
				srv.GuardianChecked(err)
			}
		}()
	}

	// Asked for here rather than at import so that nothing else pulling in the agent connects
	// to the user's servers by accident. Not awaited: a slow server costs the first turn.
	mcp.Connect(context.Background())

	if prErr != nil {
		go sendNotice(prErr.Error() + " — PR tracking is off until it is fixed")
	}
	go func() {
		for _, notice := range agent.ExcludeDrift() {
			sendNotice(notice)
		}
	}()
	if poller != nil && !isWorker {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		poller.Start(ctx)
	}

	// Once the servers are up, say where the capability rules and the connected servers disagree:
	// a subagent admitting a tool its own server does not call read-only, or a server no subagent
	// reaches at all. It cannot run before this point — the inventory is empty until the servers
	// answer — and it changes nothing either way: rules broader or narrower than their author
	// meant are something only their author can settle.
	go func() {
		ctx := context.Background()
		mcp.Ready(ctx)
		for _, warning := range agents.AuditTools(mcp) {
			sendNotice("mcp: " + warning)
		}
	}()

	// After the window lookup, so it is never shown against the fallback; again once MCP tools exist.
	go func() {
		ctx := context.Background()
		llm.DiscoverContextWindow(ctx)
		s.MeasureBaseline(ctx)
		mcp.Ready(ctx)
		s.MeasureBaseline(ctx)
	}()

	stopWatching := reg.Watch(time.Second, func(others []registry.Info) {
		if program != nil {
			program.Send(ui.SessionsMsg{Sessions: others})
		}
	})
	defer stopWatching()

	if len(agent.ContextFiles) > 0 {
		s.Show("context: " + join(agent.ContextFiles))
	}

	switch {
	case *serve:
		err = serveSession(s, server.Load(), run)
	default:
		_, err = program.Run()
		// A terminal may keep a quit program's last status on the pane until it is cleared.
		fmt.Fprint(os.Stdout, programstatus.Clear)
	}
	teardown(shells, roster, keeper, mcp)
	return err
}

// teardown stops what the session started, for either driver.
func teardown(shells *procs.Registry, roster *subagent.Roster, keeper *awake.Keeper, mcp *mcpclient.Manager) {
	// Killed rather than waited for, unlike a subagent: a background command may be a server that
	// never exits, so waiting would cost the whole timeout and leak it anyway.
	if n := shells.KillAll(); n > 0 {
		fmt.Fprintf(os.Stderr, "alux: stopped %d background command(s)\n", n)
	}
	// A background subagent dispatched just before quitting is still running. Give it a moment
	// rather than leaving a note created but unindexed.
	if !roster.Drain(15 * time.Second) {
		fmt.Fprintln(os.Stderr, "alux: gave up waiting on a background agent")
	}
	agent.CloseBrowsers()
	keeper.Close()
	mcp.Close()
}

func missingTools(names ...string) []string {
	var out []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			out = append(out, n)
		}
	}
	return out
}

// merge folds one handler table into another, later entries winning.
func merge(tables ...map[string]agent.Handler) map[string]agent.Handler {
	out := map[string]agent.Handler{}
	for _, table := range tables {
		for name, handler := range table {
			out[name] = handler
		}
	}
	return out
}

// withBash puts the main agent's own bash schema in place of the shared one.
func withBash(tools []anthropic.ToolUnionParam, bash *anthropic.ToolParam) []anthropic.ToolUnionParam {
	for i, t := range tools {
		if t.OfTool != nil && t.OfTool.Name == bash.Name {
			tools[i] = anthropic.ToolUnionParam{OfTool: bash}
		}
	}
	return tools
}

func join(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}
