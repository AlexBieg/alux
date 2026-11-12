package attach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/alexbieg/alux/internal/orchestrator"
	"github.com/alexbieg/alux/internal/prs"
	"github.com/alexbieg/alux/internal/reviewstore"
	"github.com/alexbieg/alux/internal/session"
	"github.com/alexbieg/alux/internal/ui"
)

// maxLine bounds one request; a finding brief with its patch is the largest thing a client sends.
const maxLine = 16 << 20

// Server serves one session to every TUI attached to it. One drives; a TUI attaching while another
// drives views until it takes the session, which leaves the old driver viewing.
type Server struct {
	session ui.SessionClient
	ln      *net.UnixListener
	// refuse rejects a peer before it is served; by default, any process this one started.
	refuse func(pid int) error
	// OnPane takes what the attached TUI reports of its pane. Set before Serve, like Land and Hello.
	OnPane func(Pane)
	// Land is the session's lander, nil outside a pool workspace; Hello.Land must say which.
	Land  ui.LandSource
	Hello Hello
	// PRs is the session's poller and PRStates its queue, nil unless Hello.PRs; the queue goes to each TUI on attach.
	PRs      ui.PRSource
	PRStates func() []prs.RepoState
	// Name looks up what an id in prose is called, waiting for the answer; nil leaves every id unnamed.
	Name func(id string) string
	// OnWatch takes Watched each time it flips, in order; it must not call back into the Server.
	OnWatch func(watched bool)

	watchMu sync.Mutex
	watched bool

	mu      sync.Mutex
	driver  *conn
	viewers map[*conn]bool
	closed  bool
	// guardian is the startup check's result once it has answered, nil before.
	guardian *guardianResult

	chatMu sync.Mutex
	chats  map[int]session.Chat
	chatID int
}

type conn struct {
	c     net.Conn
	wmu   sync.Mutex
	enc   *json.Encoder
	dirty chan struct{}
	done  chan struct{}
	once  sync.Once
	// sent is the Items the client holds, which the next snapshot is diffed against.
	sent []session.Item
}

// Listen opens the socket at path, replacing a stale one left by a process that died.
func Listen(path string, s ui.SessionClient) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{session: s, ln: ln, refuse: refuseSessionDescendants(filepath.Dir(path)), viewers: map[*conn]bool{}, chats: map[int]session.Chat{}}, nil
}

// Changed is the session's notifier: it marks every attached TUI's snapshot stale without blocking.
func (srv *Server) Changed() {
	for _, c := range srv.conns() {
		c.poke()
	}
}

func (srv *Server) conns() []*conn {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var all []*conn
	if srv.driver != nil {
		all = append(all, srv.driver)
	}
	for c := range srv.viewers {
		all = append(all, c)
	}
	return all
}

// Watched reports whether any TUI is attached, driving or viewing.
func (srv *Server) Watched() bool {
	return len(srv.conns()) > 0
}

func (srv *Server) noteWatch() {
	srv.watchMu.Lock()
	defer srv.watchMu.Unlock()
	now := srv.Watched()
	if now == srv.watched {
		return
	}
	srv.watched = now
	if srv.OnWatch != nil {
		srv.OnWatch(now)
	}
}

// Attached reports whether a TUI drives the session.
func (srv *Server) Attached() bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.driver != nil
}

// Notice shows text in the driving TUI. It and the other pushes are dropped when no TUI drives.
func (srv *Server) Notice(text string) { srv.toDriver(frame{Notice: text}) }

// PRsChanged hands every attached TUI the poller's new queue.
func (srv *Server) PRsChanged(states []prs.RepoState) {
	f := frame{PRs: toWire(states)}
	for _, c := range srv.conns() {
		if c.write(f) != nil {
			c.close()
		}
	}
}

// ReviewLoaded hands the driving TUI a review the poller stored; only one TUI may get it, since it raises a desktop notification.
func (srv *Server) ReviewLoaded(r reviewstore.Review) { srv.toDriver(frame{Review: &r}) }

// GuardianChecked records guardian's startup check, for the driving TUI and every one that attaches to drive.
func (srv *Server) GuardianChecked(err error) {
	g := &guardianResult{}
	if err != nil {
		g.Err = err.Error()
	}
	srv.mu.Lock()
	srv.guardian = g
	srv.mu.Unlock()
	srv.toDriver(frame{Guardian: g})
}

func (srv *Server) toDriver(f frame) {
	srv.mu.Lock()
	c := srv.driver
	srv.mu.Unlock()
	if c != nil && c.write(f) != nil {
		c.close()
	}
}

// Serve accepts until Close, handing the session to each new TUI in turn.
func (srv *Server) Serve() error {
	for {
		nc, err := srv.ln.AcceptUnix()
		if err != nil {
			srv.mu.Lock()
			closed := srv.closed
			srv.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		if err := srv.check(nc); err != nil {
			nc.Close()
			continue
		}
		srv.attach(nc)
	}
}

func (srv *Server) check(nc *net.UnixConn) error {
	pid, err := peerPID(nc)
	if err != nil {
		return err
	}
	return srv.refuse(pid)
}

// Close stops accepting, detaches every TUI and removes the socket.
func (srv *Server) Close() error {
	all := srv.conns()
	srv.mu.Lock()
	srv.closed = true
	srv.driver = nil
	clear(srv.viewers)
	srv.mu.Unlock()
	for _, c := range all {
		c.close()
	}
	path := srv.ln.Addr().String()
	err := srv.ln.Close()
	os.Remove(path)
	return err
}

func (srv *Server) attach(nc net.Conn) {
	c := &conn{c: nc, enc: json.NewEncoder(nc), dirty: make(chan struct{}, 1), done: make(chan struct{})}
	srv.mu.Lock()
	viewing := srv.driver != nil
	if viewing {
		srv.viewers[c] = true
	} else {
		srv.driver = c
	}
	guardian := srv.guardian
	srv.mu.Unlock()
	srv.noteWatch()
	hello := srv.Hello
	err := c.write(frame{Hello: &hello})
	if err == nil && srv.PRStates != nil {
		err = c.write(frame{PRs: toWire(srv.PRStates())})
	}
	if err == nil {
		err = c.write(frame{Viewing: &viewing})
	}
	if err == nil && !viewing && guardian != nil {
		err = c.write(frame{Guardian: guardian})
	}
	if err != nil {
		srv.detach(c)
		return
	}
	c.poke()
	go srv.push(c)
	go srv.read(c)
}

// detach closes c and, if it drove, leaves the session with no driver until a viewer takes it.
func (srv *Server) detach(c *conn) {
	c.close()
	srv.mu.Lock()
	current := srv.driver == c
	if current {
		srv.driver = nil
	}
	delete(srv.viewers, c)
	srv.mu.Unlock()
	srv.noteWatch()
	if current {
		srv.dropChats()
	}
}

// take makes c the driver and tells the old driver, which stays attached, that it now views.
func (srv *Server) take(c *conn) {
	srv.mu.Lock()
	old := srv.driver
	if old == c || srv.closed {
		srv.mu.Unlock()
		return
	}
	delete(srv.viewers, c)
	srv.driver = c
	if old != nil {
		srv.viewers[old] = true
	}
	srv.mu.Unlock()
	if old != nil {
		viewing := true
		if old.write(frame{Viewing: &viewing}) != nil {
			old.close()
		}
	}
	srv.dropChats()
}

func (srv *Server) drives(c *conn) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.driver == c
}

// dropChats forgets the finding chats, whose ids only the TUI that opened them held.
func (srv *Server) dropChats() {
	srv.chatMu.Lock()
	clear(srv.chats)
	srv.chatMu.Unlock()
}

func (c *conn) poke() {
	select {
	case c.dirty <- struct{}{}:
	default:
	}
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.c.Close()
	})
}

func (c *conn) write(f frame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(f)
}

// push sends a snapshot each time the session changes, coalescing changes that land while one is being written.
func (srv *Server) push(c *conn) {
	for {
		select {
		case <-c.done:
			return
		case <-c.dirty:
		}
		snap := srv.session.Snapshot()
		from := firstChanged(c.sent, snap.Items)
		c.sent = snap.Items
		snap.Items = snap.Items[from:]
		if err := c.write(frame{Snap: &snap, From: from}); err != nil {
			c.close()
			return
		}
	}
}

// firstChanged is the index from which cur differs from sent. The session replaces Items rather
// than editing it in place, so an unchanged item shares its strings and compares cheaply.
func firstChanged(sent, cur []session.Item) int {
	n := min(len(sent), len(cur))
	for i := range n {
		if !reflect.DeepEqual(sent[i], cur[i]) {
			return i
		}
	}
	return n
}

var slow = map[string]bool{
	opRecap: true, opChatSend: true,
	opLandList: true, opLandWorkspaces: true, opLandStat: true, opLandDiff: true,
	opLandPush: true, opLandRebase: true, opLandSweep: true,
	opPRDetail: true, opPRFiles: true, opPRReviewApp: true, opPRAddLabel: true,
	opPRMergeReady: true, opPRAddMergeLabel: true, opPRSync: true,
	opName: true,
}

func (srv *Server) read(c *conn) {
	defer srv.detach(c)
	sc := bufio.NewScanner(c.c)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		var r request
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			c.write(frame{Err: "bad request: " + err.Error()})
			return
		}
		if !viewerOK[r.Op] && !srv.drives(c) {
			c.write(frame{Reply: r.ID, Err: errViewing})
			continue
		}
		if r.Op == opTake {
			srv.take(c)
			driving := false
			c.write(frame{Viewing: &driving})
			c.write(frame{Reply: r.ID})
			continue
		}
		// A model call or a fetch can take minutes; everything else runs in arrival order.
		if slow[r.Op] {
			go srv.answer(c, r)
			continue
		}
		srv.answer(c, r)
	}
}

func (srv *Server) answer(c *conn, r request) {
	f := frame{Reply: r.ID}
	result, err := srv.do(r.Op, r.Args)
	if err != nil {
		f.Err = err.Error()
	} else if result != nil {
		f.Result, err = json.Marshal(result)
		if err != nil {
			f.Err = err.Error()
		}
	}
	if c.write(f) != nil {
		c.close()
	}
}

func (srv *Server) do(op string, a args) (any, error) {
	s := srv.session
	switch op {
	case opSubmit:
		s.Submit(a.Text)
	case opShow:
		s.Show(a.Text)
	case opSteer:
		s.Steer(a.ID)
	case opUnqueue:
		return s.Unqueue(a.ID), nil
	case opDropQueued:
		return s.DropQueued(), nil
	case opCancel:
		s.Cancel()
	case opSetMode:
		s.SetMode(a.Mode)
	case opRunSkill:
		s.RunSkill(a.Name, a.Inputs)
	case opApprove:
		return s.ApproveID(a.ID, a.Okay), nil
	case opAnswer:
		return s.AnswerID(a.ID, a.Answer), nil
	case opLiveProcs:
		return s.LiveProcs(), nil
	case opFileDiff:
		return s.FileDiff(a.Path)
	case opRecap:
		return nil, s.Recap(context.Background())
	case opPane:
		if a.Pane != nil && srv.OnPane != nil {
			srv.OnPane(*a.Pane)
		}
	case opChatOpen:
		if a.Brief == nil {
			return nil, errors.New("chatOpen needs a brief")
		}
		chat, err := s.FindingChat(a.Path, *a.Brief)
		if err != nil {
			return nil, err
		}
		srv.chatMu.Lock()
		defer srv.chatMu.Unlock()
		srv.chatID++
		srv.chats[srv.chatID] = chat
		return srv.chatID, nil
	case opChatSend:
		srv.chatMu.Lock()
		chat := srv.chats[a.ID]
		srv.chatMu.Unlock()
		if chat == nil {
			return nil, fmt.Errorf("no chat %d", a.ID)
		}
		reply, err := chat.Send(context.Background(), a.Text)
		if err != nil {
			return nil, err
		}
		return chatReply{Reply: reply, Model: chat.Model()}, nil
	case opLandList, opLandWorkspaces, opLandStat, opLandDiff, opLandPush, opLandRebase, opLandSweep, opLandCandidates:
		return srv.land(op, a)
	case opPRDetail, opPRFiles, opPRReviewApp, opPRAddLabel, opPRMergeReady, opPRAddMergeLabel, opPRSync:
		return srv.prs(op, a)
	case opName:
		if srv.Name == nil {
			return "", nil
		}
		return srv.Name(a.Text), nil
	default:
		return nil, fmt.Errorf("unknown op %q", op)
	}
	return nil, nil
}

// prs runs a poller call under the minute the picker allows one, since the TUI's own deadline does not cross the socket.
func (srv *Server) prs(op string, a args) (any, error) {
	p := srv.PRs
	if p == nil {
		return nil, errors.New("this session does not track pull requests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	switch op {
	case opPRDetail:
		return p.Detail(ctx, a.Repo, a.Number)
	case opPRFiles:
		return p.Files(ctx, a.Repo, a.Number)
	case opPRReviewApp:
		return p.ReviewApp(ctx, a.Repo, a.Number)
	case opPRAddLabel:
		return nil, p.AddLabel(ctx, a.Repo, a.Number, a.Label)
	case opPRMergeReady:
		return p.MergeReady(ctx, a.Repo, a.Number)
	case opPRAddMergeLabel:
		return p.AddMergeLabel(ctx, a.Repo, a.Number)
	default:
		p.Sync(ctx, a.Force)
		return nil, nil
	}
}

func (srv *Server) land(op string, a args) (any, error) {
	l := srv.Land
	if l == nil {
		return nil, errors.New("this session has no lander: it is not in a pool workspace")
	}
	switch op {
	case opLandList:
		return l.List()
	case opLandWorkspaces:
		return l.Workspaces()
	case opLandStat:
		return l.Stat(a.Change)
	case opLandDiff:
		return l.Diff(a.Change)
	case opLandPush:
		return l.Push(a.Workspace, a.Change)
	case opLandRebase:
		return l.Rebase(a.Workspace)
	case opLandCandidates:
		if cached, ok := l.(interface {
			Candidates() ([]orchestrator.Unlanded, error)
		}); ok {
			return cached.Candidates()
		}
		return l.List()
	default:
		said, held, err := l.Sweep()
		return sweepResult{Said: said, Held: held}, err
	}
}
