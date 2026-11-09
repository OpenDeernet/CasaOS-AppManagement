package v2

import (
	"net/http"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/docker"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/labstack/echo/v4"
)

// Features is what GET /info says this build can do beyond the first release of the API.
var Features = []string{"labels"}

func (a *AppManagement) Info(ctx echo.Context) error {
	architecture, err := docker.CurrentArchitecture()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: utils.Ptr(err.Error()),
		})
	}

	return ctx.JSON(http.StatusOK, codegen.InfoOK{
		Architecture: utils.Ptr(architecture),
		Features:     utils.Ptr(Features),
	})
}
