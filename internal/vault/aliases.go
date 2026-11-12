package vault

// Lexical aliases: two to four short paraphrases of a section, written once and searched
// alongside its body.
//
// The index answers a question phrased in the note's own words 93 times in 100 and a question
// phrased in anyone else's 30 times in 100 — measured over 30 sections, three phrasings each.
// The mechanism behind that gap is blunt: where a query shared two or fewer word stems with the
// section it retrieved nothing at all, 0 of 15, and adding one rare matching word turned a total
// miss into rank 1. Stripping stopwords out of the question changed nothing (27% against 30%),
// which is what rules out fixing this on the read side. So the vocabulary has to be widened
// before the question is asked, and aliases are that: the words a searcher would use, deliberately
// not the words the author used.
//
// Two rules shape everything below.
//
// Aliases cost money, so they do not live in the index. ~/.cache/alux/vault-index.db is
// disposable by design — /vault rebuild drops it, a schema bump recreates it, and it rebuilds
// from markdown in about 200 ms — and anything expensive kept there is re-bought on every one of
// those. They live in ~/.local/share instead, keyed by the section's content, and the index
// holds nothing but a copy it re-reads on every rebuild.
//
// Generation never happens on the search path. Refresh runs on every vault_search and every
// sectioned vault_read, so a missing alias that started an LLM call would make an ordinary search
// block on the network and spend money. Refresh reads what the store already holds and indexes
// it; a section with no aliases is indexed exactly as it was before this file existed.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/anthropics/anthropic-sdk-go"
	_ "modernc.org/sqlite"
)

// An alias is a search phrase, not a sentence: four to eight words of plain vocabulary. The cap
// is what a model that ignored the instruction and wrote prose runs into, and prose in the alias
// column would dilute every real alias beside it.
const maxAliasChars = 60

// Four is where the paper's own examples sit, and the ceiling matters more than it looks: the
// alias column is weighted above body text, so a model that padded the list to ten would be
// writing its own ranking.
const maxAliases = 4

// aliasStorePath is deliberately not under ~/.cache. Everything there is treated as throwaway —
// the index, the write lock — and these rows are the one thing in this package that cannot be
// recomputed from the markdown.
func aliasStorePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Share, "vault-aliases.db")
}

// No user_version stamp, and no drop-and-recreate anywhere in this file. The index gets one
// because throwing it away costs 200 ms; throwing this away costs the whole generation bill
// again. A future column is added with `alter table ... add column`, which is why every column
// here has a default.
//
// model and prompt record what produced a row. Neither is read when deciding what to skip — the
// key is the content, so a tuned prompt does not silently re-bill the vault — but a deliberate
// regeneration after a prompt change needs them to know what to delete.
const aliasSchema = `
create table if not exists aliases (
  hash    text primary key,
  -- One alias per line. Empty is a real answer, and a meaningful one: it records that this text
  -- was sent to a model and came back with nothing usable, so a resumed run does not buy it again.
  aliases text not null,
  model   text not null default '',
  prompt  integer not null default 0,
  input_tokens  integer not null default 0,
  output_tokens integer not null default 0,
  generated integer not null default 0
);
`

// aliasPromptVersion stamps rows with the prompt that produced them. Bumped by anyone changing
// aliasSystemPrompt, so the rows written by the old one can be found and deleted on purpose.
const aliasPromptVersion = 1

type aliasStore struct {
	db *sql.DB
	// Generation writes from several workers at once, and this file is small enough that
	// serialising them in the process is cheaper than letting them collide on SQLite's lock.
	mu sync.Mutex
}

func openAliasStore() (*aliasStore, error) {
	path := aliasStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(aliasSchema); err != nil {
		db.Close()
		return nil, err
	}
	return &aliasStore{db: db}, nil
}

func (s *aliasStore) close() error { return s.db.Close() }

func (s *aliasStore) put(hash string, aliases []string, model string, in, out int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`insert or replace into aliases
		(hash, aliases, model, prompt, input_tokens, output_tokens, generated) values (?,?,?,?,?,?,?)`,
		hash, strings.Join(aliases, "\n"), model, aliasPromptVersion, in, out, time.Now().Unix())
	return err
}

// hashes is every content key the store has an answer for, which is what makes a run resumable.
func (s *aliasStore) hashes() (map[string]bool, error) {
	rows, err := s.db.Query(`select hash from aliases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		have[hash] = true
	}
	return have, rows.Err()
}

func (s *aliasStore) all() (map[string]string, error) {
	rows, err := s.db.Query(`select hash, aliases from aliases where aliases <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var hash, aliases string
		if err := rows.Scan(&hash, &aliases); err != nil {
			return nil, err
		}
		out[hash] = aliases
	}
	return out, rows.Err()
}

// sectionHash is the key. Content-addressed, so a section nobody has touched keeps its aliases
// forever, an edited one silently has none until it is generated again, and the same text in two
// notes is bought once. Nothing ever invalidates a row: the old text's key simply stops being
// asked for, and comes back the moment the text does.
func sectionHash(heading, body string) string {
	sum := sha256.Sum256([]byte(heading + "\n" + body))
	return hex.EncodeToString(sum[:])
}

// aliasesByHash is what Refresh indexes. Nil on any failure — a store that is missing, locked or
// unreadable must cost a search nothing, because the alternative is a vault that cannot be
// searched at all until someone fixes a cache.
func aliasesByHash() map[string]string {
	store, err := openAliasStore()
	if err != nil {
		return nil
	}
	defer store.close()
	all, err := store.all()
	if err != nil {
		return nil
	}
	return all
}

// ---------------------------------------------------------------- generation

// AliasReply is one section's answer, with what it cost.
type AliasReply struct {
	Aliases      []string
	InputTokens  int64
	OutputTokens int64
}

// AliasGenerator is the single LLM call, behind an interface so a test can answer without a
// network and so the model stays the caller's choice.
type AliasGenerator interface {
	Generate(ctx context.Context, heading, body string) (AliasReply, error)
}

// AliasSection is what one section's generation came to. Err is set for a call that failed,
// which leaves the section unwritten and therefore still pending for the next run.
type AliasSection struct {
	Path    string
	Heading string
	Hash    string
	Aliases []string
	Err     error
}

// AliasRun is the cost summary. Sections counts rows considered, the rest count distinct texts:
// the same text in two notes is one call and one bill.
type AliasRun struct {
	Sections     int
	Distinct     int
	Skipped      int
	Generated    int
	Empty        int
	Failed       int
	InputTokens  int64
	OutputTokens int64

	// Reindexed records that the new aliases are searchable now. Generation changes no note, so
	// without this the work would sit invisible until something else edited the vault.
	Reindexed bool

	// Results holds only what this run touched, so a resumed run reports its own work rather
	// than the whole vault's.
	Results []AliasSection
}

// defaultAliasWorkers is a floor on politeness rather than a measurement: the run is thousands of
// independent short calls, and the limit that matters is the API's, not this machine's.
const defaultAliasWorkers = 4

type aliasJob struct {
	hash    string
	path    string
	heading string
	body    string
}

// GenerateAliases is the explicit operation, and the only thing in this package that calls a
// model. It is resumable: every answer is written as it lands, and a hash the store already
// holds is never sent again, so an interrupted run resumes at its cost rather than from the top.
func GenerateAliases(ctx context.Context, vaultRoot string, gen AliasGenerator, concurrency int) (AliasRun, error) {
	var run AliasRun
	if gen == nil {
		return run, fmt.Errorf("no alias generator configured")
	}
	if concurrency <= 0 {
		concurrency = defaultAliasWorkers
	}

	x, err := OpenIndex(vaultRoot)
	if err != nil {
		return run, err
	}
	defer x.Close()
	// Generating against a stale index would buy aliases for text the vault no longer holds, and
	// their keys would never be asked for again.
	if _, err := x.Refresh(); err != nil {
		return run, err
	}

	store, err := openAliasStore()
	if err != nil {
		return run, err
	}
	defer store.close()
	have, err := store.hashes()
	if err != nil {
		return run, err
	}

	jobs, counts, err := pendingAliases(x, have)
	if err != nil {
		return run, err
	}
	run.Sections, run.Distinct, run.Skipped = counts.sections, counts.distinct, counts.skipped
	if len(jobs) == 0 {
		return run, nil
	}

	model := llm.AliasModel()
	var mu sync.Mutex
	queue := make(chan aliasJob)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				reply, err := gen.Generate(ctx, job.heading, job.body)
				result := AliasSection{Path: job.path, Heading: job.heading, Hash: job.hash, Err: err}
				if err == nil {
					result.Aliases = reply.Aliases
					// Written even when the model answered with nothing usable: a recorded empty
					// answer is what stops a resumed run paying for the same refusal again.
					if perr := store.put(job.hash, reply.Aliases, model, reply.InputTokens, reply.OutputTokens); perr != nil {
						result.Err = perr
					}
				}
				mu.Lock()
				run.InputTokens += reply.InputTokens
				run.OutputTokens += reply.OutputTokens
				switch {
				case result.Err != nil:
					run.Failed++
				case len(result.Aliases) == 0:
					run.Generated++
					run.Empty++
				default:
					run.Generated++
				}
				run.Results = append(run.Results, result)
				mu.Unlock()
			}
		}()
	}

	var cancelled error
feed:
	for _, job := range jobs {
		select {
		case queue <- job:
		case <-ctx.Done():
			cancelled = ctx.Err()
			break feed
		}
	}
	close(queue)
	wg.Wait()

	// Generation moves no file, so nothing about the vault looks stale and Refresh would reindex
	// nothing: without this the aliases just bought would sit unsearchable until the note next
	// changed. The index is derived and rebuilds from markdown in about 200 ms, which is the
	// cheapest honest way to pick them all up.
	if run.Generated > run.Empty {
		if _, err := x.Rebuild(); err != nil {
			return run, err
		}
		run.Reindexed = true
	}
	return run, cancelled
}

type aliasCounts struct{ sections, distinct, skipped int }

// pendingAliases is the work left to do: one job per distinct text, in path order.
//
// Only searchable sections, because a bare link trailer is withheld from the full-text index and
// an alias on one could never match anything. Archived notes are left out for the same reason —
// Search filters them, so their aliases would be bought and never used.
func pendingAliases(x *Index, have map[string]bool) ([]aliasJob, aliasCounts, error) {
	var counts aliasCounts
	rows, err := x.db.Query(`select n.path, s.heading, s.body from sections s
		join notes n on n.id = s.note_id
		where s.searchable and n.status <> 'archived' order by n.path, s.seq`)
	if err != nil {
		return nil, counts, err
	}
	defer rows.Close()

	var jobs []aliasJob
	seen := map[string]bool{}
	for rows.Next() {
		var job aliasJob
		if err := rows.Scan(&job.path, &job.heading, &job.body); err != nil {
			return nil, counts, err
		}
		counts.sections++
		job.hash = sectionHash(job.heading, job.body)
		if seen[job.hash] {
			continue
		}
		seen[job.hash] = true
		counts.distinct++
		if have[job.hash] {
			counts.skipped++
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, counts, rows.Err()
}

// ---------------------------------------------------------------- the model

// The instruction is the whole feature. Anything the model writes that repeats the section's own
// distinctive words is wasted: those terms already match, and the queries that fail are exactly
// the ones that share no stem with the text. Hence the four directions to move in — synonym,
// category, jargon out, jargon in — and the explicit ban on reusing the nouns.
const aliasSystemPrompt = `You write search aliases for sections of a personal knowledge vault.

Given one section, write 2-4 short phrases that someone who half-remembers it might type into a
search box months later. They are index terms, not a summary: nobody reads them.

Rules:
- Use DIFFERENT words from the section. Do not reuse its distinctive nouns, names, product names
  or jargon — the search already matches those, so repeating them buys nothing.
- Move the vocabulary: a synonym for the main verb or noun, the generic category the specific
  thing belongs to, the plain-English version of any jargon, the jargon for anything said plainly.
- Each phrase is at most eight words. No punctuation, no explanation, no sentences.
- Write phrases a person would actually search for, not restatements nobody would type.
- If the section is boilerplate, a list of links, or holds nothing anyone would search for,
  return an empty list. An empty list is a valid answer and is better than a vague one.

Example. A section recording "bought a Fitbit on the 3rd" gets: picked up a fitness tracker, got
a step counter, purchased a wearable. Note that none of them say Fitbit.

Answer by calling the aliases tool, and nothing else.`

// Sections run to a median 628 bytes, but a few are enormous and the opening of one says what it
// is about. Paying for 40 KB of appendix to write four phrases is not worth it.
const maxAliasBodyChars = 4000

var aliasTool = anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
	Name:        "aliases",
	Description: anthropic.String("Record the search aliases for this section."),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{
			"aliases": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"maxItems":    maxAliases,
				"description": "2-4 short search phrases, or an empty list if none are worth writing.",
			},
		},
		Required:    []string{"aliases"},
		ExtraFields: map[string]any{"additionalProperties": false},
	},
}}

// modelAliases is the real generator. Separate from GenerateAliases so the run — its resumption,
// its counting, its concurrency — is testable without a network.
type modelAliases struct {
	client anthropic.Client
	model  string
}

// ModelGenerator is the generator alux uses: the shared Anthropic client, on the alias
// model. A caller that wants a different model sets ALUX_ALIAS_MODEL.
func ModelGenerator() AliasGenerator {
	return modelAliases{client: llm.Client(), model: llm.AliasModel()}
}

func (m modelAliases) Generate(ctx context.Context, heading, body string) (AliasReply, error) {
	if len(body) > maxAliasBodyChars {
		body = body[:maxAliasBodyChars]
	}
	msg, err := m.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: 256,
		System:    []anthropic.TextBlockParam{{Text: aliasSystemPrompt}},
		Tools:     []anthropic.ToolUnionParam{aliasTool},
		// Forced, because the answer is the only thing wanted: a model left free to reply in prose
		// here produces a paragraph about how it would write aliases.
		ToolChoice: anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "aliases"}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(
			anthropic.NewTextBlock("Heading: " + heading + "\n\nSection:\n" + body))},
	})
	if err != nil {
		return AliasReply{}, err
	}
	reply := AliasReply{
		InputTokens:  msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens,
		OutputTokens: msg.Usage.OutputTokens,
	}
	for _, block := range msg.Content {
		if block.Type == "tool_use" && block.Name == "aliases" {
			reply.Aliases = aliasesFrom(block.Input)
			break
		}
	}
	return reply, nil
}

// aliasesFrom reads the structured answer, and reads anything it cannot understand as no aliases
// at all. A run of several thousand calls will meet a refusal, a truncated object and a model
// that answered in prose; each of those is worth one section's aliases, never the run.
func aliasesFrom(raw json.RawMessage) []string {
	var answer struct {
		Aliases []string `json:"aliases"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return nil
	}
	var out []string
	for _, alias := range answer.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || len(alias) > maxAliasChars {
			continue
		}
		out = append(out, alias)
		if len(out) == maxAliases {
			break
		}
	}
	return out
}

// AliasSummary is what /vault aliases prints: what the run cost and what is left.
func (r AliasRun) AliasSummary(took time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  Generated aliases for %d of %d sections in %s.\n",
		r.Generated, r.Distinct, took.Round(time.Second))
	fmt.Fprintf(&b, "  %d already had them, %d produced none, %d failed and are left for the next run.\n",
		r.Skipped, r.Empty, r.Failed)
	fmt.Fprintf(&b, "  %d input tokens, %d output tokens.\n", r.InputTokens, r.OutputTokens)
	if r.Generated > 0 && !r.Reindexed {
		fmt.Fprintf(&b, "  Nothing new was indexed.\n")
	}
	return b.String()
}
