package prs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alexbieg/alux/internal/agent"
	"github.com/alexbieg/alux/internal/tools"
	"github.com/anthropics/anthropic-sdk-go"
)

const (
	createTool = "pr_create"
	editTool   = "pr_edit"
	pushTool   = "pr_push"

	maxAttachments = 50
	maxImageBytes  = 10 << 20
	maxVideoBytes  = 100 << 20
	bodyWordLimit  = 400
)

var (
	imageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg"}
	videoExts = []string{".mp4", ".mov", ".webm"}
	localRef  = regexp.MustCompile(`\]\((/[^)\s]+)\)`)
)

// Gh runs one gh command; on failure it still returns what gh printed to stdout.
type Gh func(ctx context.Context, args ...string) (string, error)

func GhRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	// Every call names --repo, so no checkout is needed, and a jj workspace without .git cannot confuse it.
	cmd.Dir = os.TempDir()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return string(out), fmt.Errorf("gh: %s", msg)
		}
		return string(out), fmt.Errorf("gh: %w", err)
	}
	return string(out), nil
}

// Jj runs one jj command in dir and returns what it printed.
type Jj func(ctx context.Context, dir string, args ...string) (string, error)

func JjRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", append([]string{"--no-pager", "--color=never"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return string(out), fmt.Errorf("jj: %s", msg)
		}
		return string(out), fmt.Errorf("jj: %w", err)
	}
	return string(out), nil
}

// Probe reports a video's codec and length in seconds.
type Probe func(ctx context.Context, path string) (codec string, seconds float64, err error)

// FFprobe is nil when ffprobe is not installed, which leaves the codec unchecked.
func FFprobe() Probe {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil
	}
	return func(ctx context.Context, path string) (string, float64, error) {
		out, err := exec.CommandContext(ctx, bin, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=codec_name:format=duration", "-of", "default=nw=1", path).Output()
		if err != nil {
			return "", 0, fmt.Errorf("ffprobe: %w", err)
		}
		var codec string
		var seconds float64
		for _, line := range strings.Split(string(out), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch k {
			case "codec_name":
				codec = v
			case "duration":
				seconds, _ = strconv.ParseFloat(v, 64)
			}
		}
		return codec, seconds, nil
	}
}

type attachment struct {
	path  string
	size  int64
	video bool
	note  string
}

// checkAttachments refuses, before anything uploads, every file GitHub would reject or a browser could not play.
func checkAttachments(ctx context.Context, probe Probe, paths []string) ([]attachment, error) {
	if len(paths) > maxAttachments {
		return nil, fmt.Errorf("%d attachments; gh takes at most %d per command", len(paths), maxAttachments)
	}
	var out []attachment
	var problems []string
	seen := map[string]bool{}
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		video := slices.Contains(videoExts, ext)
		info, err := os.Stat(p)
		switch {
		case !filepath.IsAbs(p):
			problems = append(problems, p+": give an absolute path")
			continue
		case strings.Contains(p, "#"):
			problems = append(problems, p+": gh reads what follows # as alt text; rename the file")
			continue
		case seen[p]:
			problems = append(problems, p+": attached twice")
			continue
		case !video && !slices.Contains(imageExts, ext):
			problems = append(problems, p+": only images ("+strings.Join(imageExts, " ")+") and videos ("+strings.Join(videoExts, " ")+") can be attached")
			continue
		case err != nil:
			problems = append(problems, err.Error())
			continue
		case !info.Mode().IsRegular():
			problems = append(problems, p+": not a regular file")
			continue
		}
		seen[p] = true
		limit := int64(maxImageBytes)
		if video {
			limit = maxVideoBytes
		}
		if info.Size() > limit {
			problems = append(problems, fmt.Sprintf("%s: %s, over GitHub's %s limit", p, megabytes(info.Size()), megabytes(limit)))
			continue
		}
		a := attachment{path: p, size: info.Size(), video: video}
		if video {
			if probe == nil {
				a.note = "codec not checked: ffprobe is not installed"
			} else if codec, seconds, err := probe(ctx, p); err != nil {
				problems = append(problems, p+": "+err.Error())
				continue
			} else if codec != "h264" {
				problems = append(problems, fmt.Sprintf("%s: video codec is %q, and only H.264 plays in every browser; re-encode with "+
					"ffmpeg -i IN -c:v libx264 -pix_fmt yuv420p -movflags +faststart OUT.mp4", p, codec))
				continue
			} else {
				a.note = fmt.Sprintf("H.264, %.0fs", seconds)
			}
		}
		out = append(out, a)
	}
	if len(problems) > 0 {
		return nil, errors.New("nothing was sent:\n" + strings.Join(problems, "\n"))
	}
	return out, nil
}

// unattachedRefs finds local files the body links to but the call does not upload, which would post as broken links.
func unattachedRefs(body string, paths []string) []string {
	var out []string
	for _, m := range localRef.FindAllStringSubmatch(body, -1) {
		p := m[1]
		if slices.Contains(paths, p) || slices.Contains(out, p) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func megabytes(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func writeBody(body string) (string, func(), error) {
	f, err := os.CreateTemp("", "alux-pr-body-*.md")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

func describeBody(b *strings.Builder, body string) {
	words := len(strings.Fields(body))
	note := ""
	if words > bodyWordLimit {
		note = fmt.Sprintf(", OVER the ~%d-word limit", bodyWordLimit)
	}
	fmt.Fprintf(b, "\nBody, %d words%s:\n%s\n", words, note, indent(body))
}

func describeUploads(b *strings.Builder, files []attachment) {
	if len(files) == 0 {
		return
	}
	b.WriteString("\nUploads to GitHub. An upload cannot be deleted afterwards except by GitHub Support, so check every frame for member data:\n")
	for _, a := range files {
		line := "- " + a.path + " · " + megabytes(a.size)
		if a.note != "" {
			line += " · " + a.note
		}
		b.WriteString(line + "\n")
	}
}

func attachArgs(files []attachment) []string {
	var args []string
	for _, a := range files {
		args = append(args, "--attach", a.path)
	}
	return args
}

func (p *Poller) authorTools() []anthropic.ToolUnionParam {
	attachments := map[string]any{
		"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxAttachments,
		"description": "Absolute paths of images (png jpg gif webp svg, up to 10 MB) and H.264 videos (mp4 mov, up to 100 MB) to upload. " +
			"Refer to each in the body by the same path, as ![alt](/tmp/shot.png), and gh swaps in the uploaded URL; a video " +
			"written as ![](/tmp/demo.mp4) alone in its paragraph renders as a player. Files the body does not mention are appended.",
	}
	repo := map[string]any{"type": "string", "description": "owner/name. Optional when only one repo is configured."}
	afterOpen := ""
	if p.conf.ReviewBot() {
		afterOpen = " After it opens, run the review bot on it with pr_review_bot_run unless the open skill run says otherwise."
	}
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name: createTool,
		Description: anthropic.String("Open one of the user's pull requests as a DRAFT, with screenshots and videos uploaded into " +
			"its description. This is the only way to open a PR; gh pr create in a shell is refused. It always opens a draft and " +
			"takes no labels: mark it ready, and add labels, later with pr_edit. The head bookmark must already be pushed. " +
			"A call with no files runs unasked; one with files stops for the user's approval, showing the title, the whole " +
			"body with its word count, and every file to upload. " +
			"Keep the body under about 400 words." + afterOpen),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"repo":        repo,
				"head":        map[string]any{"type": "string", "description": "The pushed branch (jj bookmark) holding the change."},
				"base":        map[string]any{"type": "string", "description": "The branch to merge into. Defaults to the repo's default branch."},
				"title":       map[string]any{"type": "string"},
				"body":        map[string]any{"type": "string", "description": "The description, as markdown."},
				"attachments": attachments,
			},
			Required:    []string{"head", "title", "body"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}, {OfTool: &anthropic.ToolParam{
		Name: editTool,
		Description: anthropic.String("Change one of the user's pull requests: its title, its description, uploaded screenshots " +
			"and videos, whether it is ready for review, and its labels. This is the only way to edit a PR or mark it ready; " +
			"gh pr edit and gh pr ready in a shell are refused. A new body replaces the whole description; attachments without " +
			"a body are appended to the description it has. A repo's ready label, when it has one, is refused while the PR " +
			"stays a draft: pass ready with it. Edits to an open PR run unasked, except marking it ready or adding the repo's " +
			"ready or merge label, which stop for the user's approval showing exactly what changes and every file to upload."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"number":      map[string]any{"type": "integer", "description": "The PR number."},
				"repo":        repo,
				"title":       map[string]any{"type": "string", "description": "The new title. Omit to keep it."},
				"body":        map[string]any{"type": "string", "description": "The new description, as markdown, replacing the old one. Omit to keep it."},
				"attachments": attachments,
				"ready":       map[string]any{"type": "boolean", "description": "Mark the draft ready for review."},
				"add_labels":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels to add."},
			},
			Required:    []string{"number"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}, {OfTool: &anthropic.ToolParam{
		Name: pushTool,
		Description: anthropic.String("Push a jj bookmark to GitHub as the user: moves the bookmark to rev, then pushes it. " +
			"This is the way a skill pushes a bookmark. A bookmark whose PR is merged, or the default branch, is refused. " +
			"A bookmark with no PR or only a draft one pushes unasked; one with a PR open for review stops for the user's approval."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"bookmark": map[string]any{"type": "string", "description": "The bookmark (branch) to push."},
				"rev":      map[string]any{"type": "string", "description": "jj revset naming the one change the bookmark moves to."},
				"repo":     repo,
			},
			Required:    []string{"bookmark", "rev"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}}}
}

func (p *Poller) runCreate(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Repo        string   `json:"repo"`
		Head        string   `json:"head"`
		Base        string   `json:"base"`
		Title       string   `json:"title"`
		Body        string   `json:"body"`
		Attachments []string `json:"attachments"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	in.Title, in.Head = strings.TrimSpace(in.Title), strings.TrimSpace(in.Head)
	if in.Title == "" || in.Head == "" || strings.TrimSpace(in.Body) == "" {
		return anthropic.NewToolResultBlock(call.ID, "a pull request needs a head, a title and a body", true)
	}
	owner, name, err := p.resolveRepo(in.Repo)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	files, err := p.checkUploads(ctx, in.Body, in.Attachments)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	slug := owner + "/" + name
	base := in.Base
	if base == "" {
		base = "the default branch"
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "Draft PR on %s: %s into %s\nTitle: %s\n", slug, in.Head, base, in.Title)
	describeBody(&preview, in.Body)
	describeUploads(&preview, files)
	label := "open draft PR: " + in.Title
	w := asks
	if len(files) == 0 {
		w = unasked
	}
	if res, ok := ask(ctx, cb, call, w, label, strings.TrimRight(preview.String(), "\n"), "it opens a pull request on GitHub as you"); !ok {
		return res
	}
	started(call, cb, label)
	bodyFile, cleanup, err := writeBody(in.Body)
	if err != nil {
		return finished(call, cb, label, "", err)
	}
	defer cleanup()
	args := []string{"pr", "create", "--repo", slug, "--draft", "--head", in.Head, "--title", in.Title, "--body-file", bodyFile}
	if in.Base != "" {
		args = append(args, "--base", in.Base)
	}
	out, err := p.gh(ctx, append(args, attachArgs(files)...)...)
	url := strings.TrimSpace(out)
	if err != nil {
		if url != "" {
			return finished(call, cb, label, "", fmt.Errorf("the draft opened at %s, but %w; check which attachments made it", url, err))
		}
		return finished(call, cb, label, "", err)
	}
	next := ""
	if agent.SkillWaived(ctx) {
		next = " The open skill run decides the next step."
	} else if r, _ := p.repoConfig(slug); r.ReviewBot != nil {
		next = " Run the review bot on it with pr_review_bot_run."
	}
	return finished(call, cb, label, "Opened draft PR "+url+"."+next, nil)
}

// addsAny reports whether labels holds any of the configured names, ignoring case and unset names.
func addsAny(labels []string, names ...string) bool {
	for _, l := range labels {
		for _, n := range names {
			if n != "" && strings.EqualFold(l, n) {
				return true
			}
		}
	}
	return false
}

func (p *Poller) runEdit(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		prTarget
		Title       *string  `json:"title"`
		Body        *string  `json:"body"`
		Attachments []string `json:"attachments"`
		Ready       bool     `json:"ready"`
		AddLabels   []string `json:"add_labels"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Title == nil && in.Body == nil && len(in.Attachments) == 0 && !in.Ready && len(in.AddLabels) == 0 {
		return anthropic.NewToolResultBlock(call.ID, "nothing to change: give a title, body, attachments, ready or add_labels", true)
	}
	owner, name, err := p.resolve(in.Repo, in.Number)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	slug := owner + "/" + name
	var pr struct {
		Title string `json:"title"`
		Draft bool   `json:"draft"`
		State string `json:"state"`
	}
	if err := sendJSON(ctx, p.send, "GET", fmt.Sprintf("repos/%s/pulls/%d", slug, in.Number), nil, &pr); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	ready := in.Ready && pr.Draft
	repoConf, _ := p.repoConfig(slug)
	readyLabel := repoConf.ReadyLabel
	for _, l := range in.AddLabels {
		if readyLabel != "" && strings.EqualFold(l, readyLabel) && pr.Draft && !ready {
			return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("%s#%d is a draft, and %s goes on only when it is marked ready: "+
				"pass ready: true with it, or leave the label for the user", slug, in.Number, readyLabel), true)
		}
	}
	body := ""
	if in.Body != nil {
		body = *in.Body
	}
	files, err := p.checkUploads(ctx, body, in.Attachments)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}

	state := "open"
	if pr.Draft {
		state = "draft"
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "%s#%d (%s): %s\n", slug, in.Number, state, pr.Title)
	if in.Title != nil {
		fmt.Fprintf(&preview, "\nNew title: %s\n", *in.Title)
	}
	if in.Body != nil {
		preview.WriteString("\nReplaces the whole description.")
		describeBody(&preview, body)
	} else if len(files) > 0 {
		preview.WriteString("\nAppends the uploads to the current description.\n")
	}
	describeUploads(&preview, files)
	if ready {
		preview.WriteString("\nMarks it ready for review, which notifies its reviewers.\n")
	}
	if len(in.AddLabels) > 0 {
		fmt.Fprintf(&preview, "\nAdds labels: %s\n", strings.Join(in.AddLabels, ", "))
	}
	label := fmt.Sprintf("edit PR #%d", in.Number)
	w := asks
	if pr.State == "open" && !ready && !addsAny(in.AddLabels, readyLabel, repoConf.MergeLabel) {
		w = unasked
	}
	if res, ok := ask(ctx, cb, call, w, label, strings.TrimRight(preview.String(), "\n"), "it changes a pull request on GitHub as you"); !ok {
		return res
	}
	started(call, cb, label)

	var done []string
	report := func(err error) anthropic.ContentBlockParamUnion {
		if len(done) > 0 {
			err = fmt.Errorf("%s\nThen it failed, so nothing after this was done: %w", strings.Join(done, "\n"), err)
		}
		return finished(call, cb, label, "", err)
	}
	if in.Title != nil || in.Body != nil || len(files) > 0 {
		args := []string{"pr", "edit", strconv.Itoa(in.Number), "--repo", slug}
		if in.Title != nil {
			args = append(args, "--title", *in.Title)
		}
		if in.Body != nil {
			bodyFile, cleanup, err := writeBody(body)
			if err != nil {
				return report(err)
			}
			defer cleanup()
			args = append(args, "--body-file", bodyFile)
		}
		if _, err := p.gh(ctx, append(args, attachArgs(files)...)...); err != nil {
			return report(err)
		}
		done = append(done, "Updated the title, description or attachments.")
	}
	if ready {
		if _, err := p.gh(ctx, "pr", "ready", strconv.Itoa(in.Number), "--repo", slug); err != nil {
			return report(err)
		}
		done = append(done, "Marked it ready for review.")
	} else if in.Ready {
		done = append(done, "It was already out of draft.")
	}
	if len(in.AddLabels) > 0 {
		if _, err := p.gh(ctx, "pr", "edit", strconv.Itoa(in.Number), "--repo", slug, "--add-label", strings.Join(in.AddLabels, ",")); err != nil {
			return report(err)
		}
		done = append(done, "Added labels "+strings.Join(in.AddLabels, ", ")+".")
	}
	return finished(call, cb, label, fmt.Sprintf("%s#%d:\n%s", slug, in.Number, strings.Join(done, "\n")), nil)
}

func (p *Poller) runPush(ctx context.Context, call agent.Call, cb agent.Callbacks) anthropic.ContentBlockParamUnion {
	var in struct {
		Repo     string `json:"repo"`
		Bookmark string `json:"bookmark"`
		Rev      string `json:"rev"`
	}
	if err := decodeStrict(call.Input, &in); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	in.Bookmark, in.Rev = strings.TrimSpace(in.Bookmark), strings.TrimSpace(in.Rev)
	if in.Bookmark == "" || in.Rev == "" || strings.HasPrefix(in.Bookmark, "-") || strings.HasPrefix(in.Rev, "-") {
		return anthropic.NewToolResultBlock(call.ID, "a push needs a bookmark and a rev, and neither may start with -", true)
	}
	owner, name, err := p.resolveRepo(in.Repo)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	slug := owner + "/" + name
	if in.Bookmark == "main" || in.Bookmark == "master" {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("%s is a trunk branch; pushing it is the user's to do", in.Bookmark), true)
	}
	dir := tools.Dir(ctx)
	remote, err := p.remoteFor(ctx, dir, slug)
	if err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := sendJSON(ctx, p.send, "GET", "repos/"+slug, nil, &info); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	if in.Bookmark == info.DefaultBranch {
		return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("%s is the default branch of %s; pushing it is the user's to do", in.Bookmark, slug), true)
	}
	var prs []struct {
		Number   int     `json:"number"`
		State    string  `json:"state"`
		Draft    bool    `json:"draft"`
		MergedAt *string `json:"merged_at"`
		HTMLURL  string  `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/pulls?head=%s:%s&state=all&per_page=100", slug, owner, url.QueryEscape(in.Bookmark))
	if err := sendJSON(ctx, p.send, "GET", path, nil, &prs); err != nil {
		return anthropic.NewToolResultBlock(call.ID, err.Error(), true)
	}
	var drafts, ready []string
	for _, pr := range prs {
		switch {
		case pr.MergedAt != nil:
			return anthropic.NewToolResultBlock(call.ID, fmt.Sprintf("%s is already merged (%s); pick a new bookmark", in.Bookmark, pr.HTMLURL), true)
		case pr.State == "open" && !pr.Draft:
			ready = append(ready, pr.HTMLURL)
		case pr.State == "open":
			drafts = append(drafts, pr.HTMLURL)
		}
	}
	var preview strings.Builder
	fmt.Fprintf(&preview, "Moves bookmark %s to %s and pushes it to %s (remote %s)\nIn %s\n", in.Bookmark, in.Rev, slug, remote, dir)
	if len(drafts) > 0 {
		fmt.Fprintf(&preview, "Updates the draft PR: %s\n", strings.Join(drafts, ", "))
	}
	if len(ready) > 0 {
		fmt.Fprintf(&preview, "Updates the PR open for review: %s\n", strings.Join(ready, ", "))
	}
	label := "push " + in.Bookmark
	w := asks
	if len(ready) == 0 {
		w = unasked
	}
	if res, ok := ask(ctx, cb, call, w, label, strings.TrimRight(preview.String(), "\n"), "it pushes to GitHub as you"); !ok {
		return res
	}
	started(call, cb, label)
	if _, err := p.jj(ctx, dir, "bookmark", "set", in.Bookmark, "-r", in.Rev); err != nil {
		return finished(call, cb, label, "", err)
	}
	if _, err := p.jj(ctx, dir, "git", "push", "--remote", remote, "--bookmark", in.Bookmark); err != nil {
		return finished(call, cb, label, "", fmt.Errorf("the bookmark moved locally, but the push failed: %w", err))
	}
	text := fmt.Sprintf("Pushed %s from %s to %s.", in.Bookmark, in.Rev, slug)
	if p.OnPush != nil {
		if change, err := p.jj(ctx, dir, "--ignore-working-copy", "log", "-r", in.Bookmark, "--no-graph", "-T", "change_id"); err == nil && strings.TrimSpace(change) != "" {
			if note := p.OnPush(strings.TrimSpace(change)); note != "" {
				text += " " + note
			}
		}
	}
	return finished(call, cb, label, text, nil)
}

// remoteFor names the jj git remote in dir whose URL is the GitHub repo slug.
func (p *Poller) remoteFor(ctx context.Context, dir, slug string) (string, error) {
	out, err := p.jj(ctx, dir, "git", "remote", "list")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		remote, rawURL, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if host, path := splitRemoteURL(strings.TrimSpace(rawURL)); host == "github.com" && strings.EqualFold(path, slug) {
			return remote, nil
		}
	}
	return "", fmt.Errorf("no jj git remote in %s points at %s, so nothing was pushed", dir, slug)
}

// splitRemoteURL reads the host and owner/name path from an https, ssh:// or scp-style git URL.
func splitRemoteURL(raw string) (host, path string) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", ""
		}
		host, path = u.Hostname(), u.Path
	} else {
		rest := raw
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		host, path, _ = strings.Cut(rest, ":")
	}
	return strings.ToLower(host), strings.TrimSuffix(strings.Trim(path, "/"), ".git")
}

func (p *Poller) checkUploads(ctx context.Context, body string, paths []string) ([]attachment, error) {
	if missing := unattachedRefs(body, paths); len(missing) > 0 {
		return nil, fmt.Errorf("the body links to local files that are not in attachments, which would post as broken links: %s",
			strings.Join(missing, ", "))
	}
	return checkAttachments(ctx, p.probe, paths)
}
