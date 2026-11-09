package service

import (
	"errors"
	"io/fs"
	"os"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// What the box does by itself and an operating system upgrade do not mix: dpkg rewrites the
// system underneath a backup, a build or a start, and the upgrade stops and starts the
// containers they work on as well.
//
// While an upgrade is open the core keeps this file, from the click that starts it until it
// is closed, which includes the days a finished run can wait for its restart. Only the core
// writes it, and it is read here for its existence alone: asking whether it is there needs no
// more than the right to look into its directory, which a service that is not root has.
//
// Held while it is there: the backups that go off by themselves, and the git apps' own
// work -- the check every five minutes, a push, and the deployment a check finds to do.
//
// Not held: what somebody clicks (a backup, a restore, a deployment, a check, which reads and
// whose deployment waits), the checks of the images (they only read the registries; an image
// is updated by a click and by nothing else), and what a start of this service puts back
// (the containers a backup left stopped, the apps a restart found stopped before the storage
// was there): they undo something, and held they would leave an app off.

// osUpgradeBusyPath is the file; the core and this service agree on the name. A variable
// so a test can look at a file of its own with everything else as production wires it.
var osUpgradeBusyPath = "/var/lib/casaos/os-upgrade/busy"

// osUpgradeBusyStat and logHeldForUpgrade are variables so a test can answer with an error
// a temporary directory cannot give (a denied directory, a failing disk) and count what
// was said. The file is looked at, not what it points to: a link whose target is gone is
// still something put there, and the core reads it the same way.
var (
	osUpgradeBusyStat = os.Lstat
	logHeldForUpgrade = logger.Warn
)

// osUpgradeOpen says whether what the box does by itself must stay out of the way.
//
// Only a file that is not there means no upgrade. Any other answer is an upgrade nobody
// can rule out, and is told as one: a backup that waits a day costs less than one taken
// through the middle of an upgrade. The error, when there is one, is why the answer is
// yes although nothing says an upgrade is open.
func osUpgradeOpen() (open bool, err error) {
	_, err = osUpgradeBusyStat(osUpgradeBusyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return true, err
}

// OSUpgradeOpen is osUpgradeOpen for the API, which tells the core's master whether backups wait.
func OSUpgradeOpen() (bool, error) { return osUpgradeOpen() }

// heldForUpgrade is osUpgradeOpen for what a git app does by itself: when it says yes, it also
// says in the log what waits, with fields to tell which.
func heldForUpgrade(what string, fields ...zap.Field) bool {
	open, err := osUpgradeOpen()
	if open {
		logHeldForUpgrade(what+" waits: an operating system upgrade is open, or whether one is cannot be told",
			append(fields, zap.Error(err))...)
	}

	return open
}
