package service

import (
	"strings"
	"sync"
	"time"
)

// What a pass of the scheduler, or a forced chain, has done, does and will do.
//
// The scheduler backs up one app after another, so a long copy holds up every slot behind it.
// On the owner's box thirteen apps and the box itself waited behind one for a night, and
// nothing said so. A pass is kept after it ends until the next of its origin begins.

// PassItemState is where one couple of a pass stands.
type PassItemState string

const (
	PassWaiting PassItemState = "waiting"
	PassRunning PassItemState = "running"
	PassDone    PassItemState = "done"
	PassFailed  PassItemState = "failed"
	PassStopped PassItemState = "stopped"
	// PassHeld is a couple left for later because an operating system upgrade is open.
	PassHeld PassItemState = "held"
)

// BackupPassItem is one app to one destination in a pass.
type BackupPassItem struct {
	App         string
	Destination string
	State       PassItemState
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Bytes       int64
	Error       string
	// dropped is a couple a force runs instead: kept so the pass's indexes stay, never shown
	dropped bool
}

// BackupPass is one pass of the scheduler, or one forced chain.
type BackupPass struct {
	Origin     BackupOrigin
	StartedAt  time.Time
	FinishedAt *time.Time
	Items      []BackupPassItem
}

type backupPass struct {
	BackupPass
}

var passes = struct {
	sync.Mutex
	scheduled, forced *backupPass
}{}

// startPass begins a pass of these couples, all waiting, in place of the last of its origin.
func startPass(origin BackupOrigin, couples []BackupSchedule, now time.Time) *backupPass {
	pass := &backupPass{BackupPass{Origin: origin, StartedAt: now, Items: make([]BackupPassItem, 0, len(couples))}}
	for _, couple := range couples {
		pass.Items = append(pass.Items, BackupPassItem{App: couple.App, Destination: couple.Destination, State: PassWaiting})
	}

	passes.Lock()
	if origin == OriginForced {
		passes.forced = pass
	} else {
		passes.scheduled = pass
	}
	passes.Unlock()

	return pass
}

func (p *backupPass) running(i int, at time.Time) {
	passes.Lock()
	defer passes.Unlock()
	p.Items[i].State, p.Items[i].StartedAt = PassRunning, &at
}

// finished takes how one couple ended from its record, the same words the history keeps.
func (p *backupPass) finished(i int, record BackupRunRecord) {
	passes.Lock()
	defer passes.Unlock()

	item := &p.Items[i]
	finishedAt := record.FinishedAt
	item.FinishedAt, item.Bytes, item.Error = &finishedAt, record.Bytes, record.Error
	switch {
	case record.Error == "":
		item.State = PassDone
	case strings.HasPrefix(record.Error, ErrStoppedFromInterface.Error()):
		item.State = PassStopped
	default:
		item.State = PassFailed
	}
}

// hold leaves every couple from `from` on for after the upgrade.
func (p *backupPass) hold(from int) {
	passes.Lock()
	defer passes.Unlock()
	for i := from; i < len(p.Items); i++ {
		p.Items[i].State = PassHeld
	}
}

// drop takes out a couple a force runs instead: it shows in the forced chain.
func (p *backupPass) drop(i int) {
	passes.Lock()
	defer passes.Unlock()
	p.Items[i].dropped = true
}

func (p *backupPass) end(at time.Time) {
	passes.Lock()
	defer passes.Unlock()
	p.FinishedAt = &at
}

// BackupPasses is the last scheduled pass and the last forced chain, those there are.
func BackupPasses() []BackupPass {
	passes.Lock()
	defer passes.Unlock()

	out := []BackupPass{}
	for _, pass := range []*backupPass{passes.scheduled, passes.forced} {
		if pass == nil {
			continue
		}
		copied := pass.BackupPass
		copied.Items = []BackupPassItem{}
		for _, item := range pass.Items {
			if !item.dropped {
				copied.Items = append(copied.Items, item)
			}
		}
		out = append(out, copied)
	}

	return out
}
