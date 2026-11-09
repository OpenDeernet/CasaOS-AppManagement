package v2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var ErrComposeAppIDNotProvided = errors.New("compose AppID (compose project name) is not provided")

func (a *AppManagement) MyComposeAppList(ctx echo.Context) error {
	composeAppsWithStoreInfo, err := composeAppsWithStoreInfo(ctx.Request().Context(), composeAppsWithStoreInfoOpts{
		checkIsUpdateAvailable: true,
	})
	if err != nil {
		message := err.Error()
		logger.Error("failed to list compose apps with store info", zap.Error(err))
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppListOK{
		Data: &composeAppsWithStoreInfo,
	})
}

func (a *AppManagement) MyComposeApp(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	accept := ctx.Request().Header.Get(echo.HeaderAccept)
	if accept == common.MIMEApplicationYAML {
		// the editor must see `${KEY}` for what `.env` defines, never the resolved value: the next
		// save would bake it into docker-compose.yml. No fallback to the resolved app for that reason.
		keep, err := composeApp.EnvKeys()
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
		}

		editing, err := service.LoadComposeAppForEditing(id, composeApp.ComposeFiles[0], keep)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
		}

		// generate yaml should to replace all yaml.Marshal. But for now, we just use it Setting Page API
		yaml, err := service.GenerateYAMLFromComposeApp(*editing)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		return ctx.String(http.StatusOK, string(yaml))
	}

	// A missing `x-casaos` is not a failure: the app grid has always answered for such
	// a stack with no store info rather than an error, and this endpoint describes the
	// same apps. Anything else wrong with the extension still is one.
	storeInfo, err := composeApp.StoreInfo(true)
	if err != nil && !errors.Is(err, service.ErrComposeExtensionNameXCasaOSNotFound) {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	status, err := service.MyService.Compose().Status(ctx.Request().Context(), composeApp.Name)
	if err != nil {
		status = "unknown"
		logger.Error("failed to get compose app status", zap.Error(err), zap.String("composeAppID", id))
	}

	// disable the because performance issue
	// check update is hard and cost a lot of time. specially when the tag is latest
	// such as Stable Diffusion. the check by ZimaOS GPU Application by @LinkLeong
	// Alought @LinkLeong Didn't need the field.
	// We should add a new API to get app info without update info
	// and restore the following code

	// check if updateAvailable
	// updateAvailable := service.MyService.AppStoreManagement().IsUpdateAvailable(composeApp)

	message := fmt.Sprintf("!! JSON format is for debugging purpose only - use `Accept: %s` HTTP header to get YAML instead !!", common.MIMEApplicationYAML)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppOK{
		// extension properties aren't marshalled - https://github.com/golang/go/issues/6213
		Message: &message,
		Data: &codegen.ComposeAppWithStoreInfo{
			StoreInfo: storeInfo,
			Compose:   (*types.Project)(composeApp),
			Status:    &status,

			// see above comment
			UpdateAvailable: nil,
		},
	})
}

func (a *AppManagement) IsNewComposeUncontrolled(newComposeApp *service.ComposeApp) (bool, error) {
	// to check if the new compose app is uncontrolled
	newTag, err := newComposeApp.MainTag()
	if err != nil {
		return false, err
	}

	// TODO refactor this. because if user not update. the status will be uncontrolled.
	if lo.Contains(common.NeedCheckDigestTags, newTag) {
		return false, nil
	}

	// compare store info
	StoreApp, err := service.MyService.AppStoreManagement().ComposeApp(newComposeApp.Name)
	if err != nil {
		return false, err
	}

	if StoreApp == nil {
		logger.Error("store app not found", zap.String("composeAppID", newComposeApp.Name))
		return false, nil
	}

	StableTag, err := StoreApp.MainTag()
	if err != nil {
		return false, err
	}

	return StableTag != newTag, nil
}

func (a *AppManagement) ApplyComposeAppSettings(ctx echo.Context, id codegen.ComposeAppID, params codegen.ApplyComposeAppSettingsParams) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	buf, err := YAMLfromRequest(ctx)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// validate new compose yaml (ComposeAppFromSettingsYAML says why interpolation stays on: #1988)
	keep, err := composeApp.EnvKeys()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	newComposeApp, err := service.ComposeAppFromSettingsYAML(buf, keep)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	if err := newComposeApp.ValidateLabels(composeApp); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	uncontrolled, err := a.IsNewComposeUncontrolled(newComposeApp)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	_ = newComposeApp.SetUncontrolled(uncontrolled)
	buf, err = service.GenerateYAMLFromComposeApp(*newComposeApp)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	if params.CheckPortConflict == nil || *params.CheckPortConflict {

		// validation 1 - check if there are ports in use
		validation, err := newComposeApp.GetPortsInUseFor(composeApp.Profiles)
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		if validation != nil && validation.PortsInUse != nil {
			// we want to ignore the ports being used by current compose app
			for _, service := range composeApp.Services {
				for _, portToSkip := range service.Ports {
					if validation.PortsInUse.TCP != nil {
						tcpPortsInUse := []string{}
						for _, tcpPort := range *validation.PortsInUse.TCP {
							if tcpPort != portToSkip.Published {
								tcpPortsInUse = append(tcpPortsInUse, tcpPort)
							}
						}
						validation.PortsInUse.TCP = &tcpPortsInUse
					}

					if validation.PortsInUse.UDP != nil {
						udpPortsInUse := []string{}
						for _, udpPort := range *validation.PortsInUse.UDP {
							if udpPort != portToSkip.Published {
								udpPortsInUse = append(udpPortsInUse, udpPort)
							}
						}
						validation.PortsInUse.UDP = &udpPortsInUse
					}
				}
			}

			if (validation.PortsInUse.TCP != nil && len(*validation.PortsInUse.TCP) > 0) ||
				(validation.PortsInUse.UDP != nil && len(*validation.PortsInUse.UDP) > 0) {

				validationErrors := codegen.ComposeAppValidationErrors{}
				if err := validationErrors.FromComposeAppValidationErrorsPortsInUse(*validation); err != nil {
					message := err.Error()
					return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
						Message: &message,
					})
				}

				message := "there are ports in use"
				return ctx.JSON(http.StatusBadRequest, codegen.ComposeAppBadRequest{
					Message: &message,
					Data:    &validationErrors,
				})
			}
		}
	}

	if params.DryRun != nil && *params.DryRun {
		return ctx.JSON(http.StatusOK, codegen.ComposeAppInstallOK{
			Message: lo.ToPtr("only validation has been done because `dry_run` is specified - skipping compose app installation"),
		})
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := composeApp.Apply(backgroundCtx, buf); err != nil {
		return conflictOrServerError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateSettingsOK{
		Message: utils.Ptr("compose app is being applied with changes asynchroniously"),
	})
}

// conflictOrServerError answers an operation that did not start: 409 naming the operation
// that holds the app, 500 for anything else.
func conflictOrServerError(ctx echo.Context, err error) error {
	message := err.Error()
	if errors.As(err, new(service.ErrAppBusy)) {
		return ctx.JSON(http.StatusConflict, codegen.ResponseConflict{Message: &message})
	}

	return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
}

// installedComposeApp resolves id to an installed app, or writes the error response and returns nil.
func installedComposeApp(ctx echo.Context, id codegen.ComposeAppID) (*service.ComposeApp, error) {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return nil, ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	composeApps, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		message := err.Error()
		return nil, ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	composeApp, ok := composeApps[id]
	if !ok {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return nil, ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	return composeApp, nil
}

func (a *AppManagement) ComposeAppEnv(ctx echo.Context, id codegen.ComposeAppID) error {
	composeApp, err := installedComposeApp(ctx, id)
	if composeApp == nil {
		return err
	}

	text, err := composeApp.EnvFileText()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.String(http.StatusOK, string(text))
}

func (a *AppManagement) ApplyComposeAppEnv(ctx echo.Context, id codegen.ComposeAppID, params codegen.ApplyComposeAppEnvParams) error {
	// the body is validated before the app is resolved: `dry_run` needs nothing else
	body, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if _, err := service.ParseEnvFile(body); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	if params.DryRun != nil && *params.DryRun {
		return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateSettingsOK{
			Message: lo.ToPtr("only validation has been done because `dry_run` is specified - skipping .env update"),
		})
	}

	composeApp, err := installedComposeApp(ctx, id)
	if composeApp == nil {
		return err
	}

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := composeApp.ApplyEnv(backgroundCtx, body); err != nil {
		return conflictOrServerError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateSettingsOK{
		Message: utils.Ptr("compose app is being applied with changes asynchroniously"),
	})
}

func (a *AppManagement) InstallComposeApp(ctx echo.Context, params codegen.InstallComposeAppParams) error {
	buf, err := YAMLfromRequest(ctx)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// a .env sent with the compose (x-casaos-env): checked with its values, installed beside it
	buf, env, err := service.SplitInstallEnv(buf)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	// validate new compose yaml; checked is what the port check looks at, composeApp what is installed
	var composeApp, checked *service.ComposeApp
	if len(env) > 0 {
		checked, composeApp, err = service.NewComposeAppWithEnv(buf, env)
	} else {
		composeApp, err = service.NewComposeAppFromYAML(buf, false, true)
		checked = composeApp
	}
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	if err := composeApp.ValidateLabels(composeApp.StoreVersion()); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	uncontrolled, err := a.IsNewComposeUncontrolled(composeApp)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: &message,
		})
	}

	_ = composeApp.SetUncontrolled(uncontrolled)

	if params.CheckPortConflict == nil || *params.CheckPortConflict {
		// validation 1 - check if there are ports in use
		validation, err := checked.GetPortsInUse()
		if err != nil {
			message := err.Error()
			return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
				Message: &message,
			})
		}

		if validation != nil {
			validationErrors := codegen.ComposeAppValidationErrors{}
			if err := validationErrors.FromComposeAppValidationErrorsPortsInUse(*validation); err != nil {
				message := err.Error()
				return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
					Message: &message,
				})
			}

			message := "there are ports in use"
			return ctx.JSON(http.StatusBadRequest, codegen.ComposeAppBadRequest{
				Message: &message,
				Data:    &validationErrors,
			})
		}
	}

	if params.DryRun != nil && *params.DryRun {
		return ctx.JSON(http.StatusOK, codegen.ComposeAppInstallOK{
			Message: lo.ToPtr("only validation has been done because `dry_run` is specified - skipping compose app installation"),
		})
	}

	if service.MyService.Compose().IsInstalling(composeApp.Name) {
		message := fmt.Sprintf("compose app `%s` is already being installed", composeApp.Name)
		return ctx.JSON(http.StatusConflict, codegen.ComposeAppBadRequest{Message: &message})
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := service.MyService.Compose().Install(backgroundCtx, composeApp, env); err != nil {
		logger.Error("failed to start compose app installation", zap.Error(err))

		message := err.Error()
		if err == service.ErrComposeExtensionNameXCasaOSNotFound {
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		return conflictOrServerError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppInstallOK{
		Message: lo.ToPtr("compose app is being installed asynchronously"),
	})
}

func (a *AppManagement) UninstallComposeApp(ctx echo.Context, id codegen.ComposeAppID, params codegen.UninstallComposeAppParams) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	appList, err := service.MyService.Compose().List(ctx.Request().Context())
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	composeApp, ok := appList[id]
	if !ok {
		message := fmt.Sprintf("compose app `%s` not found", id)
		return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
	}

	// attach context key/value pairs from upstream
	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	deleteConfigFolder := true
	if params.DeleteConfigFolder != nil {
		deleteConfigFolder = *params.DeleteConfigFolder
	}

	if err := service.MyService.Compose().Uninstall(backgroundCtx, composeApp, deleteConfigFolder); err != nil {
		logger.Error("failed to uninstall compose app", zap.Error(err), zap.String("appID", id))
		return conflictOrServerError(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppUninstallOK{
		Message: utils.Ptr("compose app is being uninstalled asynchronously"),
	})
}

func (a *AppManagement) UpdateComposeApp(ctx echo.Context, id codegen.ComposeAppID, params codegen.UpdateComposeAppParams) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	// `force` defaults to false in the API contract, but the generated binding
	// leaves it nil when the query string omits it, and an absent force used to skip
	// the check entirely. That is what made the dashboard's `Check then update`
	// button never check: it applied the store's compose whatever version it held.
	if params.Force == nil || !*params.Force {
		// The button is called `Check then update`, so it checks: ask this app's
		// registries now rather than answering from whatever the last sweep left in
		// the cache, or from nothing at all when no sweep has run. A check that fails
		// is not fatal -- the store comparison below still has something to say.
		checkErr := service.CheckImageUpdatesForApp(ctx.Request().Context(), composeApp)
		if checkErr != nil {
			logger.Info("could not check images before updating", zap.Error(checkErr), zap.String("appID", id))
		}
		// the answer below is cached for an hour, which would outlive the check just made
		service.MyService.AppStoreManagement().ForgetUpgradable(id)

		if available, reason := service.MyService.AppStoreManagement().UpdateAvailability(composeApp); !available {
			// For an app with no catalogue entry the image check IS the answer, so
			// reporting `up to date` after it failed would be a claim about a registry
			// nobody managed to reach.
			if checkErr != nil {
				message := fmt.Sprintf("could not check compose app `%s`: %s", id, checkErr.Error())
				return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{Message: &message})
			}

			// An app held back rather than current: say which service and why, because
			// `is up to date` on an app that is not is how someone ends up with a
			// button that will not act and a screen that will not say why.
			if reason != "" {
				message := fmt.Sprintf("compose app `%s` is not updated: %s", id, reason)
				return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{Message: &message})
			}

			message := fmt.Sprintf("compose app `%s` is up to date", id)
			return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{Message: &message})
		}
	}

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))

	if err := composeApp.Update(backgroundCtx); err != nil {
		logger.Error("failed to update compose app", zap.Error(err), zap.String("appID", id))
		return conflictOrServerError(ctx, err)
	}

	message := fmt.Sprintf("compose app `%s` is being updated asynchronously", id)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppUpdateOK{
		Message: &message,
	})
}

func (a *AppManagement) SetComposeAppStatus(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
	}

	var action codegen.RequestComposeAppStatus
	if err := ctx.Bind(&action); err != nil {
		message := err.Error()
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

	backgroundCtx := common.WithProperties(context.Background(), PropertiesFromQueryParams(ctx))
	if err := composeApp.SetStatus(backgroundCtx, action); err != nil {
		message := err.Error()

		if err == service.ErrInvalidComposeAppStatus {
			return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
		}

		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.RequestComposeAppStatusOK{
		Message: utils.Ptr("compose app status is being changed asynchronously"),
	})
}

func (a *AppManagement) ComposeAppLogs(ctx echo.Context, id codegen.ComposeAppID, params codegen.ComposeAppLogsParams) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	lines := lo.If(params.Lines == nil, 1000).Else(*params.Lines)

	// No service named means the whole app, which is what the app-wide viewer asks for.
	// A named one that the app does not declare is the client asking for the wrong thing
	// -- answering with an empty log would read as a container that never said anything.
	var services []string

	if params.Service != nil && *params.Service != "" {
		if _, ok := composeApp.Services[*params.Service]; !ok {
			message := fmt.Sprintf("service `%s` not found in compose app `%s`", *params.Service, id)
			return ctx.JSON(http.StatusNotFound, codegen.ResponseNotFound{Message: &message})
		}

		services = append(services, *params.Service)
	}

	logs, err := composeApp.Logs(ctx.Request().Context(), lines, services...)
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.ComposeAppLogsOK{Data: utils.Ptr(string(logs))})
}

func (a *AppManagement) ComposeAppContainers(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	containerLists, err := composeApp.Containers(ctx.Request().Context())
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{Message: &message})
	}

	// Not StoreInfo: a stack nobody installed from the App Store has no `x-casaos`,
	// and that used to come back from here as a 500 reading ``extension `x-casaos` not
	// found`` -- so the Containers tab was broken for precisely the hand-assembled
	// stacks it was added for. Which service leads is a question about the compose
	// file and has an answer without the extension.
	main := composeApp.MainServiceName()

	// Every container of every service is reported. Until now the response kept only
	// containerList[0] per service -- a workaround from v0.4.4 to spare the frontend a
	// change -- which hid the replicas of a scaled service, and panicked outright on a
	// service whose list was empty.
	return ctx.JSON(http.StatusOK, codegen.ComposeAppContainersOK{
		Data: &codegen.ComposeAppContainers{
			Main:       &main,
			Containers: &containerLists,
		},
	})
}

func (a *AppManagement) CheckComposeAppHealthByID(ctx echo.Context, id codegen.ComposeAppID) error {
	if id == "" {
		message := ErrComposeAppIDNotProvided.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{
			Message: &message,
		})
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

	result, err := composeApp.HealthCheck()
	if err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{Message: &message})
	}

	if !result {
		return ctx.JSON(http.StatusServiceUnavailable, codegen.ResponseServiceUnavailable{})
	}

	message := fmt.Sprintf("compose app `%s` passed the health check", id)
	return ctx.JSON(http.StatusOK, codegen.ComposeAppHealthCheckOK{
		Message: &message,
	})
}

func YAMLfromRequest(ctx echo.Context) ([]byte, error) {
	var buf []byte

	switch ctx.Request().Header.Get(echo.HeaderContentType) {
	case common.MIMEApplicationYAML:

		_buf, err := io.ReadAll(ctx.Request().Body)
		if err != nil {
			return nil, err
		}

		buf = _buf

	default:
		var c codegen.ComposeApp
		if err := ctx.Bind(&c); err != nil {
			return nil, err
		}

		_buf, err := yaml.Marshal(c)
		if err != nil {
			return nil, err
		}
		buf = _buf
	}

	return buf, nil
}

type composeAppsWithStoreInfoOpts struct {
	checkIsUpdateAvailable bool
	// The /web/appgrid endpoint does not require information about whether the application can be updated, so we added an option.
	// This endpoint is called as soon as CasaOS is opened, and we don't have time to cache it in advance.
	// We must ensure that this endpoint responds as quickly as possible.
}

// containerStateSeverity ranks the states a container can be in, worst last. An app
// is only as healthy as its unhealthiest part, so this is what picks the one state
// the dashboard shows for the whole app.
var containerStateSeverity = map[string]int{
	"running":    0,
	"created":    1,
	"removing":   2,
	"paused":     3,
	"restarting": 4,
	"exited":     5,
	"dead":       6,
}

// composeAppStatus folds every container of every service into the one state the
// dashboard renders for the app. A state this does not recognise outranks every
// state it does: an unfamiliar answer is not a reason to report `running`.
//
// No container at all is `unknown`: the app exists and nothing of it is up, which is
// not a state any container reported.
func composeAppStatus(containerLists map[string][]codegen.ContainerSummary) string {
	worst, worstRank := "unknown", -1

	for _, containers := range containerLists {
		for _, container := range containers {
			rank, known := containerStateSeverity[string(container.State)]
			if !known {
				rank = len(containerStateSeverity)
			}

			if rank > worstRank {
				worst, worstRank = string(container.State), rank
			}
		}
	}

	return worst
}

func composeAppsWithStoreInfo(ctx context.Context, opts composeAppsWithStoreInfoOpts) (map[string]codegen.ComposeAppWithStoreInfo, error) {
	composeApps, err := service.MyService.Compose().List(ctx)
	if err != nil {
		return nil, err
	}

	return lo.MapValues(composeApps, func(composeApp *service.ComposeApp, id string) codegen.ComposeAppWithStoreInfo {
		if composeApp == nil {
			return codegen.ComposeAppWithStoreInfo{}
		}

		composeAppWithStoreInfo := codegen.ComposeAppWithStoreInfo{
			Compose:         (*codegen.ComposeApp)(composeApp),
			StoreInfo:       nil,
			Status:          utils.Ptr("unknown"),
			UpdateAvailable: utils.Ptr(false),
			IsUncontrolled:  utils.Ptr(false),
		}

		// A stack written by hand has no `x-casaos`, and giving up here left it at the
		// `unknown` this struct starts on -- which the dashboard draws as an app that is
		// not running: greyed out, on a stack whose containers are all up. Store info is
		// presentation, and its absence is not a reason to stop answering what the app is
		// DOING. Only the store info goes missing; the status below is still read.
		storeInfo, err := composeApp.StoreInfo(true)
		if err != nil && !errors.Is(err, service.ErrComposeExtensionNameXCasaOSNotFound) {
			logger.Error("failed to get store info", zap.Error(err), zap.String("composeAppID", id))
			return composeAppWithStoreInfo
		}

		composeAppWithStoreInfo.StoreInfo = storeInfo

		if opts.checkIsUpdateAvailable {
			// check if updateAvailable
			updateAvailable := service.MyService.AppStoreManagement().IsUpdateAvailable(composeApp)
			composeAppWithStoreInfo.UpdateAvailable = &updateAvailable
		}

		// status
		//
		// Nothing here asks about the MAIN service. It used to: the status was the
		// state of that service's first container, so both a missing main service and
		// a main service with no container had to bail out before reaching the index.
		// Neither is a reason to give up now -- the fold below reads every container
		// of every service -- and bailing out left the app `unknown` and its
		// IsUncontrolled unread, which is the reported stack exactly: main service
		// down, another service running, no status and no answer.
		containerLists, err := composeApp.Containers(ctx)
		if err != nil {
			logger.Error("failed to get containers", zap.Error(err), zap.String("composeAppID", id))
			return composeAppWithStoreInfo
		}

		// one rule for this, in service: the copy that used to live here is what took
		// the whole service down on a compose file with no `x-casaos`
		composeAppWithStoreInfo.IsUncontrolled = lo.ToPtr(composeApp.IsUncontrolled())

		// The status used to be the state of the main service's FIRST container and
		// nothing else, so an app whose database had died reported `running` and the
		// dashboard drew a green dot -- the one thing that view exists to tell you,
		// wrong. Every container of every service counts now.
		composeAppWithStoreInfo.Status = lo.ToPtr(composeAppStatus(containerLists))

		return composeAppWithStoreInfo
	}), nil
}
