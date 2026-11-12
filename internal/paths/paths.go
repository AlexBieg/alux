// Package paths names alux's directories under the home directory, relative to it.
package paths

const Name = "alux"

const (
	Config = ".config/" + Name
	Cache  = ".cache/" + Name
	State  = ".local/state/" + Name
	Share  = ".local/share/" + Name
	// Workspaces sits beside State, not in it, because the sandbox denies writes to State and agents write in its workspaces.
	Workspaces = ".local/state/" + Name + "-workspaces"
	// JobsOut holds what scheduled jobs leave for the user to read: the only place a job may write.
	JobsOut = State + "/jobs/out"
)
