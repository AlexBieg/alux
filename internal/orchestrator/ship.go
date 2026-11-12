package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Ship is where one change stands on its way into trunk through a pull request; zero outside the PR flow.
type Ship struct {
	// OnTrunk is a change already in trunk's history.
	OnTrunk bool
	// Pushed is a change some bookmark on the remote already carries.
	Pushed bool
	// Bookmark is the change's local bookmark, empty for none; BookmarkConflict is one that moved both here and on the remote.
	Bookmark         string
	BookmarkConflict bool
	// NotOnRemote is a bookmark the remote has never had.
	NotOnRemote bool
	// Ahead counts the commits under the bookmark that the remote's copy lacks, rewritten ones included.
	Ahead int
	// Moved is a bookmark that left this change for MovedBy commits on top of it that the stack does not hold, as a fetch of someone else's push does.
	Moved   string
	MovedBy int
}

// Remote is the remote trunk is read from: the part after @ in a plain trunk such as main@origin.
func (p *Prepare) Remote() string {
	t := p.TrunkRev()
	if i := strings.LastIndex(t, "@"); i > 0 && !strings.ContainsAny(t, " ()|&~:") {
		return t[i+1:]
	}
	return "origin"
}

// ship fills in where each of w's changes, read by rev, stands against the remote and trunk.
func (p *Pool) ship(ctx context.Context, rev string, w *WorkspaceView, onRemote map[string]bool) error {
	if len(w.Changes) == 0 {
		return nil
	}
	trunk, remote := "present("+p.Prepare.TrunkRev()+")", jjString(p.Prepare.Remote())
	out, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T",
		`change_id.shortest(8) ++ "\t" ++ if(self.contained_in(`+jjString("::"+trunk)+`), "trunk") ++ "\t" ++ if(self.contained_in(`+jjString("::remote_bookmarks(remote=exact:"+remote+")")+`), "pushed") ++ "\t" ++ local_bookmarks.map(|b| b.name() ++ if(b.conflict(), "?")).join(",") ++ "\t" ++ remote_bookmarks.filter(|b| b.remote() == `+remote+`).map(|b| b.name()).join(",") ++ "\n"`)
	if err != nil {
		return err
	}
	at := map[string]int{}
	for i, c := range w.Changes {
		at[c.Change] = i
	}
	for _, line := range lines(out) {
		// jjStdout trims the output, which takes the last row's empty trailing fields with it.
		f := append(strings.Split(line, "\t"), "", "", "", "")[:5]
		i, ok := at[f[0]]
		if !ok {
			continue
		}
		s := &w.Changes[i].Ship
		s.OnTrunk, s.Pushed = f[1] != "", f[2] != ""
		if f[3] == "" {
			continue
		}
		s.Bookmark, s.BookmarkConflict = strings.CutSuffix(strings.Split(f[3], ",")[0], "?")
		switch {
		case s.BookmarkConflict:
		case !onRemote[s.Bookmark]:
			s.NotOnRemote = true
		case !slices.Contains(strings.Split(f[4], ","), s.Bookmark):
			b := jjString(s.Bookmark)
			if s.Ahead, err = count(ctx, p.Origin, "(remote_bookmarks(exact:"+b+", remote=exact:"+remote+")..bookmarks(exact:"+b+")) ~ ::"+trunk); err != nil {
				return err
			}
		}
	}
	// Every slot is built on the default card's commits, so only a slot's own stack can have a bookmark move off it.
	if w.Name != "default" {
		if err := p.moved(ctx, rev, trunk, w); err != nil {
			return err
		}
	}
	w.Behind, err = count(ctx, p.Origin, "change_id("+w.Changes[0].Change+").."+trunk)
	return err
}

// moved finds bookmarks on commits built on top of the stack but outside it, and marks the stack change each was built on.
func (p *Pool) moved(ctx context.Context, rev, trunk string, w *WorkspaceView) error {
	out, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", "(("+rev+"):: ~ ("+rev+")) & bookmarks()", "--no-graph", "-T",
		`commit_id ++ "\t" ++ local_bookmarks.map(|b| b.name()).join(",") ++ "\n"`)
	if err != nil {
		return err
	}
	for _, line := range lines(out) {
		commit, names, ok := strings.Cut(line, "\t")
		if !ok || names == "" {
			continue
		}
		onto, err := jjStdout(ctx, p.Origin, "--ignore-working-copy", "log", "-r", "heads(::"+commit+" & ("+rev+"))", "--no-graph", "-T", `change_id.shortest(8) ++ "\n"`)
		if err != nil {
			return err
		}
		onto, _, _ = strings.Cut(onto, "\n")
		i := slices.IndexFunc(w.Changes, func(c WorkspaceChange) bool { return c.Change == onto })
		if i < 0 {
			continue
		}
		n, err := count(ctx, p.Origin, "(::"+commit+" ~ ::("+rev+")) ~ ::"+trunk)
		if err != nil {
			return err
		}
		w.Changes[i].Ship.Moved, w.Changes[i].Ship.MovedBy = strings.Split(names, ",")[0], n
	}
	return nil
}

var githubURL = regexp.MustCompile(`github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)

// githubSlug is owner/name of the GitHub repo remote points at, empty when it points elsewhere.
func githubSlug(ctx context.Context, dir, remote string) (string, error) {
	out, err := jjStdout(ctx, dir, "--ignore-working-copy", "git", "remote", "list")
	if err != nil {
		return "", err
	}
	for _, line := range lines(out) {
		name, url, _ := strings.Cut(line, " ")
		if m := githubURL.FindStringSubmatch(strings.TrimSpace(url)); name == remote && m != nil {
			return m[1] + "/" + m[2], nil
		}
	}
	return "", nil
}

// remoteBookmarks is every bookmark name the remote has.
func remoteBookmarks(ctx context.Context, dir, remote string) (map[string]bool, error) {
	out, err := jjStdout(ctx, dir, "--ignore-working-copy", "bookmark", "list", "--all-remotes", "-T", `if(remote == `+jjString(remote)+`, name ++ "\n")`)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, n := range lines(out) {
		names[n] = true
	}
	return names, nil
}

// count is how many commits rev holds.
func count(ctx context.Context, dir, rev string) (int, error) {
	out, err := jjStdout(ctx, dir, "--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", `"+"`)
	return len(out), err
}

// jjString quotes s as a jj string literal.
func jjString(s string) string { return fmt.Sprintf("%q", s) }
