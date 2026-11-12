package subagent

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/guardian"
	"github.com/alexbieg/alux/internal/project"
	"github.com/alexbieg/alux/internal/vault"
	"github.com/anthropics/anthropic-sdk-go"
)

// No web tools: a note it reads may be sensitivity: phi, and a migration needs nothing from the web.
const (
	migratorName  = "migrator"
	migratorModel = "claude-sonnet-5-5"
)

func migratorModelName() string { return migratorModel }

// Migrator is nil without a vault and a record store, so it is never offered half-equipped.
func Migrator() *Definition {
	if len(agent.MigrationTools()) == 0 || len(agent.RecordTools()) == 0 {
		return nil
	}
	return &Definition{
		Name: migratorName,
		Description: "migrates one markdown vault note into the record store per run. It claims the next pending note from the " +
			"migration ledger (or the one you name), follows the stored migration procedure, writes the records and finishes the " +
			"claim, then reports the note and record ids. Reading a note that is not migrated yet already starts one, so " +
			"dispatch it yourself only for a bulk run the user asked for; claims are exclusive, so several can run at once. It " +
			"reads the vault and the repo but cannot edit files.",
		Upkeep:     true,
		System:     migratorPrompt,
		Tools:      migratorTools,
		Handlers:   migratorHandlers(),
		Model:      migratorModelName,
		Policy:     readOnlyShell(migratorName),
		Background: true,
		Commands:   &guardian.Criteria{Agent: migratorName, Allowed: []guardian.Category{guardian.Read}, Threshold: guardian.ReadOnlyThreshold},
		Trail:      func(_ agent.ToolKind, label, _ string) string { return label },
	}
}

func migratorPrompt() string {
	return strings.Join([]string{
		"You are a migrator subagent. Another agent, working in the user's terminal, has asked you to move markdown vault notes into the user's record store. You never talk to the user.",
		"Working directory: " + project.Cwd(),
		"Vault: " + vault.Root(),
		"",
		"Start by reading the procedure: vault_record_read op search, kind procedure, query \"migrate one Obsidian vault note\". Follow its steps.",
		"",
		"Write the note's anchor record first, usually its decision, then each other record with its links to the anchor and to siblings already written. One link per pair is enough: get shows it on both ends as link and linked from. Run about on the subject, and on the subject of every note this one names in a [[wikilink]] it extends, refines or depends on, and collect the ids of related records. Give each record you add its links to them: refines when this note extends or refines that one, otherwise supports, depends_on, contradicts or see_also. Only conclude there is nothing to link after those reads come back empty, and say which you ran. Run get on a record before linking to it, and link only when its title actually backs or depends on what you are writing; a shared topic is not a link. Records you split from one another in this run link to each other.",
		"",
		"Supersede a record only when its title, text, attrs or confidence are wrong. To add a link to an existing record, whether yours or another migrator's that landed mid-run, use vault_record op link; never supersede for a link. A supersession carries topical links (about, see_also, refines, depends_on) on its own; pass drop_links for any the new content no longer warrants, and re-add with op link each contradicts, supports or answers link the reply lists that still holds. Leave valid_until unset on a record that stays true as history, and say what later replaced it in the title or text instead. When this note states a fact an existing current record already holds, cite this note on it with vault_record op cite instead of writing a near-copy. A state that describes something which can hold only one value at a time, such as a PR's heads or an experiment's allocation, gets attrs.slot; a refusal naming the slot's holder means supersede that record.",
		"",
		"An add or entity is written at once, and its reply may list similar records or names as a hint. Most need nothing. Link with op link one that what you wrote backs or depends on; when one states this exact fact, write one record superseding both; when one names this exact thing, fold yours into it with entity_edit merge_into.",
		"",
		"Concepts are entities too. Systems, people, tickets, and every concept the note wikilinks or leans on (a domain term, an acronym, a glossary term) get an entity, a term for a concept, so records about them can link. Other migrators run beside you. Right before creating an entity, run about on its name and each likely alias again, and reuse a near-match rather than adding a second entity under another name. A create refused because the name exists means someone just made it: use that one.",
		"",
		"Each record holds what this note says, all of it: every dated callout, update and narrowing written in this note is this note's content, however late it was added. Only text that lives in another note belongs to that note's migration. Never write a title the note itself qualifies or contradicts. When one claim mixes a fact you checked with one you did not, split it into two records rather than letting the checked one lend its confidence to the other.",
		"",
		"When the note describes code in this repository, check it before writing. A claim about code you did not check gets confidence medium and a verified_how that says so.",
		"",
		"Before you finish, walk the note section by section and confirm each statement landed in a record or was left out on purpose. Then read back: about on the subject, get on every record you added, and get on every record you linked to, confirming every link you meant to add is there. Fix a missing link with op link. Never finish done without this read-back.",
		"",
		"Migrate exactly one note unless the task says otherwise: claim it with vault_migration, migrate it, and always finish the claim — done with its record ids, or skipped, failed or pending with a reason. A claim you leave open blocks the note for an hour. When the task hands you a claim token, the note is already claimed for you: skip claiming and finish with that token.",
		"",
		"The markdown vault is read-only to you. Verify a claim against the code with view, glob, grep or a shell command that only looks when that is cheap; otherwise say how it was sourced. " + readOnlyShellPrompt,
		"",
		agent.BatchCalls,
		"",
		"Report in a few lines: the note, how it ended, each record id with its one-line title, any entity you created, the links you added and the about reads behind them, and anything the caller should know — a chain note left pending, a claim you could not verify, each statement you left out on purpose and why. Never quote content from a note marked sensitivity: phi.",
	}, "\n")
}

// StartMigration runs a migrator on a note the caller already claimed. Its outcome reaches the notice strip and its box, never the main agent's next turn.
func (m *Manager) StartMigration(cb agent.Callbacks, path, token string) (int, error) {
	def, ok := m.defs[migratorName]
	if !ok {
		return 0, errors.New("no migrator agent this session")
	}
	task := "Migrate the vault note " + path + ". It is already claimed for you with token " + token +
		": do not claim it again; finish it with that path and token. It was started automatically because the main agent read the note, and nobody is waiting on it."
	h := m.Roster.start(def.Name, "migrate "+strings.TrimSuffix(filepath.Base(path), ".md"), task, def.steps(), true)
	h.quiet = true
	if cb.OnRun != nil {
		cb.OnRun(h.run.ID)
	}
	m.detach(m.Roster.Context(), def, task, "", h, cb, nil, nil)
	return h.run.ID, nil
}

func (m *Manager) Has(name string) bool { _, ok := m.defs[name]; return ok }

func migratorTools() []anthropic.ToolUnionParam {
	tools := append(readOnlyTools(), agent.VaultTools()...)
	tools = append(tools, agent.RecordTools()...)
	tools = append(tools, agent.RecordReadTools()...)
	return append(tools, agent.MigrationTools()...)
}

func migratorHandlers() map[string]agent.Handler {
	handlers := readOnlyHandlers()
	for _, set := range []map[string]agent.Handler{
		agent.VaultHandlers(),
		agent.RecordHandlersAs(migratorName, migratorModelName),
		agent.RecordReadHandlers(),
		agent.MigrationHandlersAs(migratorName, migratorModelName),
	} {
		for name, handler := range set {
			handlers[name] = handler
		}
	}
	return handlers
}
