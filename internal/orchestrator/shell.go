package orchestrator

import (
	"fmt"
	"slices"

	"github.com/alexbieg/alux/internal/sandbox"
)

var sandboxOn = sandbox.Enabled

// shellArgv is the argv that runs command with sh -c, writing only under dir, under the sandbox unless settings turn it off.
func shellArgv(dir, command string, network bool) ([]string, error) {
	if !sandboxOn() {
		return []string{"sh", "-c", command}, nil
	}
	w, err := sandbox.Build(sandbox.Profile{Root: dir, Write: true, Network: network})
	if err != nil {
		return nil, fmt.Errorf("the sandbox could not be set up, so the command is not run: %w", err)
	}
	return append(slices.Clone(w.Args), "sh", "-c", command), nil
}
