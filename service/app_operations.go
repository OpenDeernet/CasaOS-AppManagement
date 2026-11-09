package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// One operation per app.
//
// Installs, updates, applies and backup holds each keep a marker of their own for their
// own readers, and none of them looks at the others': a settings save could land in the
// middle of a deployment that is switching the app's containers. This is the one place
// that refuses an overlap. The markers stay for what reads them.

// ErrAppBusy is the refusal, naming what holds the app. The API answers it with 409.
type ErrAppBusy struct {
	App     string
	Running string
}

func (e ErrAppBusy) Error() string {
	return fmt.Sprintf("`%s` is busy: %s in progress", e.App, e.Running)
}

var appOperations = struct {
	sync.Mutex
	running map[string]string
	// marked is what runs on an app without holding it: see markInProgress
	marked map[string][]string
}{running: map[string]string{}, marked: map[string][]string{}}

// Begin claims app for an operation of the given kind, or refuses with ErrAppBusy. end
// releases it; calling it more than once is harmless.
func Begin(app, kind string) (end func(), err error) {
	appOperations.Lock()
	defer appOperations.Unlock()

	if running, ok := appOperations.running[app]; ok {
		return nil, ErrAppBusy{App: app, Running: running}
	}
	appOperations.running[app] = kind

	var once sync.Once

	return func() {
		once.Do(func() {
			appOperations.Lock()
			defer appOperations.Unlock()
			delete(appOperations.running, app)
		})
	}, nil
}

// markInProgress says that an operation of the given kind runs on app, without claiming
// it: it never refuses, and it stops nothing else from running. done says it is over;
// calling it more than once is harmless.
//
// It is for what RunningOperations must list and Begin never sees. A backup copied from
// the running app, as asked or because the app answers DNS, takes no hold; nor does the
// box's own backup or restore, which hold services rather than an app; nor the retention
// that deletes old runs once a backup is written. The core reads the list before it
// updates the box, and the update stops app-management with whatever it is copying.
func markInProgress(app, kind string) (done func()) {
	appOperations.Lock()
	defer appOperations.Unlock()

	appOperations.marked[app] = append(appOperations.marked[app], kind)

	var once sync.Once

	return func() {
		once.Do(func() {
			appOperations.Lock()
			defer appOperations.Unlock()

			kinds := appOperations.marked[app]
			i := slices.Index(kinds, kind)
			if kinds = slices.Delete(kinds, i, i+1); len(kinds) == 0 {
				delete(appOperations.marked, app)
			} else {
				appOperations.marked[app] = kinds
			}
		})
	}
}

// AppOperation is one app something runs on, and the kind of operation it is.
type AppOperation struct {
	App  string
	Kind string
}

// RunningOperations lists what runs on the apps right now: what Begin holds, and what
// markInProgress says runs without a hold. One entry per app, the hold's kind over a mark's,
// sorted by app so that the same operations always read the same. Empty when nothing runs:
// the core asks before it updates the box by itself, and waits while anything is listed.
func RunningOperations() []AppOperation {
	appOperations.Lock()
	defer appOperations.Unlock()

	kinds := maps.Clone(appOperations.running)
	for app, marked := range appOperations.marked {
		if _, held := kinds[app]; !held {
			kinds[app] = marked[0]
		}
	}

	operations := []AppOperation{}
	for _, app := range slices.Sorted(maps.Keys(kinds)) {
		operations = append(operations, AppOperation{App: app, Kind: kinds[app]})
	}

	return operations
}

// howOftenToAskAgain is the gap between two attempts at a held app. Short enough
// that a hold let go is picked up without anybody noticing, long enough to be
// nothing at all next to the operations it waits for.
const howOftenToAskAgain = 250 * time.Millisecond

// beginWaiting claims app like Begin, except that a hold somebody else has makes it
// wait for its turn instead of refusing. It gives up with that same refusal when
// patience runs out or ctx ends.
//
// Only an operation that can sensibly queue should use it. An install, an update or a
// settings save is the answer to somebody clicking, and a refusal they can read beats
// a request that hangs. A backup is the other kind: it was asked for by a timer, or
// the moment after an install whose hold is about to be let go, and nobody meant it to
// fail because it arrived a few seconds early.
func beginWaiting(ctx context.Context, app, kind string, patience time.Duration) (end func(), err error) {
	deadline := time.Now().Add(patience)
	waited := false

	for {
		end, err := Begin(app, kind)
		if err == nil {
			if waited {
				logger.Info("the app came free", zap.String("app", app), zap.String("for", kind))
			}

			return end, nil
		}

		if !waited {
			waited = true

			var busy ErrAppBusy
			_ = errors.As(err, &busy)
			logger.Info("waiting for the app to come free",
				zap.String("app", app), zap.String("for", kind), zap.String("held_by", busy.Running))
		}

		if time.Now().After(deadline) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(howOftenToAskAgain):
		}
	}
}

// held reports whether an operation holds app: what Begin claimed and has not let go. A
// mark is no hold: what runs without one leaves the app's containers alone.
func held(app string) bool {
	appOperations.Lock()
	defer appOperations.Unlock()

	_, ok := appOperations.running[app]

	return ok
}

// handOver renames what holds app when an operation passes its hold to one it starts, so
// that a refusal names what really runs.
func handOver(app, kind string) {
	appOperations.Lock()
	defer appOperations.Unlock()

	if _, ok := appOperations.running[app]; ok {
		appOperations.running[app] = kind
	}
}

// appOperationRunning says whether an operation of this kind runs on app, held or marked:
// RunningOperations shows one kind per app, which can hide it.
func appOperationRunning(app, kind string) bool {
	appOperations.Lock()
	defer appOperations.Unlock()

	return appOperations.running[app] == kind || slices.Contains(appOperations.marked[app], kind)
}
