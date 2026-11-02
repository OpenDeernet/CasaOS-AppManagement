package service

import (
	"context"
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// A repository nobody looks at is a repository nobody knows is good: a pack lost at the
// provider is found the day a restore needs it. Once a week every destination kept with
// restic is asked whether its repository is sound, and a "no" is told on the message
// bus for the core's alerts.

// ResticCheckPath is where the last check of each destination is written down.
var ResticCheckPath = "/var/lib/casaos/backups/restic-checks.json"

const (
	resticCheckEvery = 7 * 24 * time.Hour
	// a repository that failed is looked at again sooner: the person is told, mends it,
	// and wants to know it is mended
	resticCheckAfterFault = 24 * time.Hour
	// a check of a big repository over a slow link takes hours, not days
	resticCheckLimit = 6 * time.Hour
)

// ResticCheck is the last check of one destination.
type ResticCheck struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
}

var (
	resticCheckLock sync.Mutex
	// held by the tick that is running, and by a check asked for by hand: one at a time,
	// since a check reads the whole repository
	resticCheckBusy sync.Mutex
)

// ResticChecks reads what is known of the last checks, by destination.
func ResticChecks() (map[string]ResticCheck, error) {
	resticCheckLock.Lock()
	defer resticCheckLock.Unlock()

	return readResticChecks()
}

func readResticChecks() (map[string]ResticCheck, error) {
	content, err := os.ReadFile(ResticCheckPath)
	if os.IsNotExist(err) || (err == nil && len(content) == 0) {
		return map[string]ResticCheck{}, nil
	}
	if err != nil {
		return nil, err
	}

	checks := map[string]ResticCheck{}
	if err := stdjson.Unmarshal(content, &checks); err != nil {
		return nil, err
	}

	return checks, nil
}

func recordResticCheck(destination string, check ResticCheck) error {
	resticCheckLock.Lock()
	defer resticCheckLock.Unlock()

	// a file that will not parse is started again: it holds when each destination was last
	// looked at, and the worst a fresh start does is look once more
	checks, err := readResticChecks()
	if err != nil {
		checks = map[string]ResticCheck{}
	}
	checks[destination] = check

	encoded, err := stdjson.MarshalIndent(checks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ResticCheckPath), 0o700); err != nil {
		return err
	}

	temp := ResticCheckPath + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}

	return os.Rename(temp, ResticCheckPath)
}

// resticChecker is what the checks need of the world, so the decisions can be tested
// without a daemon, a repository or a clock.
type resticChecker interface {
	Destinations() ([]string, error)
	Held(destination string) bool
	Check(ctx context.Context, destination string) error
}

type resticCheckerOnThisBox struct {
	*rclone.Client
	*restic.Engine
}

// NewResticChecker is what the tick drives in production.
func NewResticChecker() resticChecker { //nolint:revive // deliberately unexported type behind a constructor
	return resticCheckerOnThisBox{rclone.NewClient(), restic.New()}
}

// RunDueResticChecks checks the destinations kept with restic that are due: never checked,
// last checked a week ago, or a day ago when that check found a fault.
//
// Like the backups it waits while an operating system upgrade is open, and it checks one
// destination at a time. A tick that finds another running leaves the work to it.
func RunDueResticChecks(ctx context.Context, now time.Time, checker resticChecker) {
	if !resticCheckBusy.TryLock() {
		return
	}
	defer resticCheckBusy.Unlock()

	if open, _ := osUpgradeOpen(); open {
		return
	}

	destinations, err := checker.Destinations()
	if err != nil {
		logger.Error("could not list the destinations, so no repository was checked", zap.Error(err))
		return
	}

	checks, err := ResticChecks()
	if err != nil {
		// the same repositories get looked at again, which costs a download of their metadata
		logger.Error("could not read when the repositories were last checked", zap.Error(err))
		checks = map[string]ResticCheck{}
	}

	for _, destination := range destinations {
		if !checker.Held(destination) || !resticCheckDue(checks[destination], now) {
			continue
		}

		checkResticDestination(ctx, now, checker, destination)
	}
}

func resticCheckDue(last ResticCheck, now time.Time) bool {
	switch {
	case last.At.IsZero():
		return true
	case last.OK:
		return now.Sub(last.At) >= resticCheckEvery
	default:
		return now.Sub(last.At) >= resticCheckAfterFault
	}
}

// StartResticCheck checks one destination now, in the background, and says whether it
// started: not while another check runs, since each one reads the whole repository. The
// channel closes when the check is over.
func StartResticCheck(ctx context.Context, now time.Time, checker resticChecker, destination string) (<-chan struct{}, bool) {
	if !resticCheckBusy.TryLock() {
		return nil, false
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resticCheckBusy.Unlock()

		checkResticDestination(ctx, now, checker, destination)
	}()

	return done, true
}

func checkResticDestination(ctx context.Context, now time.Time, checker resticChecker, destination string) {
	limited, cancel := context.WithTimeout(ctx, resticCheckLimit)
	defer cancel()

	err := checker.Check(limited, destination)
	check := ResticCheck{At: now, OK: err == nil}

	if err != nil {
		check.Error = err.Error()
		logger.Error("the restic repository of a destination does not check out",
			zap.Error(err), zap.String("destination", destination))

		PublishEventWrapper(common.WithProperties(ctx, map[string]string{
			common.PropertyTypeBackupDestination.Name: destination,
		}), common.EventTypeBackupCheckError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
	}

	// written even after a fault: a failing check is not tried again every minute, and the
	// fault is in the log, on the bus and in this file
	if err := recordResticCheck(destination, check); err != nil {
		logger.Error("could not write down that a repository was checked, so it may be checked again",
			zap.Error(err), zap.String("destination", destination))
	}
}
