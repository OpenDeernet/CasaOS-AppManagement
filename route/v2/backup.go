package v2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// stampLayout names a run inside a destination. Sortable, and with nothing in it
// a filesystem objects to -- a colon is not a character every destination accepts
// in a path, and this is read back by people in file browsers.
const stampLayout = "2006-01-02T15-04-05Z"

// BackupDestinations lists the destinations, by name.
//
// Names only. What a destination needs to be reached is rclone's to keep, and a
// secret that is never read back out through an API is a secret that cannot leak
// through one.
func (a *AppManagement) BackupDestinations(ctx echo.Context) error {
	names, err := rclone.NewClient().Destinations()
	if err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationsOK{
		Message: utils.Ptr("OK"),
		Data:    &names,
	})
}

// SetBackupDestination adds or replaces one.
func (a *AppManagement) SetBackupDestination(ctx echo.Context, name codegen.BackupDestinationName) error {
	var spec codegen.BackupDestinationSpec
	if err := ctx.Bind(&spec); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	parameters := map[string]string{}
	if spec.Parameters != nil {
		parameters = *spec.Parameters
	}

	root := ""
	if spec.Root != nil {
		root = *spec.Root
	}

	var err error
	if spec.Encrypt != nil && *spec.Encrypt {
		password := ""
		if spec.Password != nil {
			password = *spec.Password
		}
		err = rclone.NewClient().CreateEncryptedDestination(string(name), spec.Backend, root, parameters, password)
	} else {
		err = rclone.NewClient().CreateDestination(string(name), spec.Backend, root, parameters)
	}
	if err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("destination `%s` saved", name)),
	})
}

// DeleteBackupRun removes one backup from a destination. The run log keeps its
// record: that a backup was taken and later deleted is history worth keeping.
func (a *AppManagement) DeleteBackupRun(ctx echo.Context, name codegen.BackupDestinationName, app codegen.BackupApp, stamp codegen.BackupStamp) error {
	if app == "" || stamp == "" {
		message := "a backup is named by its app and its stamp"
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if err := restic.NewRouter().Purge(ctx.Request().Context(), string(name), path.Join(string(app), string(stamp))); err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("backup `%s` of `%s` deleted from `%s`", stamp, app, name)),
	})
}

// DeleteBackupDestination forgets one. What was copied there stays there.
func (a *AppManagement) DeleteBackupDestination(ctx echo.Context, name codegen.BackupDestinationName) error {
	if err := rclone.NewClient().DeleteDestination(string(name)); err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("destination `%s` forgotten; anything already copied there is untouched", name)),
	})
}

// CheckBackupDestination asks a destination whether it is reachable.
func (a *AppManagement) CheckBackupDestination(ctx echo.Context, name codegen.BackupDestinationName) error {
	space, err := rclone.NewClient().CheckDestination(string(name))
	if err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationCheckOK{
		Message: utils.Ptr("OK"),
		Data: &codegen.BackupDestinationSpace{
			Used: space.Used, Free: space.Free, Total: space.Total,
		},
	})
}

// BackupDestinationRestic says whether a destination keeps its backups with restic.
func (a *AppManagement) BackupDestinationRestic(ctx echo.Context, name codegen.BackupDestinationName) error {
	state := &codegen.BackupDestinationRestic{
		Available: restic.Available(),
		Enabled:   restic.New().Enabled(string(name)),
		Kept:      restic.New().Held(string(name)),
	}

	// how the last check went is a nicety: a file that cannot be read says nothing here
	if checks, err := service.ResticChecks(); err == nil {
		if last, ok := checks[string(name)]; ok {
			state.LastCheck = &codegen.BackupDestinationResticCheck{At: last.At, Ok: last.OK}
			if last.Error != "" {
				state.LastCheck.Error = utils.Ptr(last.Error)
			}
		}
	}

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationResticOK{Message: utils.Ptr("OK"), Data: state})
}

// CheckBackupDestinationRestic starts a check of the repository now and returns; the
// answer is the destination's restic state once it is over.
func (a *AppManagement) CheckBackupDestinationRestic(ctx echo.Context, name codegen.BackupDestinationName) error {
	if !restic.New().Held(string(name)) {
		return backupBadRequest(ctx, fmt.Errorf("`%s` does not keep its backups with restic", name))
	}

	// detached from the request, as a backup is: the check outlives the answer
	if _, started := service.StartResticCheck(context.Background(), time.Now(), service.NewResticChecker(), string(name)); !started {
		message := "a repository is being checked already; each check reads a whole repository, so one runs at a time"
		return ctx.JSON(http.StatusConflict, codegen.ResponseConflict{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("checking the restic repository of `%s`", name)),
	})
}

// EnableBackupDestinationRestic switches a destination to restic and hands back
// the password of its repository (read back later by BackupDestinationResticPassword).
func (a *AppManagement) EnableBackupDestinationRestic(ctx echo.Context, name codegen.BackupDestinationName) error {
	var spec codegen.BackupDestinationResticSpec
	if err := ctx.Bind(&spec); err != nil {
		return backupBadRequest(ctx, err)
	}

	password := ""
	if spec.Password != nil {
		password = *spec.Password
	}

	password, err := restic.New().Enable(ctx.Request().Context(), string(name), password)
	if err != nil {
		// each of these is something the person can fix; the rest is a failure to report
		if errors.Is(err, restic.ErrNotInstalled) || errors.Is(err, restic.ErrAlreadyEnabled) || errors.Is(err, restic.ErrWrongPassword) || errors.Is(err, restic.ErrKeyKept) {
			return backupBadRequest(ctx, err)
		}

		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationResticPasswordOK{
		Message: utils.Ptr("OK"),
		Data:    &codegen.BackupDestinationResticPassword{Password: password},
	})
}

// BackupDestinationResticPassword reads back the password of a destination's
// repository, for its owner to keep away from this box.
func (a *AppManagement) BackupDestinationResticPassword(ctx echo.Context, name codegen.BackupDestinationName) error {
	password, err := restic.New().Password(string(name))
	if err != nil {
		if errors.Is(err, restic.ErrNotKept) {
			return backupBadRequest(ctx, err)
		}

		return backupError(ctx, err)
	}

	// a secret: not to be kept by a browser or a proxy on the way
	ctx.Response().Header().Set("Cache-Control", "no-store")

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationResticPasswordOK{
		Message: utils.Ptr("OK"),
		Data:    &codegen.BackupDestinationResticPassword{Password: password},
	})
}

// DisableBackupDestinationRestic sends the next backups of a destination back to
// rclone. Nothing is deleted: the repository and its key stay, and it can be taken
// up again.
func (a *AppManagement) DisableBackupDestinationRestic(ctx echo.Context, name codegen.BackupDestinationName) error {
	if err := restic.New().Disable(string(name)); err != nil {
		if errors.Is(err, restic.ErrNotKept) {
			return backupBadRequest(ctx, err)
		}

		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("the next backups of `%s` are stamped copies again; its restic repository is kept and still read", name)),
	})
}

// BackupComposeApp starts a backup and returns.
//
// A copy takes as long as it takes, so nothing waits for it here. What the caller
// gets back is that it started, and where it is going; the rest arrives on the
// message bus, like every other long operation in this service.
func (a *AppManagement) BackupComposeApp(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	var request codegen.BackupRequest
	if err := ctx.Bind(&request); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if request.Destination == "" {
		message := "a backup needs a destination"
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	composeApps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	composeApp, ok := composeApps[id]
	if !ok {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	holdStill := holdStillFrom(request)
	keep := keepFrom(request)
	stamp := time.Now().UTC().Format(stampLayout)

	// Detached from the request: the copy outlives it by design, and a caller that
	// closes its connection must not cancel a backup half-written.
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	go runBackupInBackground(backgroundCtx, composeApp, request.Destination, stamp, holdStill, keep)

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("backing `%s` up to `%s` as `%s`", id, request.Destination, stamp)),
	})
}

func runBackupInBackground(ctx context.Context, composeApp *service.ComposeApp, destination, stamp string, holdStill bool, keep int) {
	client := restic.NewRouter()
	manifest, err := service.BackupOnDemand(ctx, composeApp, service.MyService.Docker(), client, service.BackupOptions{
		Destination: destination,
		Stamp:       stamp,
		HoldStill:   holdStill,
	})
	if err != nil {
		logger.Error("backup failed",
			zap.Error(err), zap.String("app", composeApp.Name), zap.String("destination", destination), zap.String("stamp", stamp))

		return
	}

	logger.Info("backup finished",
		zap.String("app", composeApp.Name), zap.String("destination", destination), zap.String("stamp", stamp),
		zap.Int("copied", len(manifest.Operations)), zap.Int("skipped", len(manifest.Skipped)))

	// Only after a backup that landed: retention after a failure could drop a
	// good backup in favour of nothing.
	service.ApplyRetention(ctx, destination, composeApp.Name, keep, client)
}

// backupError answers rclone's failures.
//
// The daemon not running and a destination being wrong are the same HTTP status
// and completely different problems, so the message has to carry the difference:
// one is fixed by starting a service, the other by correcting a bucket name.
func backupError(ctx echo.Context, err error) error {
	message := err.Error()

	return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
}

// holdStillFrom reads whether to stop the app, defaulting to stopping it.
//
// Copying a database while it is writing produces a backup that looks fine and
// does not restore. A field the request omits arrives nil, and nil has to mean
// the safe answer here rather than the zero value -- Go's zero for a bool is
// false, which would quietly turn every request that forgot the field into a hot
// copy.
func holdStillFrom(request codegen.BackupRequest) bool {
	if request.HoldStill == nil {
		return true
	}

	return *request.HoldStill
}

// keepFrom is the retention a request asks for, or zero for none: an absent
// field keeps everything, and so does anything that is not a count.
func keepFrom(request codegen.BackupRequest) int {
	if request.Keep == nil || *request.Keep <= 0 {
		return 0
	}

	return *request.Keep
}

// BackupSchedules lists the standing arrangements.
func (a *AppManagement) BackupSchedules(ctx echo.Context) error {
	schedules, err := service.BackupSchedules()
	if err != nil {
		return backupError(ctx, err)
	}

	out := make([]codegen.BackupSchedule, 0, len(schedules))
	for _, schedule := range schedules {
		out = append(out, toCodegenSchedule(schedule))
	}

	return ctx.JSON(http.StatusOK, codegen.BackupSchedulesOK{
		Message: utils.Ptr("OK"), Data: &out,
	})
}

// SetBackupSchedules replaces the lot.
//
// Every schedule is checked before any is saved. A list saved half-valid is a box
// where some backups run and some silently never will, and nothing on the screen
// tells the two apart.
func (a *AppManagement) SetBackupSchedules(ctx echo.Context) error {
	var incoming []codegen.BackupSchedule
	if err := ctx.Bind(&incoming); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	// What is already on disk, so a LastRun the screen never saw is not lost --
	// losing it makes every schedule fire again at the next tick.
	existing, err := service.BackupSchedules()
	if err != nil {
		return backupError(ctx, err)
	}

	lastRuns := map[string]time.Time{}
	for _, schedule := range existing {
		lastRuns[schedule.App+"\x00"+schedule.Destination] = schedule.LastRun
	}

	schedules := make([]service.BackupSchedule, 0, len(incoming))
	for _, item := range incoming {
		schedule := fromCodegenSchedule(item)
		schedule.LastRun = lastRuns[schedule.App+"\x00"+schedule.Destination]

		if _, err := service.IsDue(schedule, time.Now()); err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		schedules = append(schedules, schedule)
	}

	if err := service.SaveBackupSchedules(schedules); err != nil {
		return backupError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("%d schedule(s) saved", len(schedules))),
	})
}

// BackupRuns is what happened, newest first, failures included.
func (a *AppManagement) BackupRuns(ctx echo.Context) error {
	records, err := service.BackupRuns()
	if err != nil {
		return backupError(ctx, err)
	}

	out := make([]codegen.BackupRunRecord, 0, len(records))
	for _, record := range records {
		out = append(out, backupRunOut(record))
	}

	return ctx.JSON(http.StatusOK, codegen.BackupRunsOK{
		Message: utils.Ptr("OK"), Data: &out,
	})
}

func toCodegenSchedule(schedule service.BackupSchedule) codegen.BackupSchedule {
	weekday := int(schedule.Weekday)
	every := string(schedule.Every)
	lastRun := schedule.LastRun

	return codegen.BackupSchedule{
		App: schedule.App, Destination: schedule.Destination,
		Every: codegen.BackupScheduleEvery(every), At: schedule.At,
		Weekday: &weekday, Keep: &schedule.Keep,
		HoldStill: &schedule.HoldStill, Enabled: &schedule.Enabled,
		LastRun: &lastRun,
	}
}

func fromCodegenSchedule(item codegen.BackupSchedule) service.BackupSchedule {
	schedule := service.BackupSchedule{
		App: item.App, Destination: item.Destination,
		Every: service.ScheduleEvery(item.Every), At: item.At,
	}

	if item.Weekday != nil {
		schedule.Weekday = time.Weekday(*item.Weekday)
	}
	if item.Keep != nil {
		schedule.Keep = *item.Keep
	}
	if item.HoldStill != nil {
		schedule.HoldStill = *item.HoldStill
	}
	if item.Enabled != nil {
		schedule.Enabled = *item.Enabled
	}

	return schedule
}

// RestoreBackup puts an app back from a backup, in the background.
func (a *AppManagement) RestoreBackup(ctx echo.Context) error {
	var request codegen.RestoreRequest
	if err := ctx.Bind(&request); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	opts := service.RestoreOptions{Destination: request.Destination, App: request.App, Stamp: request.Stamp}
	if request.Paths != nil {
		opts.Paths = *request.Paths
	}
	switch {
	case opts.Destination == "":
		return backupBadRequest(ctx, service.ErrRestoreNeedsDestination)
	case opts.App == "":
		return backupBadRequest(ctx, service.ErrRestoreNeedsApp)
	case opts.Stamp == "":
		return backupBadRequest(ctx, service.ErrRestoreNeedsStamp)
	}
	// said now rather than from the background, where only the run log would hear it
	if len(opts.Paths) > 0 {
		if opts.App == service.SystemBackupName {
			return backupBadRequest(ctx, service.ErrRestoreBoxInParts)
		}
		if err := service.CheckRestorePaths(opts.Paths); err != nil {
			return backupBadRequest(ctx, err)
		}
	}

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	// The box itself is not an app: no compose file to install, and the files
	// come back under services that are stopped for it.
	if opts.App == service.SystemBackupName {
		go func() {
			report, err := service.SystemRestoreOnDemand(backgroundCtx, restic.NewRouter(), service.Systemd(), service.SystemRestoreOptions{
				Destination: opts.Destination, Stamp: opts.Stamp,
			})
			if err != nil {
				logger.Error("restore of the box failed", zap.Error(err), zap.String("destination", opts.Destination), zap.String("stamp", opts.Stamp))
				return
			}
			logger.Info("restore of the box finished", zap.Int("restored", len(report.Restored)), zap.Int("missing", len(report.Missing)))
		}()

		return ctx.JSON(http.StatusOK, codegen.BaseResponse{
			Message: utils.Ptr(fmt.Sprintf("restoring this box from `%s` as of `%s`", opts.Destination, opts.Stamp)),
		})
	}

	composeApps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return backupError(ctx, err)
	}
	// nil when the app is not installed: the restore installs it first, from the
	// compose file the backup holds
	installed := composeApps[opts.App]
	if installed == nil && len(opts.Paths) > 0 {
		return backupBadRequest(ctx, service.ErrRestorePathsNeedApp)
	}

	go func() {
		report, err := service.RestoreOnDemand(backgroundCtx, installed, service.MyService.Docker(), restic.NewRouter(), installFromBackup, opts)
		if err != nil {
			logger.Error("restore failed",
				zap.Error(err), zap.String("app", opts.App), zap.String("destination", opts.Destination), zap.String("stamp", opts.Stamp))
			return
		}

		logger.Info("restore finished",
			zap.String("app", opts.App), zap.String("destination", opts.Destination), zap.String("stamp", opts.Stamp),
			zap.Int("restored", len(report.Restored)), zap.Int("missing", len(report.Missing)), zap.Bool("installed", report.Installed))
	}()

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("restoring `%s` from `%s` as of `%s`", opts.App, opts.Destination, opts.Stamp)),
	})
}

func backupBadRequest(ctx echo.Context, err error) error {
	message := err.Error()

	return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
}

// installFromBackup puts an app that is not installed back from the compose file
// its backup holds, and its .env when there was one, and waits until its
// containers exist -- so that its named volumes exist and can be restored into.
//
// This is Install without the store lookup and without the goroutine: a restore
// has to know the app is there before it starts copying into it.
func installFromBackup(ctx context.Context, name string, compose, env []byte) (*service.ComposeApp, error) {
	workingDirectory, err := service.MyService.Compose().PrepareWorkingDirectory(name)
	if err != nil {
		return nil, err
	}

	yamlFilePath := filepath.Join(workingDirectory, common.ComposeYAMLFileName)
	if err := os.WriteFile(yamlFilePath, compose, 0o600); err != nil {
		return nil, err
	}
	if env != nil {
		if err := os.WriteFile(filepath.Join(workingDirectory, ".env"), env, 0o600); err != nil {
			return nil, err
		}
	}

	composeApp, err := service.LoadComposeAppFromConfigFile(name, yamlFilePath)
	if err != nil {
		return nil, err
	}

	if err := composeApp.PullAndInstall(ctx); err != nil {
		return nil, err
	}

	return composeApp, nil
}

// backupRunOut is the record as the API shows it.
//
// Field by field, because the generated type is pointers and the service type
// is not -- and a field forgotten here is a field the dashboard and the install
// check never see. The restore flag was forgotten here first: the log had it,
// the API did not, and the check waited two minutes for a run it had already
// been handed. The test beside this compares the two encodings whole, so the
// next field added to the record cannot be dropped here silently.
func backupRunOut(record service.BackupRunRecord) codegen.BackupRunRecord {
	return codegen.BackupRunRecord{
		App: &record.App, Destination: &record.Destination, Stamp: &record.Stamp,
		StartedAt: &record.StartedAt, FinishedAt: &record.FinishedAt,
		Scheduled: &record.Scheduled, ContainersStopped: &record.ContainersStopped,
		Copied: &record.Copied, SkippedCount: &record.SkippedCount, Bytes: &record.Bytes,
		Error: &record.Error, Restore: &record.Restore,
	}
}

// BackupDestinationRuns is what a destination holds: the apps it has backups
// of, their runs newest first, and whether this box runs each app now.
func (a *AppManagement) BackupDestinationRuns(ctx echo.Context, name codegen.BackupDestinationName) error {
	composeApps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		return backupError(ctx, err)
	}
	installed := map[string]bool{service.SystemBackupName: true}
	for appName := range composeApps {
		installed[appName] = true
	}

	held, err := service.DestinationContents(ctx.Request().Context(), restic.NewRouter(), string(name), installed)
	if err != nil {
		return backupError(ctx, err)
	}

	out := make([]codegen.BackupHeldApp, 0, len(held))
	for _, app := range held {
		out = append(out, codegen.BackupHeldApp{App: app.App, Installed: app.Installed, Stamps: app.Stamps})
	}

	return ctx.JSON(http.StatusOK, codegen.BackupDestinationRunsOK{
		Message: utils.Ptr("OK"), Data: &out,
	})
}

// BrowseBackup lists one level inside a backup, to choose what to put back.
func (a *AppManagement) BrowseBackup(ctx echo.Context, name codegen.BackupDestinationName, app codegen.BackupApp, stamp codegen.BackupStamp, params codegen.BrowseBackupParams) error {
	if string(app) == service.SystemBackupName {
		return backupBadRequest(ctx, service.ErrRestoreBoxInParts)
	}

	rel := ""
	if params.Path != nil {
		rel = *params.Path
	}

	files, err := service.BrowseBackup(ctx.Request().Context(), restic.NewRouter(), string(name), string(app), string(stamp), rel)
	if err != nil {
		if errors.Is(err, service.ErrBackupPathOutside) || errors.Is(err, service.ErrBackupPathNotHeld) {
			return backupBadRequest(ctx, err)
		}

		return backupError(ctx, err)
	}

	out := make([]codegen.BackupFile, 0, len(files))
	for _, file := range files {
		item := codegen.BackupFile{Name: file.Name, Directory: file.Directory, Size: file.Size}
		if !file.Modified.IsZero() {
			modified := file.Modified
			item.Modified = &modified
		}
		out = append(out, item)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupFilesOK{Message: utils.Ptr("OK"), Data: &out})
}

// BackupSystem backs the box itself up, in the background.
func (a *AppManagement) BackupSystem(ctx echo.Context) error {
	var request codegen.SystemBackupRequest
	if err := ctx.Bind(&request); err != nil {
		return backupBadRequest(ctx, err)
	}
	if request.Destination == "" {
		message := "a backup needs a destination"
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	holdStill := true
	if request.HoldStill != nil {
		holdStill = *request.HoldStill
	}
	stamp := time.Now().UTC().Format(stampLayout)
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	go func() {
		manifest, err := service.SystemBackupOnDemand(backgroundCtx, restic.NewRouter(), service.Systemd(), service.SystemBackupOptions{
			Destination: request.Destination, Stamp: stamp, HoldStill: holdStill,
		})
		if err != nil {
			logger.Error("backup of the box failed", zap.Error(err), zap.String("destination", request.Destination), zap.String("stamp", stamp))
			return
		}
		logger.Info("backup of the box finished", zap.String("destination", request.Destination), zap.String("stamp", stamp),
			zap.Int("copied", len(manifest.Operations)), zap.Int("skipped", len(manifest.Skipped)))
	}()

	return ctx.JSON(http.StatusOK, codegen.BaseResponse{
		Message: utils.Ptr(fmt.Sprintf("backing this box up to `%s` as `%s`", request.Destination, stamp)),
	})
}

// BackupSignIn starts a sign-in to a cloud drive and returns the address to open.
func (a *AppManagement) BackupSignIn(ctx echo.Context) error {
	var spec codegen.BackupSignInSpec
	if err := ctx.Bind(&spec); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	address, err := rclone.SignIn(string(spec.Backend))
	if err != nil {
		return signInError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupSignInOK{
		Message: utils.Ptr("OK"),
		Data:    &codegen.BackupSignInLink{Url: &address},
	})
}

// BackupSignInFinish trades the page the provider redirected to for a token.
func (a *AppManagement) BackupSignInFinish(ctx echo.Context) error {
	var spec codegen.BackupSignInFinishSpec
	if err := ctx.Bind(&spec); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	token, err := rclone.CompleteSignIn(spec.Redirected)
	if err != nil {
		return signInError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.BackupSignInFinishOK{
		Message: utils.Ptr("OK"),
		Data:    &codegen.BackupSignInToken{Token: &token},
	})
}

// signInError answers a mistake in what was sent as a bad request, and anything
// else -- rclone missing, rclone failing -- as the box's own error.
func signInError(ctx echo.Context, err error) error {
	var bad rclone.BadSignIn
	if errors.As(err, &bad) {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return backupError(ctx, err)
}
