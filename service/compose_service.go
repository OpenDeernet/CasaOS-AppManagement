package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	timeutils "github.com/ReCasaOS/CasaOS-Common/utils/time"
	"gopkg.in/yaml.v3"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/cmd/display"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/client"

	"go.uber.org/zap"
)

type ComposeService struct {
	installationInProgress sync.Map
	_recoveryDone          atomic.Bool
	_recoveryStartedAt     time.Time
}

func (s *ComposeService) PrepareWorkingDirectory(name string) (string, error) {
	workingDirectory := filepath.Join(config.AppInfo.AppsPath, name)

	if err := file.IsNotExistMkDir(workingDirectory); err != nil {
		logger.Error("failed to create working dir", zap.Error(err), zap.String("path", workingDirectory))
		return "", err
	}

	return workingDirectory, nil
}

func (s *ComposeService) IsInstalling(appName string) bool {
	_, ok := s.installationInProgress.Load(appName)
	return ok
}

// Install writes the app's compose file, and env as its .env beside it when there is one (see
// NewComposeAppWithEnv), then installs it.
func (s *ComposeService) Install(ctx context.Context, composeApp *ComposeApp, env []byte) error {
	end, err := Begin(composeApp.Name, "install")
	if err != nil {
		return err
	}
	// released here on every early return, and by the install itself once it runs
	handedOff := false
	defer func() {
		if !handedOff {
			end()
		}
	}()

	// set store_app_id (by convention is the same as app name at install time if it does not exist)
	_, isStoreApp := composeApp.SetStoreAppID(composeApp.Name)
	if !isStoreApp {
		logger.Info("the compose app getting installed is not a store app, skipping store app id setting.")
	}

	logger.Info("installing compose app", zap.String("name", composeApp.Name))

	composeYAMLInterpolated, err := yaml.Marshal(composeApp)
	if err != nil {
		return err
	}

	workingDirectory, err := s.PrepareWorkingDirectory(composeApp.Name)
	if err != nil {
		return err
	}

	yamlFilePath := filepath.Join(workingDirectory, common.ComposeYAMLFileName)

	if err := os.WriteFile(yamlFilePath, composeYAMLInterpolated, 0o600); err != nil {
		logger.Error("failed to save compose file", zap.Error(err), zap.String("path", yamlFilePath))

		if err := file.RMDir(workingDirectory); err != nil {
			logger.Error("failed to cleanup working dir after failing to save compose file", zap.Error(err), zap.String("path", workingDirectory))
		}
		return err
	}

	// before the load: the load resolves the compose file's references from it
	if len(env) > 0 {
		if err := file.WriteToFullPath(env, filepath.Join(workingDirectory, ".env"), 0o600); err != nil {
			logger.Error("failed to save the .env of the compose app", zap.Error(err), zap.String("dir", workingDirectory))
			cleanup(workingDirectory)
			return err
		}
	}

	// load project
	composeApp, err = LoadComposeAppFromConfigFile(composeApp.Name, yamlFilePath)

	if err != nil {
		logger.Error("failed to install compose app", zap.Error(err), zap.String("name", composeApp.Name))
		cleanup(workingDirectory)
		return err
	}

	// prepare for message bus events
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: composeApp.Name}

	if err := composeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", composeApp.Name))
	}

	common.SetProperties(ctx, eventProperties)

	handedOff = true
	go func(ctx context.Context) {
		defer end()

		s.installationInProgress.Store(composeApp.Name, true)
		defer func() {
			s.installationInProgress.Delete(composeApp.Name)
		}()

		go PublishEventWrapper(ctx, common.EventTypeAppInstallBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppInstallEnd, nil)

		if err := composeApp.PullAndInstall(ctx); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppInstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to install compose app", zap.Error(err), zap.String("name", composeApp.Name))
			return
		}
		RecordAppEvent(composeApp.Name, "installed", "", mainImage(composeApp), "")
	}(ctx)

	return nil
}

func (s *ComposeService) Uninstall(ctx context.Context, composeApp *ComposeApp, deleteConfigFolder bool) error {
	// released by the uninstall itself once it runs
	end, err := Begin(composeApp.Name, "uninstall")
	if err != nil {
		return err
	}

	// prepare for message bus events
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: composeApp.Name}

	if err := composeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", composeApp.Name))
	}

	common.SetProperties(ctx, eventProperties)

	clearAppStopped(composeApp.Name)

	// the cached update answers were about the app being removed. Kept, they badge
	// the next install under the same name with an update its button will not act on.
	forgetImageUpdates(composeApp.Name)

	go func(ctx context.Context) {
		defer end()

		go PublishEventWrapper(ctx, common.EventTypeAppUninstallBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppUninstallEnd, nil)

		if err := composeApp.Uninstall(ctx, deleteConfigFolder); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppUninstallError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to uninstall compose app", zap.Error(err), zap.String("name", composeApp.Name))
			return
		}
		RecordAppEvent(composeApp.Name, "uninstalled", "", "", "")
	}(ctx)

	return nil
}

func (s *ComposeService) Status(ctx context.Context, appID string) (string, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return "", err
	}
	defer dockerClient.Close()

	stackList, err := service.List(ctx, api.ListOptions{
		All: true,
	})
	if err != nil {
		return "", err
	}

	for _, stack := range stackList {
		if stack.ID == appID {
			return stack.Status, nil
		}
	}

	return "", ErrComposeAppNotFound
}

func (s *ComposeService) List(ctx context.Context) (map[string]*ComposeApp, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	stackList, err := service.List(ctx, api.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]*ComposeApp{}

	for _, stack := range stackList {

		composeApp, err := LoadComposeAppFromConfigFile(stack.ID, stack.ConfigFiles)
		// load project
		if err != nil {
			logger.Error("failed to load compose file", zap.Error(err), zap.String("path", stack.ConfigFiles))
			continue
		}

		result[stack.ID] = composeApp
	}

	return result, nil
}

func NewComposeService() *ComposeService {
	return &ComposeService{
		installationInProgress: sync.Map{},
		_recoveryStartedAt:     time.Now(),
	}
}

func baseInterpolationMap() map[string]string {
	return map[string]string{
		"DefaultUserName": common.DefaultUserName,
		"DefaultPassword": common.DefaultPassword,
		"PUID":            common.DefaultPUID,
		"PGID":            common.DefaultPGID,
		"TZ":              timeutils.GetSystemTimeZoneName(),
	}
}

func apiService() (api.Compose, client.APIClient, error) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		return nil, nil, err
	}

	if err := dockerCli.Initialize(&flags.ClientOptions{}); err != nil {
		return nil, nil, err
	}

	// compose v5 reports progress only to an event processor, and ignores it by default;
	// v2 wrote it to stderr, plain when stderr is not a terminal, as under systemd.
	service, err := compose.NewComposeService(dockerCli, compose.WithEventProcessor(display.Plain(dockerCli.Err())))
	if err != nil {
		return nil, nil, err
	}

	return service, dockerCli.Client(), nil
}

func ApiService() (api.Compose, client.APIClient, error) {
	return apiService()
}

func cleanup(workDir string) {
	logger.Info("cleaning up working dir", zap.String("path", workDir))
	if err := file.RMDir(workDir); err != nil {
		logger.Error("failed to cleanup working dir", zap.Error(err), zap.String("path", workDir))
	}
}
