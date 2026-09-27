package v1

import (
	"errors"
	"net/http"
	"sync"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/dockerlocation"
	modelCommon "github.com/ReCasaOS/CasaOS-Common/model"
	"github.com/ReCasaOS/CasaOS-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// dockerLocationStatePath keeps where Docker's data is and the last move.
const dockerLocationStatePath = "/var/lib/casaos/docker-location.json"

var (
	dockerLocationOnce  sync.Once
	dockerLocationMover *dockerlocation.Mover
)

func dockerLocation() *dockerlocation.Mover {
	dockerLocationOnce.Do(func() { dockerLocationMover = dockerlocation.New(dockerLocationStatePath) })
	return dockerLocationMover
}

func dockerLocationOK(ctx echo.Context, data interface{}) error {
	return ctx.JSON(http.StatusOK, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: data})
}

func dockerLocationFail(ctx echo.Context, status int, err error) error {
	return ctx.JSON(status, &modelCommon.Result{Success: common_err.CLIENT_ERROR, Message: err.Error(), Data: dockerLocation().State()})
}

// GetDockerLocation says where Docker's data is, and how the last move went.
func GetDockerLocation(ctx echo.Context) error {
	return dockerLocationOK(ctx, dockerLocation().State())
}

// CheckDockerLocation says whether a folder, or the default place with
// ?default=true, can take Docker's data.
func CheckDockerLocation(ctx echo.Context) error {
	path := ctx.QueryParam("path")
	if ctx.QueryParam("default") == "true" {
		path = dockerlocation.Default
	} else if path == "" {
		return dockerLocationFail(ctx, http.StatusBadRequest, errors.New("give a path, or default=true"))
	}
	return dockerLocationOK(ctx, dockerLocation().CheckTarget(ctx.Request().Context(), path))
}

// MoveDockerLocation starts moving Docker's data to a folder; GET follows it.
func MoveDockerLocation(ctx echo.Context) error {
	// default: back to /var/lib/docker; a path is required otherwise, so an
	// empty body never moves anything
	var req struct {
		Path    string `json:"path"`
		Default bool   `json:"default"`
	}
	if err := ctx.Bind(&req); err != nil {
		return dockerLocationFail(ctx, http.StatusBadRequest, err)
	}
	if req.Path == "" && !req.Default {
		return dockerLocationFail(ctx, http.StatusBadRequest, errors.New("give a path, or default"))
	}
	if req.Default {
		req.Path = dockerlocation.Default
	}

	state, err := dockerLocation().Start(req.Path)
	if err != nil {
		return dockerLocationFail(ctx, http.StatusBadRequest, err)
	}

	return ctx.JSON(http.StatusAccepted, &modelCommon.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: state})
}

// DeleteDockerLocationOldCopy deletes what the last move left behind.
func DeleteDockerLocationOldCopy(ctx echo.Context) error {
	state, err := dockerLocation().DeleteOldCopy()
	if err != nil {
		return dockerLocationFail(ctx, http.StatusConflict, err)
	}
	return dockerLocationOK(ctx, state)
}
