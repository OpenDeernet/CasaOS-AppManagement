package v2

import (
	"errors"
	"net/http"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/labstack/echo/v4"
)

func (a *AppManagement) StartBackupVerification(ctx echo.Context) error {
	var request codegen.BackupVerificationRequest
	if err := ctx.Bind(&request); err != nil {
		return backupBadRequest(ctx, errors.New("a verification needs a JSON destination, app and stamp"))
	}
	record, err := service.StartBackupVerification(service.BackupVerificationOptions{
		Destination: request.Destination, App: request.App, Stamp: request.Stamp,
	}, rclone.NewClient())
	if err != nil {
		return backupVerificationError(ctx, err)
	}
	out := backupVerificationOut(record)
	return ctx.JSON(http.StatusAccepted, codegen.BackupVerificationOK{Message: utils.Ptr("Verification started"), Data: &out})
}

func (a *AppManagement) BackupVerifications(ctx echo.Context) error {
	records, err := service.BackupVerifications()
	if err != nil {
		return backupVerificationError(ctx, err)
	}
	out := make([]codegen.BackupVerificationRecord, 0, len(records))
	for _, record := range records {
		out = append(out, backupVerificationOut(record))
	}
	return ctx.JSON(http.StatusOK, codegen.BackupVerificationsOK{Message: utils.Ptr("OK"), Data: &out})
}

func (a *AppManagement) CancelBackupVerification(ctx echo.Context, verificationId string) error {
	if err := service.CancelBackupVerification(verificationId); err != nil {
		return backupVerificationError(ctx, err)
	}
	records, err := service.BackupVerifications()
	if err != nil {
		return backupVerificationError(ctx, err)
	}
	for _, record := range records {
		if record.ID == verificationId {
			out := backupVerificationOut(record)
			return ctx.JSON(http.StatusAccepted, codegen.BackupVerificationOK{Message: utils.Ptr("Cancellation requested"), Data: &out})
		}
	}
	return backupVerificationError(ctx, service.ErrBackupVerificationNotFound)
}

func backupVerificationOut(record service.BackupVerificationRecord) codegen.BackupVerificationRecord {
	out := codegen.BackupVerificationRecord{
		Id: record.ID, Destination: record.Destination, App: record.App, Stamp: record.Stamp,
		Status:    record.Status,
		StartedAt: record.StartedAt, FinishedAt: record.FinishedAt,
		CheckedOperations: record.CheckedOperations, SkippedCount: record.SkippedCount,
		Files: record.Files, Bytes: record.Bytes,
	}
	if record.Error != "" {
		out.Error = &record.Error
	}
	return out
}

func backupVerificationError(ctx echo.Context, err error) error {
	status := http.StatusInternalServerError
	message := "backup verification is unavailable; check the service and its verification history"
	switch {
	case errors.Is(err, service.ErrBackupVerificationInvalid), errors.Is(err, service.ErrBackupVerificationRestic):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, service.ErrBackupVerificationBusy), errors.Is(err, service.ErrBackupVerificationNeedsRecovery),
		errors.Is(err, service.ErrBackupVerificationNotRunning):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, service.ErrBackupVerificationNotFound):
		status, message = http.StatusNotFound, service.ErrBackupVerificationNotFound.Error()
	}
	return ctx.JSON(status, codegen.BaseResponse{Message: &message})
}
