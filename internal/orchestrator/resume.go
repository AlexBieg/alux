package orchestrator

import (
	"context"
	"fmt"
	"strings"
)

// Resume moves own's session onto a new change on top of change, so work an ended session left behind carries on here.
// It refuses work a live session holds, work already under default's @ that every new session starts from,
// and a session that has work of its own, which the move would leave off its stack.
func (p *Pool) Resume(ctx context.Context, own *Slot, change string) (string, error) {
	if own == nil {
		return "", fmt.Errorf("this session is not in a pool workspace")
	}
	at := "change_id(" + change + ")"
	info, err := jjStdout(ctx, own.Dir, "log", "-r", at, "--no-graph", "-T",
		`change_id.shortest(8) ++ "\t" ++ if(immutable, "immutable") ++ "\t" ++ if(empty, "empty") ++ "\t" ++ if(self.contained_in("::default@"), "default") ++ "\t" ++ if(self.contained_in("::@"), "here") ++ "\t" ++ description.first_line()`)
	if err != nil {
		return "", fmt.Errorf("%q is not a change in this repo: %w", change, err)
	}
	f := append(strings.Split(info, "\t"), "", "", "", "", "", "")[:6]
	id, desc := f[0], f[5]
	switch {
	case f[4] != "":
		return "", fmt.Errorf("%s is already in this session's workspace", id)
	case f[1] != "":
		return "", fmt.Errorf("%s is immutable, so there is nothing to carry on", id)
	case f[3] != "":
		return "", fmt.Errorf("%s is already under the default workspace's @, which every new session starts from", id)
	case f[2] != "" && desc == "":
		return "", fmt.Errorf("%s is empty and undescribed; there is no work to resume", id)
	}
	if holder, err := p.liveHolder(ctx, own, at); err != nil {
		return "", err
	} else if holder != "" {
		return "", fmt.Errorf("%s is live work in %s; it can be resumed once that session ends", id, holder)
	}
	mine, err := p.slotRev(own.Name)
	if err != nil {
		return "", err
	}
	if n, err := count(ctx, own.Dir, "("+mine+") ~ empty()"); err != nil {
		return "", err
	} else if n > 0 {
		return "", fmt.Errorf("this session has work of its own, which moving would leave off its stack; resume %s from a fresh session (/new)", id)
	}

	left, err := jjStdout(ctx, own.Dir, "log", "-r", "@", "--no-graph", "-T", "change_id")
	if err != nil {
		return "", err
	}
	if _, err := jj(ctx, own.Dir, "new", at); err != nil {
		return "", err
	}
	if _, err := jj(ctx, own.Dir, "abandon", "present(change_id("+left+")) & empty() & ~working_copies()"); err != nil {
		return "", err
	}
	return fmt.Sprintf("This session now carries on from %s (%s) in a new change on top of it, which needs jj describe -m before the next edit.", id, desc), nil
}

// liveHolder is the slot other than own whose running session holds rev in its stack, empty when none does.
func (p *Pool) liveHolder(ctx context.Context, own *Slot, rev string) (string, error) {
	for _, n := range p.numbers() {
		name := fmt.Sprintf("pool-%d", n)
		if name == own.Name {
			continue
		}
		stack, err := p.slotRev(name)
		if err != nil {
			return "", err
		}
		if c, err := count(ctx, p.Origin, rev+" & ::("+stack+")"); err != nil || c == 0 {
			if err != nil {
				return "", err
			}
			continue
		}
		s, free, err := p.lock(n)
		if err != nil {
			return "", err
		}
		if !free {
			return name, nil
		}
		s.unlock()
	}
	return "", nil
}

// Resume is /resume and the pane's s: carry on from change in this session's workspace.
func (l *Lander) Resume(change string) (string, error) {
	return l.Pool.Resume(context.Background(), l.Slot, strings.TrimSpace(change))
}
