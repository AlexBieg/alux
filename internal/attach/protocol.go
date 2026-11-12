// Package attach serves one session over a unix socket to the TUIs attached to it: one drives, the rest view.
// The stream is a snapshot on attach and then whole snapshots whose Items carry only what changed.
package attach

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexbieg/alux/internal/ask"
	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/paths"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/subagent"
	"github.com/alexbieg/alux/internal/ui"
	"github.com/alexbieg/alux/internal/vcs"
)

const (
	opSubmit     = "submit"
	opShow       = "show"
	opSteer      = "steer"
	opUnqueue    = "unqueue"
	opDropQueued = "dropQueued"
	opCancel     = "cancel"
	opSetMode    = "setMode"
	opRunSkill   = "runSkill"
	opApprove    = "approve"
	opAnswer     = "answer"
	opLiveProcs  = "liveProcs"
	opFileDiff   = "fileDiff"
	opRecap      = "recap"
	opChatOpen   = "chatOpen"
	opChatSend   = "chatSend"
	opPane       = "pane"

	opLandList       = "landList"
	opLandWorkspaces = "landWorkspaces"
	opLandStat       = "landStat"
	opLandDiff       = "landDiff"
	opLandPush       = "landPush"
	opLandRebase     = "landRebase"
	opLandSweep      = "landSweep"
	// opLandCandidates is landList through the lander's short cache, for completion on the TUI's event loop.
	opLandCandidates = "landCandidates"

	opPRDetail        = "prDetail"
	opPRFiles         = "prFiles"
	opPRReviewApp     = "prReviewApp"
	opPRAddLabel      = "prAddLabel"
	opPRMergeReady    = "prMergeReady"
	opPRAddMergeLabel = "prAddMergeLabel"
	opPRSync          = "prSync"

	opName = "name"

	// opTake makes the caller the session's driver.
	opTake = "take"
)

// errViewing is the refusal of a driver-only op, which the client matches to take and retry.
const errViewing = "this pane is viewing; another pane drives the session"

// viewerOK is every op a viewing TUI may make; any other changes the session and needs the driver.
var viewerOK = map[string]bool{
	opLiveProcs: true, opFileDiff: true,
	opLandList: true, opLandWorkspaces: true, opLandStat: true, opLandDiff: true, opLandCandidates: true,
	opPRDetail: true, opPRFiles: true, opPRReviewApp: true, opPRMergeReady: true, opPRSync: true,
	opName: true, opTake: true,
}

// Hello is what the session says once on attach, before its first snapshot: what never changes while it runs.
type Hello struct {
	// Land is whether the session has a lander, which only a pool workspace does.
	Land     bool      `json:"land"`
	PRFlow   bool      `json:"prFlow"`
	VCSScope vcs.Scope `json:"vcsScope"`
	VCSSlug  string    `json:"vcsSlug"`
	// PRs is whether the session tracks pull requests.
	PRs   bool      `json:"prs"`
	Setup *ui.Setup `json:"setup,omitempty"`
}

// guardianResult is guardian's startup check, Err empty when it is ready.
type guardianResult struct {
	Err string `json:"err"`
}

// prStates carries the poller's queue, which may be empty, as against absent.
type prStates struct {
	States []repoState `json:"states"`
}

// repoState is a RepoState whose failed poll survives JSON, which encodes an error as {}. Err hides
// the embedded one only while both share the JSON name Err.
type repoState struct {
	prs.RepoState
	Err string `json:"Err,omitempty"`
}

func toWire(states []prs.RepoState) *prStates {
	out := &prStates{States: make([]repoState, len(states))}
	for i, s := range states {
		out.States[i].RepoState = s
		if s.Err != nil {
			out.States[i].Err = s.Err.Error()
		}
	}
	return out
}

func (w prStates) states() []prs.RepoState {
	out := make([]prs.RepoState, len(w.States))
	for i, s := range w.States {
		out[i] = s.RepoState
		if s.Err != "" {
			out[i].Err = errors.New(s.Err)
		}
	}
	return out
}

type sweepResult struct {
	Said []string                     `json:"said"`
	Held map[string]orchestrator.Held `json:"held"`
}

// request is one line from the client. Every op is answered with a frame whose Reply is ID.
type request struct {
	ID   int    `json:"id"`
	Op   string `json:"op"`
	Args args   `json:"args"`
}

// args holds every op's arguments; each op reads only its own.
type args struct {
	Text      string                 `json:"text,omitempty"`
	ID        int                    `json:"id,omitempty"`
	Mode      session.Mode           `json:"mode,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Inputs    map[string]any         `json:"inputs,omitempty"`
	Okay      bool                   `json:"okay,omitempty"`
	Answer    *ask.Answer            `json:"answer,omitempty"`
	Path      string                 `json:"path,omitempty"`
	Brief     *subagent.FindingBrief `json:"brief,omitempty"`
	Pane      *Pane                  `json:"pane,omitempty"`
	Change    string                 `json:"change,omitempty"`
	Workspace string                 `json:"workspace,omitempty"`
	Repo      string                 `json:"repo,omitempty"`
	Number    int                    `json:"number,omitempty"`
	Label     string                 `json:"label,omitempty"`
	Force     bool                   `json:"force,omitempty"`
}

// Pane is what only the attached TUI knows of its session's registry entry.
type Pane struct {
	Unseen   bool   `json:"unseen"`
	Bookmark string `json:"bookmark"`
	Change   string `json:"change"`
}

// frame is one line from the server: a reply, a snapshot, or a push such as the TUI's role.
type frame struct {
	Reply  int             `json:"reply,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Err    string          `json:"err,omitempty"`

	// Snap's Items replace the client's from index From on.
	Snap *session.Snapshot `json:"snap,omitempty"`
	From int               `json:"from,omitempty"`

	// Viewing is the TUI's role when set: true while another TUI drives.
	Viewing *bool  `json:"viewing,omitempty"`
	Notice  string `json:"notice,omitempty"`
	Hello   *Hello `json:"hello,omitempty"`

	PRs      *prStates           `json:"prs,omitempty"`
	Review   *reviewstore.Review `json:"review,omitempty"`
	Guardian *guardianResult     `json:"guardian,omitempty"`
}

type chatReply struct {
	Reply string `json:"reply"`
	Model string `json:"model"`
}

// SocketPath is where the session in process pid listens. It sits in the registry's directory,
// which the sandbox denies writes to and the guardian holds every command naming for approval.
func SocketPath(pid int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.Cache, "sessions", fmt.Sprintf("%d.sock", pid)), nil
}
