package subagent

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	editorName = "str_replace_based_edit_tool"
	seedBytes  = 80_000
)

type viewSpan struct{ start, end int }

type seedFile struct {
	path  string
	whole bool
	spans []viewSpan
}

// Seed is one file or range the main agent hands a subagent to start from.
type Seed struct {
	Path      string `json:"path"`
	ViewRange []int  `json:"view_range,omitempty"`
}

// addView folds one view of path into files, keeping first-view order.
func addView(files []*seedFile, path string, viewRange []int) []*seedFile {
	var span viewSpan
	whole := len(viewRange) != 2
	if !whole {
		span = viewSpan{viewRange[0], viewRange[1]}
		if span.start < 1 || (span.end != -1 && span.end < span.start) {
			return files
		}
		whole = span == viewSpan{1, -1}
	}
	i := slices.IndexFunc(files, func(f *seedFile) bool { return f.path == path })
	if i < 0 {
		files = append(files, &seedFile{path: path})
		i = len(files) - 1
	}
	if whole {
		files[i].whole = true
	} else {
		files[i].spans = append(files[i].spans, span)
	}
	return files
}

func settled(files []*seedFile) []seedFile {
	var out []seedFile
	for _, f := range files {
		f.spans = mergeSpans(f.spans)
		if f.whole || (len(f.spans) == 1 && f.spans[0] == (viewSpan{1, -1})) {
			f.spans = nil
		}
		out = append(out, *f)
	}
	return out
}

func existingFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveSeeds places each seed in dir: an absolute path under one of roots moves to the same place under dir.
func resolveSeeds(seeds []Seed, dir string, roots ...string) []seedFile {
	var files []*seedFile
	for _, s := range seeds {
		path := s.Path
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		} else {
			for _, root := range roots {
				if root == "" {
					continue
				}
				if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					path = filepath.Join(dir, rel)
					break
				}
			}
		}
		path = filepath.Clean(path)
		if !existingFile(path) {
			continue
		}
		files = addView(files, path, s.ViewRange)
	}
	return settled(files)
}

// mergeSeeds is every file of each list once, a whole view absorbing ranges of the same file.
func mergeSeeds(lists ...[]seedFile) []seedFile {
	var files []*seedFile
	for _, list := range lists {
		for _, f := range list {
			if f.whole || len(f.spans) == 0 {
				files = addView(files, f.path, nil)
			}
			for _, s := range f.spans {
				files = addView(files, f.path, []int{s.start, s.end})
			}
		}
	}
	return settled(files)
}

func viewedFiles(history []anthropic.MessageParam, dir string) []*seedFile {
	failed := map[string]bool{}
	for _, msg := range history {
		for _, block := range msg.Content {
			if r := block.OfToolResult; r != nil && r.IsError.Or(false) {
				failed[r.ToolUseID] = true
			}
		}
	}

	var files []*seedFile
	for _, msg := range history {
		if msg.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		for _, block := range msg.Content {
			use := block.OfToolUse
			if use == nil || use.Name != editorName || failed[use.ID] {
				continue
			}
			raw, err := json.Marshal(use.Input)
			if err != nil {
				continue
			}
			var in tools.EditInput
			if json.Unmarshal(raw, &in) != nil || in.Command != "view" || in.Path == "" {
				continue
			}
			path := in.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			files = addView(files, filepath.Clean(path), in.ViewRange)
		}
	}
	return files
}

// selectSeeds is the files viewed in history that the plan names; dir is where the viewer resolved relative paths.
func selectSeeds(history []anthropic.MessageParam, plan, root, dir string) []seedFile {
	files := slices.DeleteFunc(viewedFiles(history, dir), func(f *seedFile) bool { return !existingFile(f.path) })
	names := map[string]int{}
	for _, f := range files {
		names[filepath.Base(f.path)]++
	}
	files = slices.DeleteFunc(files, func(f *seedFile) bool {
		return !named(plan, f.path, root, names[filepath.Base(f.path)] == 1)
	})
	return settled(files)
}

func named(plan, abs, root string, unique bool) bool {
	if mentions(plan, abs) {
		return true
	}
	if root != "" {
		rel, err := filepath.Rel(root, abs)
		if err == nil && !strings.HasPrefix(rel, "..") && strings.ContainsRune(rel, filepath.Separator) && mentions(plan, rel) {
			return true
		}
	}
	return unique && mentions(plan, filepath.Base(abs))
}

// mentions is a substring match that refuses the tail of a longer path or the head of a longer name.
func mentions(text, s string) bool {
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], s)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(s)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (i == 0 || !(nameRune(before) || strings.ContainsRune("-./", before))) && (end == len(text) || !nameRune(after)) {
			return true
		}
		from = i + 1
	}
	return false
}

func nameRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func mergeSpans(spans []viewSpan) []viewSpan {
	slices.SortFunc(spans, func(a, b viewSpan) int { return cmp.Compare(a.start, b.start) })
	var out []viewSpan
	for _, s := range spans {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.end == -1 || s.start <= last.end+1 {
				if last.end != -1 && (s.end == -1 || s.end > last.end) {
					last.end = s.end
				}
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func offers(a *agent.Agent, name string) bool {
	if a.Tools == nil {
		return false
	}
	return slices.ContainsFunc(a.Tools(), func(t anthropic.ToolUnionParam) bool {
		got := t.GetName()
		return got != nil && *got == name
	})
}

func seededNote(n int, from string) string {
	if n == 1 {
		return "seeded 1 read from " + from
	}
	return "seeded " + strconv.Itoa(n) + " reads from " + from
}

func (p *peer) seedReads(ctx context.Context, driver *agent.Agent) ([]anthropic.MessageParam, []Seed) {
	p.mu.Lock()
	files := p.seeds
	p.mu.Unlock()
	return seedViews(ctx, driver, files)
}

// seedViews re-reads files through a's own editor as call and result pairs, and lists what it read.
func seedViews(ctx context.Context, a *agent.Agent, files []seedFile) ([]anthropic.MessageParam, []Seed) {
	if len(files) == 0 || !offers(a, editorName) {
		return nil, nil
	}

	var out []anthropic.MessageParam
	var read []Seed
	total := 0
	for _, f := range files {
		spans := f.spans
		if len(spans) == 0 {
			spans = []viewSpan{{}}
		}
		for _, s := range spans {
			in := tools.EditInput{Command: "view", Path: f.path}
			if s != (viewSpan{}) {
				in.ViewRange = []int{s.start, s.end}
			}
			raw, err := json.Marshal(in)
			if err != nil {
				continue
			}
			id := "toolu_seed_" + strconv.Itoa(len(out)/2+1)
			res := a.Run(ctx, agent.Call{ID: id, Name: editorName, Input: raw}, agent.Callbacks{})
			text, failed := callOutput(res)
			if failed || hasImage(res) || total+len(text) > seedBytes {
				continue
			}
			total += len(text)
			out = append(out,
				anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(id, json.RawMessage(raw), editorName)),
				anthropic.NewUserMessage(res))
			read = append(read, Seed{Path: in.Path, ViewRange: in.ViewRange})
		}
	}
	return out, read
}

func recordSeeds(t *tally, read []Seed) {
	t.set(func(rec *Dispatch) { rec.Seeds = read })
	if raw, err := json.Marshal(read); err == nil {
		t.transcript.add(TranscriptEvent{Kind: TranscriptSeed, Input: raw})
	}
}

// An image costs thousands of tokens that the text budget cannot see, so it is not replayed.
func hasImage(res anthropic.ContentBlockParamUnion) bool {
	return res.OfToolResult != nil && slices.ContainsFunc(res.OfToolResult.Content,
		func(c anthropic.ToolResultBlockParamContentUnion) bool { return c.OfImage != nil })
}
