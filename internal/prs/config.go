// Package prs sorts the user's pull requests into lists by fixed rules that each repo's config selects.
package prs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

const defaultPoll = 4 * time.Minute

type Config struct {
	Poll  time.Duration
	Repos []Repo
}

type Repo struct {
	Owner, Name string
	// Team is "org/slug"; its members' approvals are what approved_by_team counts.
	Team   string
	Review Kind
	Mine   Kind
	Flags  []Flag
	// ReviewApp is nil when the repo has no review apps.
	ReviewApp *ReviewAppConfig
	// MergeLabel is the label that puts a PR in the merge queue; empty turns the picker's m off.
	MergeLabel string
	// ReadyLabel is refused on a draft PR unless the same edit marks it ready.
	ReadyLabel string
	// ReviewBot is nil when the repo runs no review bot.
	ReviewBot *ReviewBot

	fingerprint string
}

// ReviewAppConfig says how a repo's CI deploys a review app and where it reports it.
type ReviewAppConfig struct {
	Label       string `json:"label"`
	Bot         string `json:"bot"`
	HostSuffix  string `json:"host_suffix"`
	HealthCheck string `json:"health_check"`
	// Heading is text the bot's comment must contain; empty accepts any comment by Bot.
	Heading string `json:"heading"`
	// SlugApp is the app whose host is read first for the environment's slug.
	SlugApp string `json:"slug_app"`
	// AppsDir is the path prefix whose next segment names an app; empty turns Pick off.
	AppsDir string   `json:"apps_dir"`
	Skip    []string `json:"skip"`
	// Rank breaks ties between apps; its first entry is the fallback when no changed path names an app.
	Rank     []string `json:"rank"`
	LoginApp string   `json:"login_app"`
}

// ReviewBot is a bot that reviews a PR when the comment Trigger is posted, and opens its threads as Login.
type ReviewBot struct {
	Trigger string `json:"trigger"`
	Login   string `json:"login"`
}

// defaultReviewBot is what "bugbot": true means; Login is as GraphQL reports it, without REST's [bot] suffix.
var defaultReviewBot = ReviewBot{Trigger: "bugbot run", Login: "cursor"}

func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// ReviewBot reports whether any configured repo runs a review bot.
func (c Config) ReviewBot() bool {
	return slices.ContainsFunc(c.Repos, func(r Repo) bool { return r.ReviewBot != nil })
}

type Kind struct {
	Queries []Query
	Exclude []Exclude
}

// TitleMatches tightens GitHub's loose text search after the fetch.
type Query struct {
	Search       string
	TitleMatches *regexp.Regexp
}

// Exclude drops a PR when every condition it sets holds.
type Exclude struct {
	Draft          bool
	ApprovedByTeam bool
	// ApprovedUnlessRequested drops a PR any human has approved, unless the viewer is requested by name.
	ApprovedUnlessRequested bool
	Author                  string
	Label                   string
	UnlessLabel             string
	TitleMatches            *regexp.Regexp
	BodyMatches             *regexp.Regexp
}

type FlagKind string

const (
	FlagPendingReview FlagKind = "my_pending_review"
	FlagLabelOnRed    FlagKind = "label_on_red"
)

type Flag struct {
	Kind  FlagKind
	Label string
}

var (
	defaultReview = Kind{Queries: []Query{{Search: "review-requested:@me"}}, Exclude: []Exclude{{Draft: true}}}
	defaultMine   = Kind{Queries: []Query{{Search: "author:@me"}}}
	defaultFlags  = []Flag{{Kind: FlagPendingReview}}
)

// Load reads the "prs" block of settings.json; an unknown rule name is an error, not a no-op filter.
func Load() (Config, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, false, nil
	}
	data, err := os.ReadFile(filepath.Join(home, paths.Config, "settings.json"))
	if err != nil {
		return Config{}, false, nil
	}
	return Parse(data)
}

func Parse(settings []byte) (Config, bool, error) {
	var file struct {
		PRs *struct {
			Poll  string                     `json:"poll"`
			Repos map[string]json.RawMessage `json:"repos"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(settings, &file); err != nil {
		return Config{}, false, fmt.Errorf("prs: settings.json: %w", err)
	}
	if file.PRs == nil {
		return Config{}, false, nil
	}
	conf := Config{Poll: defaultPoll}
	if file.PRs.Poll != "" {
		d, err := time.ParseDuration(file.PRs.Poll)
		if err != nil || d < time.Minute {
			return Config{}, false, fmt.Errorf("prs: poll %q must be a duration of at least 1m", file.PRs.Poll)
		}
		conf.Poll = d
	}
	if len(file.PRs.Repos) == 0 {
		return Config{}, false, errors.New(`prs: no repos configured; add "repos": {"owner/name": {}}`)
	}
	for slug, raw := range file.PRs.Repos {
		repo, err := parseRepo(slug, raw)
		if err != nil {
			return Config{}, false, fmt.Errorf("prs: %s: %w", slug, err)
		}
		conf.Repos = append(conf.Repos, repo)
	}
	slices.SortFunc(conf.Repos, func(a, b Repo) int { return strings.Compare(a.Slug(), b.Slug()) })
	return conf, true, nil
}

func parseRepo(slug string, raw json.RawMessage) (Repo, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, errors.New(`repo key must be "owner/name"`)
	}
	var in struct {
		Team  string                     `json:"team"`
		Kinds map[string]json.RawMessage `json:"kinds"`
		Flags []string                   `json:"flags"`

		ReviewApp  json.RawMessage `json:"review_app"`
		MergeLabel string          `json:"merge_label"`
		ReadyLabel string          `json:"ready_label"`
		Bugbot     json.RawMessage `json:"bugbot"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Repo{}, err
	}
	sum := sha256.Sum256(append([]byte(slug+"\n"), raw...))
	repo := Repo{Owner: owner, Name: name, Team: in.Team, Review: defaultReview, Mine: defaultMine, Flags: defaultFlags,
		MergeLabel: in.MergeLabel, ReadyLabel: in.ReadyLabel, fingerprint: hex.EncodeToString(sum[:])}
	if in.ReviewApp != nil {
		app, err := parseReviewApp(in.ReviewApp)
		if err != nil {
			return Repo{}, fmt.Errorf("review_app: %w", err)
		}
		repo.ReviewApp = &app
	}
	bot, err := parseReviewBot(in.Bugbot)
	if err != nil {
		return Repo{}, fmt.Errorf("bugbot: %w", err)
	}
	repo.ReviewBot = bot
	if in.Team != "" {
		if org, team, ok := strings.Cut(in.Team, "/"); !ok || org == "" || team == "" {
			return Repo{}, fmt.Errorf(`team %q must be "org/slug"`, in.Team)
		}
	}
	for name, body := range in.Kinds {
		kind, err := parseKind(body)
		if err != nil {
			return Repo{}, fmt.Errorf("kinds.%s: %w", name, err)
		}
		switch name {
		case "review":
			repo.Review = kind
		case "mine":
			repo.Mine = kind
		default:
			return Repo{}, fmt.Errorf(`unknown kind %q; the kinds are "review" and "mine"`, name)
		}
	}
	if in.Flags != nil {
		repo.Flags = nil
		for _, f := range in.Flags {
			flag, err := parseFlag(f)
			if err != nil {
				return Repo{}, err
			}
			repo.Flags = append(repo.Flags, flag)
		}
	}
	if repo.Team == "" && usesTeam(repo.Review, repo.Mine) {
		return Repo{}, errors.New(`approved_by_team needs "team"`)
	}
	return repo, nil
}

func parseReviewApp(raw json.RawMessage) (ReviewAppConfig, error) {
	var app ReviewAppConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&app); err != nil {
		return ReviewAppConfig{}, err
	}
	if app.Label == "" || app.Bot == "" || app.HostSuffix == "" || app.HealthCheck == "" {
		return ReviewAppConfig{}, errors.New(`needs "label", "bot", "host_suffix" and "health_check"`)
	}
	app.HostSuffix = strings.ToLower(app.HostSuffix)
	if app.AppsDir != "" && !strings.HasSuffix(app.AppsDir, "/") {
		app.AppsDir += "/"
	}
	return app, nil
}

func parseReviewBot(raw json.RawMessage) (*ReviewBot, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		if !on {
			return nil, nil
		}
		bot := defaultReviewBot
		return &bot, nil
	}
	var bot ReviewBot
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bot); err != nil || bot.Trigger == "" || bot.Login == "" {
		return nil, errors.New(`must be true, false or {"trigger": "<comment>", "login": "<bot login>"}`)
	}
	return &bot, nil
}

func usesTeam(kinds ...Kind) bool {
	for _, k := range kinds {
		for _, e := range k.Exclude {
			if e.ApprovedByTeam {
				return true
			}
		}
	}
	return false
}

func parseKind(raw json.RawMessage) (Kind, error) {
	var in struct {
		Queries []json.RawMessage `json:"queries"`
		Exclude []json.RawMessage `json:"exclude"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Kind{}, err
	}
	if len(in.Queries) == 0 {
		return Kind{}, errors.New("needs at least one query")
	}
	var kind Kind
	for _, q := range in.Queries {
		query, err := parseQuery(q)
		if err != nil {
			return Kind{}, err
		}
		kind.Queries = append(kind.Queries, query)
	}
	for _, e := range in.Exclude {
		ex, err := parseExclude(e)
		if err != nil {
			return Kind{}, err
		}
		kind.Exclude = append(kind.Exclude, ex)
	}
	return kind, nil
}

func parseQuery(raw json.RawMessage) (Query, error) {
	var search string
	if json.Unmarshal(raw, &search) == nil {
		return Query{Search: search}, checkSearch(search)
	}
	var in struct {
		Search       string `json:"search"`
		TitleMatches string `json:"title_matches"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Query{}, fmt.Errorf("query: %w", err)
	}
	if err := checkSearch(in.Search); err != nil {
		return Query{}, err
	}
	re, err := compile("title_matches", in.TitleMatches)
	return Query{Search: in.Search, TitleMatches: re}, err
}

// checkSearch refuses qualifiers the fetcher sets, which would silently widen the scope.
func checkSearch(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("query is empty")
	}
	for _, field := range strings.Fields(s) {
		lower := strings.ToLower(strings.TrimPrefix(field, "-"))
		for _, owned := range []string{"repo:", "is:", "state:", "type:", "org:", "user:"} {
			if strings.HasPrefix(lower, owned) && lower != "is:draft" {
				return fmt.Errorf("query %q: %s is set by alux", s, owned)
			}
		}
	}
	return nil
}

func parseExclude(raw json.RawMessage) (Exclude, error) {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "draft":
			return Exclude{Draft: true}, nil
		case "approved_by_team":
			return Exclude{ApprovedByTeam: true}, nil
		case "approved_unless_requested":
			return Exclude{ApprovedUnlessRequested: true}, nil
		}
		return Exclude{}, fmt.Errorf(`unknown exclude %q; the named ones are "draft", "approved_by_team" and "approved_unless_requested"`, name)
	}
	var in struct {
		Author       string `json:"author"`
		Label        string `json:"label"`
		UnlessLabel  string `json:"unless_label"`
		TitleMatches string `json:"title_matches"`
		BodyMatches  string `json:"body_matches"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Exclude{}, fmt.Errorf("exclude: %w", err)
	}
	ex := Exclude{Author: in.Author, Label: in.Label, UnlessLabel: in.UnlessLabel}
	var err error
	if ex.TitleMatches, err = compile("title_matches", in.TitleMatches); err != nil {
		return Exclude{}, err
	}
	if ex.BodyMatches, err = compile("body_matches", in.BodyMatches); err != nil {
		return Exclude{}, err
	}
	if ex.Author == "" && ex.Label == "" && ex.TitleMatches == nil && ex.BodyMatches == nil {
		return Exclude{}, errors.New("exclude needs author, label, title_matches or body_matches; unless_label alone would drop everything")
	}
	return ex, nil
}

func parseFlag(s string) (Flag, error) {
	kind, arg, _ := strings.Cut(s, ":")
	switch FlagKind(kind) {
	case FlagPendingReview:
		if arg != "" {
			return Flag{}, fmt.Errorf("flag %q takes no argument", s)
		}
		return Flag{Kind: FlagPendingReview}, nil
	case FlagLabelOnRed:
		if arg == "" {
			return Flag{}, fmt.Errorf(`flag %q needs a label, as in "label_on_red:merge-queue"`, s)
		}
		return Flag{Kind: FlagLabelOnRed, Label: arg}, nil
	}
	return Flag{}, fmt.Errorf(`unknown flag %q; the flags are "my_pending_review" and "label_on_red:<label>"`, s)
}

func compile(field, pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return re, nil
}
