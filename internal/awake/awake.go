// Package awake keeps the Mac from idle-sleeping while alux has work in flight, by running caffeinate -i for as long as any hold is counted.
package awake

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Floor is the battery charge below which no hold keeps the Mac awake off the charger.
const Floor = 20

// recheckEvery is how often the power source is read again while work is in flight.
const recheckEvery = time.Minute

// State is what the keeper is doing about sleep.
type State int

const (
	// Off is nothing in flight, so the Mac sleeps as it normally would.
	Off State = iota
	// Holding is caffeinate running.
	Holding
	// LowBattery is work in flight on a battery under Floor, so the Mac is let sleep anyway.
	LowBattery
)

// Power is a reading of where the Mac draws power from.
type Power struct {
	Battery bool
	Percent int
}

// Keeper counts the work in flight and holds one sleep assertion while the count is above zero. A nil Keeper does nothing.
type Keeper struct {
	// OnChange fires when State changes. It must not block.
	OnChange func()

	bin   string
	pid   int
	power func() (Power, error)
	floor int
	every time.Duration

	mu     sync.Mutex
	n      int
	closed bool
	last   Power
	state  State
	proc   *os.Process
	done   chan struct{}
	stop   chan struct{}
}

// New is the keeper for this process, or nil off darwin, where there is nothing to hold.
func New() *Keeper {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return &Keeper{bin: "/usr/bin/caffeinate", pid: os.Getpid(), power: readPower, floor: Floor, every: recheckEvery}
}

// CounterForTest is a keeper that counts holds and never runs caffeinate, for tests of what takes a hold.
func CounterForTest() *Keeper {
	return &Keeper{power: func() (Power, error) { return Power{}, nil }, every: time.Hour}
}

// Args is caffeinate's whole argument list: -i alone, so the display still sleeps and locks, and -w so a dead alux releases it.
func Args(pid int) []string { return []string{"-i", "-w", strconv.Itoa(pid)} }

// State is whether the keeper is holding the Mac awake right now.
func (k *Keeper) State() State {
	if k == nil {
		return Off
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state
}

// Count is how many holds are counted.
func (k *Keeper) Count() int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

// Hold counts one piece of work in flight until it is released.
func (k *Keeper) Hold() *Hold {
	h := &Hold{k: k}
	h.set(true)
	return h
}

// Close releases the assertion whatever the count, and for good.
func (k *Keeper) Close() {
	if k == nil {
		return
	}
	k.mu.Lock()
	k.n, k.closed = 0, true
	changed := k.applyLocked()
	k.mu.Unlock()
	k.announce(changed)
}

func (k *Keeper) add(d int) {
	if k == nil {
		return
	}
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	if k.n == 0 && d > 0 {
		k.readLocked()
	}
	k.n += d
	changed := k.applyLocked()
	k.mu.Unlock()
	k.announce(changed)
}

// readLocked reads the power source; a reading that fails counts as the charger, so work is never let sleep by mistake.
func (k *Keeper) readLocked() {
	p, err := k.power()
	if err != nil {
		p = Power{}
	}
	k.last = p
}

func (k *Keeper) recheck() {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.readLocked()
	changed := k.applyLocked()
	k.mu.Unlock()
	k.announce(changed)
}

func (k *Keeper) announce(changed bool) {
	if changed && k.OnChange != nil {
		k.OnChange()
	}
}

// applyLocked brings caffeinate and the recheck loop in line with the count and the battery, reporting whether State changed.
func (k *Keeper) applyLocked() bool {
	was := k.state
	want := Off
	if k.n > 0 {
		want = Holding
		if k.last.Battery && k.last.Percent < k.floor {
			want = LowBattery
		}
	}
	if want == Holding && k.proc == nil {
		k.startLocked()
	}
	if want != Holding && k.proc != nil {
		k.killLocked()
	}
	k.state = Off
	switch {
	case want == Holding && k.proc != nil:
		k.state = Holding
	case want == LowBattery:
		k.state = LowBattery
	}
	if k.n > 0 && k.stop == nil {
		k.stop = make(chan struct{})
		go k.watch(k.stop)
	}
	if k.n == 0 && k.stop != nil {
		close(k.stop)
		k.stop = nil
	}
	return k.state != was
}

func (k *Keeper) startLocked() {
	if k.bin == "" {
		return
	}
	cmd := exec.Command(k.bin, Args(k.pid)...)
	if err := cmd.Start(); err != nil {
		return
	}
	done := make(chan struct{})
	k.proc, k.done = cmd.Process, done
	go func() {
		_ = cmd.Wait()
		close(done)
		k.mu.Lock()
		if k.done != done {
			k.mu.Unlock()
			return
		}
		k.proc, k.done = nil, nil
		changed := k.state == Holding
		if changed {
			k.state = Off
		}
		k.mu.Unlock()
		k.announce(changed)
	}()
}

func (k *Keeper) killLocked() {
	_ = k.proc.Kill()
	<-k.done
	k.proc, k.done = nil, nil
}

// watch rereads the battery while work is in flight, and restarts a caffeinate that died.
func (k *Keeper) watch(stop chan struct{}) {
	t := time.NewTicker(k.every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			k.recheck()
		}
	}
}

// Hold is one piece of work in flight. Its methods are safe to call in any order and more than once.
type Hold struct {
	k        *Keeper
	mu       sync.Mutex
	counted  bool
	released bool
	waits    int
}

// Release stops counting the work, for good.
func (h *Hold) Release() {
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
	h.set(false)
}

// Pause stops counting the work while it waits on the user; Resume counts it again.
func (h *Hold) Pause() {
	h.mu.Lock()
	h.waits++
	h.mu.Unlock()
	h.set(false)
}

// Resume undoes one Pause.
func (h *Hold) Resume() {
	h.mu.Lock()
	h.waits = max(h.waits-1, 0)
	want := !h.released && h.waits == 0
	h.mu.Unlock()
	h.set(want)
}

func (h *Hold) set(want bool) {
	h.mu.Lock()
	if want && (h.released || h.waits > 0) {
		want = false
	}
	if want == h.counted {
		h.mu.Unlock()
		return
	}
	h.counted = want
	h.mu.Unlock()
	if want {
		h.k.add(1)
	} else {
		h.k.add(-1)
	}
}

var percent = regexp.MustCompile(`(\d+)%`)

func readPower() (Power, error) {
	out, err := exec.Command("/usr/bin/pmset", "-g", "batt").Output()
	if err != nil {
		return Power{}, err
	}
	return parsePower(string(out)), nil
}

// parsePower reads pmset -g batt; a Mac with no battery reads as on the charger.
func parsePower(out string) Power {
	if !strings.Contains(out, "'Battery Power'") {
		return Power{}
	}
	p := Power{Battery: true, Percent: 100}
	if m := percent.FindStringSubmatch(out); m != nil {
		p.Percent, _ = strconv.Atoi(m[1])
	}
	return p
}
