package v2

import (
	"net/http"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/labstack/echo/v4"
)

// AppOperations answers what runs on each app right now. The core asks before it updates the
// box by itself, as an internal request.
func (a *AppManagement) AppOperations(ctx echo.Context) error {
	operations := []codegen.AppOperation{}
	for _, operation := range service.RunningOperations() {
		operations = append(operations, codegen.AppOperation{App: operation.App, Kind: operation.Kind})
	}

	// an upgrade that cannot be ruled out is told as open, as the scheduler reads it
	open, _ := osUpgradeOpen()

	return ctx.JSON(http.StatusOK, codegen.AppOperationsOK{Operations: operations, OsUpgradeOpen: open})
}

// osUpgradeOpen is a variable so a test can open an upgrade without the core's file.
var osUpgradeOpen = service.OSUpgradeOpen
