//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/app_management/openapi.yaml > codegen/app_management_api.go"
//go:generate bash -c "mkdir -p codegen/message_bus && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,client -package message_bus https://raw.githubusercontent.com/ReCasaOS/CasaOS-MessageBus/v0.4.19/api/message_bus/openapi.yaml > codegen/message_bus/api.go"

package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/rclone"
	"github.com/ReCasaOS/CasaOS-AppManagement/route"
	v1 "github.com/ReCasaOS/CasaOS-AppManagement/route/v1"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/model"
	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/coreos/go-systemd/daemon"
	client2 "github.com/docker/docker/client"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	util_http "github.com/ReCasaOS/CasaOS-Common/utils/http"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/index_v1.html
	_docHTMLV1 string

	//go:embed api/app_management/openapi.yaml
	_docYAML string

	//go:embed api/app_management/openapi_v1.yaml
	_docYAMLV1 string

	//go:embed build/sysroot/etc/casaos/app-management.conf.sample
	_confSample string
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// parse arguments and intialize
	{
		configFlag := flag.String("c", "", "config file path")
		versionFlag := flag.Bool("v", false, "version")
		removeRuntimeIfNoNvidiaGPUFlag := flag.Bool("removeRuntimeIfNoNvidiaGPU", false, "remove runtime with nvidia gpu")

		flag.Parse()

		if *versionFlag {
			fmt.Printf("v%s\n", common.AppManagementVersion)
			os.Exit(0)
		}

		println("git commit:", commit)
		println("build date:", date)

		config.InitSetup(*configFlag, _confSample)
		config.InitGlobal(*configFlag)

		root := service.ApplyDataPath(config.AppInfo.DataPath)
		service.ApplyGitAppsDataRoot(os.Getenv("CASAOS_GIT_APPS_DATA"))
		v1.DockerRootDirFilePath = filepath.Join(root, "docker_root")

		logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)

		service.MyService = service.NewService(config.CommonInfo.RuntimePath)

		// before the cron below, so the app grid's first paint carries the badges the
		// last run left rather than none until a sweep has finished
		service.LoadImageUpdates()

		// A backup stops the app it is copying and starts it again afterwards. A
		// process killed in between -- an upgrade, a power cut -- leaves the app off
		// and nothing looking at it. This is the only thing that looks.
		//
		// Once, here, and never on a tick: a note belonging to a backup running right
		// now would be read as one to undo, and the app would come back up in the
		// middle of its own copy.
		service.RecoverHeldApps(service.MyService.Docker())

		// A deployment or a check of a git app the last process did not live to finish is
		// recorded as interrupted, and what its build left is removed. Once, here, for the
		// same reason as above: an operation running now would be taken for one to undo.
		service.RecoverGitApps(ctx)

		config.RemoveRuntimeIfNoNvidiaGPUFlag = *removeRuntimeIfNoNvidiaGPUFlag
	}

	// setup cron
	{
		crontab := cron.New(cron.WithSeconds())

		// schedule async v2job to get v2 appstore list
		go func() {
			// run once at startup
			if err := service.MyService.AppStoreManagement().UpdateCatalog(); err != nil {
				logger.Error("error when updating AppStore catalog at startup", zap.Error(err))
			}
		}()

		if _, err := crontab.AddFunc("@every 10m", func() {
			if err := service.MyService.AppStoreManagement().UpdateCatalog(); err != nil {
				logger.Error("error when updating AppStore catalog", zap.Error(err))
			}
		}); err != nil {
			panic(err)
		}

		// start apps that were abandoned because the default storage was not
		// ready when docker first tried to start them at boot
		if _, err := crontab.AddFunc("@every 15s", func() {
			service.MyService.Compose().RecoverAppsWaitingForStorage(ctx)
		}); err != nil {
			panic(err)
		}

		// Ask the registries what the installed images point at now. Hours, not
		// minutes: this is one round trip per distinct image, registries rate-limit
		// anonymous callers by IP, and an image that moved an hour ago is news that
		// keeps. Checking only ever fills in a badge -- nothing here updates anything.
		go func() {
			// not at startup proper: docker, the network and the catalogue this
			// compares against are all still coming up, and an answer nobody is
			// looking at yet can wait for them
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Minute):
			}

			if _, err := service.CheckImageUpdates(ctx); err != nil {
				logger.Error("error when checking for image updates at startup", zap.Error(err))
			}
		}()

		if _, err := crontab.AddFunc("@every 6h", func() {
			if _, err := service.CheckImageUpdates(ctx); err != nil {
				logger.Error("error when checking for image updates", zap.Error(err))
			}
		}); err != nil {
			panic(err)
		}

		// Apps deployed from git: where each branch is now, and what the automatic rebuild
		// may deploy. One ls-remote per app, which any forge answers in a second.
		if _, err := crontab.AddFunc("@every 5m", func() {
			service.CheckGitApps(ctx)
		}); err != nil {
			panic(err)
		}

		// Backups that go off by themselves. A minute because the smallest thing
		// anyone can schedule is a time of day, and a tick that finds nothing due
		// costs one file read.
		backupRunner := service.NewScheduledBackupRunner()
		if _, err := crontab.AddFunc("@every 1m", func() {
			service.RunDueBackups(ctx, time.Now(), backupRunner)
		}); err != nil {
			panic(err)
		}

		// The newest backups read back, every few days, when the owner switched it on. A job of
		// its own, so a verification -- up to an hour each -- never holds up the backups' tick;
		// the tour takes a lock of its own, as cron starts one a minute whether the last ended.
		if _, err := crontab.AddFunc("@every 1m", func() {
			service.RunDueVerifications(ctx, time.Now(), rclone.NewClient())
		}); err != nil {
			panic(err)
		}

		// The repositories kept with restic, looked at once a week. An hour because that is how
		// soon a check that found a fault, or a destination just switched, is picked up; what is
		// not due costs one file read.
		resticChecker := service.NewResticChecker()
		if _, err := crontab.AddFunc("@every 1h", func() {
			service.RunDueResticChecks(ctx, time.Now(), resticChecker)
		}); err != nil {
			panic(err)
		}

		// An app's container that crashes, turns unhealthy or keeps restarting, told on
		// the message bus for the core's alerts. Docker's own events rather than a tick:
		// a crash is news the moment it happens.
		go service.WatchDocker(ctx)

		crontab.Start()
		defer crontab.Stop()

	}

	// register at message bus
	{
		response, err := service.MyService.MessageBus().RegisterEventTypesWithResponse(ctx, common.EventTypes)
		if err != nil {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.Error(err))
		}

		if response != nil && response.StatusCode() != http.StatusOK {
			logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.String("status", response.Status()), zap.String("body", string(response.Body)))
		}
	}

	// Every container's CPU, memory, network and disk, every five seconds, and the
	// resource alerts on the message bus. Started once the event types are registered:
	// the events rebuilt from the history at start are published at once. A stop loses
	// at most the minute in progress: the history files are crash-safe.
	if err := service.StartStats(ctx); err != nil {
		logger.Error("the stats sampler did not start, the stats routes answer 503", zap.Error(err))
	}

	// Every app container's lines and events, kept on /DATA. The store opens in the
	// background: /DATA may not be mounted yet.
	dockerClient, err := client2.NewClientWithOpts(client2.FromEnv, client2.WithAPIVersionNegotiation())
	if err == nil {
		err = service.StartLogs(ctx, dockerClient)
	}
	if err != nil {
		logger.Error("the logs collector did not start, the logs routes answer 503", zap.Error(err))
	}

	// setup listener
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic(err)
	}

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "app-management.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// initialize routers and register at gateway
	{
		apiPaths := []string{
			"/v1/apps",
			"/v1/container",
			"/v1/app-categories",
			route.V1DocPath,
			route.V2APIPath,
			route.V2DocPath,
		}

		for _, apiPath := range apiPaths {
			if err := service.MyService.Gateway().CreateRoute(&model.Route{
				Path:   apiPath,
				Target: "http://" + listener.Addr().String(),
			}); err != nil {
				panic(err)
			}
		}
	}

	// a move of Docker's data cut short by a stop of this service
	v1.RecoverDockerLocation()

	v1Router := route.InitV1Router()
	v2Router := route.InitV2Router()
	v1DocRouter := route.InitV1DocRouter(_docHTMLV1, _docYAMLV1)
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v1":    v1Router,
			"v2":    v2Router,
			"v1doc": v1DocRouter,
			"doc":   v2DocRouter,
		},
	}

	// notify systemd that we are ready
	{
		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that casaos main service is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that casaos main service is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}

		logger.Info("App management service is listening...", zap.Any("address", listener.Addr().String()))
	}

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil {
		panic(err)
	}
}
