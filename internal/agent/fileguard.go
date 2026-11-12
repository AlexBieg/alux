package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexbieg/alux/internal/llm"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/sandbox"
	"github.com/alexbieg/alux/internal/tools"
)

// readDenied is what no file tool may read, relative to the home directory: keys, the record store and saved credentials.
var readDenied = []string{".ssh", paths.Share, llm.AuthFile, paths.Config + "/mcp-auth"}

// skillRuns holds each skill run's files, which sit inside State and stay open to the file tools.
const skillRuns = paths.State + "/skills"

// settingsFile and skillsDir are all under Config an edit may write, and only once the user says yes to it.
const (
	settingsFile = paths.Config + "/settings.json"
	skillsDir    = paths.Config + "/skills"
)

// userConfig names what path is when its edit must go through Require: alux settings or an alux skill.
func userConfig(path string) (string, bool) {
	target, err := realPath(path)
	if err != nil {
		return "", false
	}
	home, err := realHome()
	switch {
	case err != nil:
		return "", false
	case strings.EqualFold(target, filepath.Join(home, settingsFile)):
		return "alux settings", true
	case under(target, filepath.Join(home, skillsDir)) && !strings.EqualFold(target, filepath.Join(home, skillsDir)):
		return "an alux skill", true
	}
	return "", false
}

// checkPath is the check every file tool makes, for every agent, before it reads or writes path.
func checkPath(ctx context.Context, path string, write bool) error {
	target, err := realPath(path)
	if err != nil {
		return err
	}
	home, err := realHome()
	if err != nil {
		return err
	}
	open := under(target, filepath.Join(home, skillRuns)) || under(target, filepath.Join(home, paths.JobsOut))
	_, config := userConfig(path)
	if !open && !(write && config) {
		denied := readDenied
		if write {
			denied = sandbox.Protected
		}
		for _, name := range denied {
			if root := filepath.Join(home, name); under(target, root) {
				return refusal(path, root, write)
			}
		}
	}
	if write && Guard != nil {
		return Guard.Check(ctx, path)
	}
	return nil
}

// checkSearch refuses a grep or glob rooted at, or containing, a location no file tool may read.
func checkSearch(ctx context.Context, path string) error {
	if path == "" {
		path = tools.Dir(ctx)
	}
	if err := checkPath(ctx, inDir(ctx, path), false); err != nil {
		return err
	}
	target, err := realPath(inDir(ctx, path))
	if err != nil {
		return err
	}
	home, err := realHome()
	if err != nil {
		return err
	}
	for _, name := range readDenied {
		if root := filepath.Join(home, name); under(root, target) {
			return fmt.Errorf("searching %s would descend into %s, which holds keys or credentials; search a narrower path", path, root)
		}
	}
	return nil
}

func realHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return realPath(home)
}

func refusal(path, root string, write bool) error {
	if write {
		return fmt.Errorf("%s is under %s, which no agent may write to", path, root)
	}
	return fmt.Errorf("%s is under %s, which holds keys or credentials and no agent may read", path, root)
}
