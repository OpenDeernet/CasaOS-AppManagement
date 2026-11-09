package v2

import (
	"errors"
	"net/http"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/backupprogress"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/labstack/echo/v4"
)

// BackupRunning answers what runs and how far it has got, without an envelope, as /operations.
func (a *AppManagement) BackupRunning(ctx echo.Context) error {
	out := codegen.BackupRunningOK{Backups: []codegen.LiveBackup{}, Passes: []codegen.BackupPass{}}

	for _, live := range service.LiveBackups() {
		backup := codegen.LiveBackup{
			Id: live.ID, App: live.App, Destination: live.Destination, Stamp: live.Stamp,
			Origin: string(live.Origin), StartedAt: live.StartedAt,
			Operation: live.Operation, Operations: live.Operations, Source: live.Source,
		}
		if live.Stats != nil {
			backup.Stats = liveStatsOut(*live.Stats, live)
		}
		out.Backups = append(out.Backups, backup)
	}

	for _, pass := range service.BackupPasses() {
		passOut := codegen.BackupPass{
			Origin: string(pass.Origin), StartedAt: pass.StartedAt, FinishedAt: pass.FinishedAt,
			Items: []codegen.BackupPassItem{},
		}
		for _, item := range pass.Items {
			itemOut := codegen.BackupPassItem{
				App: item.App, Destination: item.Destination, State: string(item.State),
				StartedAt: item.StartedAt, FinishedAt: item.FinishedAt,
			}
			if item.FinishedAt != nil {
				itemOut.Bytes = utils.Ptr(item.Bytes)
			}
			if item.Error != "" {
				itemOut.Error = utils.Ptr(item.Error)
			}
			passOut.Items = append(passOut.Items, itemOut)
		}
		out.Passes = append(out.Passes, passOut)
	}

	return ctx.JSON(http.StatusOK, out)
}

// liveStatsOut leaves out what the engine has not said: a zero total is a total not known yet.
func liveStatsOut(stats backupprogress.Stats, live service.LiveBackup) *codegen.LiveBackupStats {
	out := &codegen.LiveBackupStats{Bytes: stats.Bytes, Files: stats.Files, Eta: stats.ETA, UpdatedAt: live.StatsAt}
	if stats.TotalBytes > 0 {
		out.TotalBytes = utils.Ptr(stats.TotalBytes)
	}
	if stats.TotalFiles > 0 {
		out.TotalFiles = utils.Ptr(stats.TotalFiles)
	}
	if stats.Speed > 0 {
		out.Speed = utils.Ptr(stats.Speed)
	}
	if stats.Current != "" {
		out.Current = utils.Ptr(stats.Current)
	}

	return out
}

// StopRunningBackup stops one backup; the pass it belongs to goes on.
func (a *AppManagement) StopRunningBackup(ctx echo.Context, backupID string) error {
	if err := service.StopLiveBackup(backupID); err != nil {
		if errors.Is(err, service.ErrLiveBackupNotFound) {
			return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: utils.Ptr(err.Error())})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: utils.Ptr(err.Error())})
	}

	return ctx.JSON(http.StatusAccepted, codegen.BaseResponse{Message: utils.Ptr("Stop requested")})
}
