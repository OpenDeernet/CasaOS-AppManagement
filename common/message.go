package common

import (
	"github.com/ReCasaOS/CasaOS-AppManagement/codegen/message_bus"
	"github.com/ReCasaOS/CasaOS-Common/utils"
)

// common properties
var (
	PropertyTypeMessage = message_bus.PropertyType{
		Name:        "message",
		Description: utils.Ptr("message at different levels, typically for error"),
	}
)

// app properties
var (
	PropertyTypeAppName = message_bus.PropertyType{
		Name:        "app:name",
		Description: utils.Ptr("name of the app which could be a container image name including version, a snap name or the name of any other forms of app"),
		Example:     utils.Ptr("hello-world:latest (this is the name of a container image"),
	}

	PropertyTypeAppTitle = message_bus.PropertyType{
		Name:        "app:title",
		Description: utils.Ptr("titles of the app in different languages - serialized as JSON"),
		Example:     utils.Ptr("{\"en_us\":\"OpenSpeedTest\"}"),
	}

	PropertyTypeAppIcon = message_bus.PropertyType{
		Name:        "app:icon",
		Description: utils.Ptr("icon of the app"),
		Example:     utils.Ptr("https://example.com/icon.png"),
	}

	PropertyTypeAppProgress = message_bus.PropertyType{
		Name:        "app:progress",
		Description: utils.Ptr("progress of the app"),
		Example:     utils.Ptr("64"),
	}

	PropertyTypeCheckPortConflict = message_bus.PropertyType{
		Name:        "check_port_conflict",
		Description: utils.Ptr("todo: the field should be remove in future. it is not need for most message"),
		Example:     utils.Ptr("true"),
	}

	PropertyTypeDryRun = message_bus.PropertyType{
		Name:        "dry_run",
		Description: utils.Ptr("todo: the field should be remove in future. it is not need for most message"),
		Example:     utils.Ptr("false"),
	}
)

// container properties
var (
	PropertyTypeContainerID = message_bus.PropertyType{
		Name:        "docker:container:id",
		Description: utils.Ptr("ID of the container"),
		Example:     utils.Ptr("855084f79fc89bea4de5111c69621b3329ecf0a1106863a7a83bbdef01d33b9e"),
	}

	PropertyTypeContainerName = message_bus.PropertyType{
		Name:        "docker:container:name",
		Description: utils.Ptr("name of the container"),
		Example:     utils.Ptr("hello-world"),
	}

	PropertyTypeContainerExitCode = message_bus.PropertyType{
		Name:        "docker:container:exit-code",
		Description: utils.Ptr("exit code of the container's main process, as Docker reports it"),
		Example:     utils.Ptr("1"),
	}
)

// image properties
var (
	PropertyTypeImageName = message_bus.PropertyType{
		Name:        "docker:image:name",
		Description: utils.Ptr("name of the image"),
		Example:     utils.Ptr("hello-world:latest"),
	}

	PropertyTypeImageUpdated = message_bus.PropertyType{
		Name:        "docker:image:updated",
		Description: utils.Ptr("true if a newer image was pulled, false if the check found none. Absent when no check completed: a failed pull says nothing either way."),
	}
)

// backup properties
var (
	PropertyTypeBackupDestination = message_bus.PropertyType{
		Name:        "backup:destination",
		Description: utils.Ptr("name of the destination a backup goes to or comes from"),
		Example:     utils.Ptr("offsite"),
	}
	PropertyTypeBackupStamp = message_bus.PropertyType{
		Name:        "backup:stamp",
		Description: utils.Ptr("which run, as the run log names it"),
		Example:     utils.Ptr("2026-09-13T02-29-56Z"),
	}
	// PropertyTypeBackupKind tells a backup from a restore: the same four events
	// carry both, since a dashboard shows both the same way.
	PropertyTypeBackupKind = message_bus.PropertyType{
		Name:        "backup:kind",
		Description: utils.Ptr("`backup` or `restore`"),
		Example:     utils.Ptr("backup"),
	}
)

// app update properties
var (
	// PropertyTypeAppUpdated is set only when an update actually replaced what the app
	// runs. Absent means it did not: nothing newer, nothing checked, or it failed --
	// app:update-error carries the reason when there is one.
	PropertyTypeAppUpdated = message_bus.PropertyType{
		Name:        "app:updated",
		Description: utils.Ptr("true if the app now runs what the update installed"),
	}
)

var EventTypes = []message_bus.EventType{
	// app-store
	EventTypeAppStoreRegisterBegin, EventTypeAppStoreRegisterEnd, EventTypeAppStoreRegisterError,

	// app
	EventTypeAppInstallBegin, EventTypeAppInstallProgress, EventTypeAppInstallEnd, EventTypeAppInstallError,
	EventTypeAppUninstallBegin, EventTypeAppUninstallEnd, EventTypeAppUninstallError,
	EventTypeAppUpdateBegin, EventTypeAppUpdateEnd, EventTypeAppUpdateError,
	EventTypeBackupBegin, EventTypeBackupProgress, EventTypeBackupEnd, EventTypeBackupError, EventTypeBackupCheckError,
	EventTypeAppApplyChangesBegin, EventTypeAppApplyChangesEnd, EventTypeAppApplyChangesError,
	EventTypeAppStartBegin, EventTypeAppStartEnd, EventTypeAppStartError,
	EventTypeAppStopBegin, EventTypeAppStopEnd, EventTypeAppStopError,
	EventTypeAppRestartBegin, EventTypeAppRestartEnd, EventTypeAppRestartError,
	EventTypeAppGitBuildBegin, EventTypeAppGitBuildProgress, EventTypeAppGitBuildEnd, EventTypeAppGitBuildError,
	EventTypeAppGitDeployEnd, EventTypeAppGitDeployError,
	EventTypeAppContainerDied, EventTypeAppContainerUnhealthy, EventTypeAppContainerRestarting, EventTypeAppContainerHealthy,

	// resources
	EventTypeResourceAlertFiring, EventTypeResourceAlertResolved,

	// image
	EventTypeImagePullBegin, EventTypeImagePullProgress, EventTypeImagePullEnd, EventTypeImagePullError,

	// container
	EventTypeContainerCreateBegin, EventTypeContainerCreateEnd, EventTypeContainerCreateError,
	EventTypeContainerStartBegin, EventTypeContainerStartEnd, EventTypeContainerStartError,
	EventTypeContainerStopBegin, EventTypeContainerStopEnd, EventTypeContainerStopError,
	EventTypeContainerRenameBegin, EventTypeContainerRenameEnd, EventTypeContainerRenameError,
	EventTypeContainerRemoveBegin, EventTypeContainerRemoveEnd, EventTypeContainerRemoveError,
}

// event types for app-store
var (
	EventTypeAppStoreRegisterBegin = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app-store:register-begin",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppStoreRegisterEnd = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app-store:register-end",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppStoreRegisterError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app-store:register-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
		},
	}
)

// event types for app
var (
	EventTypeAppInstallBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
		},
	}

	EventTypeAppInstallProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeAppProgress,
			PropertyTypeAppTitle,
			PropertyTypeCheckPortConflict,
			PropertyTypeDryRun,
		},
	}

	EventTypeAppInstallEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
		},
	}

	EventTypeAppInstallError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:install-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeAppIcon,
			PropertyTypeMessage,
		},
	}

	EventTypeAppUninstallBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppUninstallEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppUninstallError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:uninstall-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppUpdateBegin = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app:update-begin",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppUpdateEnd = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app:update-end",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppUpdateError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:update-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
		},
	}

	// A backup or a restore, from start to end. Progress is one event per
	// operation rather than per byte: what a person watching wants to know is
	// which folder it is on and how many are left, and rclone's own transfer
	// statistics are a job away for anyone who wants bytes.
	EventTypeBackupBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "backup:begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName, PropertyTypeAppTitle, PropertyTypeAppIcon,
			PropertyTypeBackupDestination, PropertyTypeBackupStamp, PropertyTypeBackupKind,
		},
	}

	EventTypeBackupProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "backup:progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName, PropertyTypeAppTitle, PropertyTypeAppIcon,
			PropertyTypeBackupDestination, PropertyTypeBackupStamp, PropertyTypeBackupKind,
			PropertyTypeAppProgress, PropertyTypeMessage,
		},
	}

	EventTypeBackupEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "backup:end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName, PropertyTypeAppTitle, PropertyTypeAppIcon,
			PropertyTypeBackupDestination, PropertyTypeBackupStamp, PropertyTypeBackupKind,
		},
	}

	EventTypeBackupError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "backup:error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName, PropertyTypeAppTitle, PropertyTypeAppIcon,
			PropertyTypeBackupDestination, PropertyTypeBackupStamp, PropertyTypeBackupKind,
			PropertyTypeMessage,
		},
	}

	// A restic repository that does not check out. Its own event rather than a backup
	// error: that one has an app, and the dashboard names the app in its toast.
	EventTypeBackupCheckError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "backup:check-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeBackupDestination, PropertyTypeMessage,
		},
	}

	EventTypeAppApplyChangesBegin = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app:apply-changes-begin",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppApplyChangesEnd = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "app:apply-changes-end",
		PropertyTypeList: []message_bus.PropertyType{},
	}

	EventTypeAppApplyChangesError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:apply-changes-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeMessage,
		},
	}

	EventTypeAppStartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:start-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppStopBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStopEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppStopError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:stop-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppRestartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppRestartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppRestartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:restart-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}
)

// event types for apps deployed from their git repository. A build says what it is doing
// line by line in progress events; a deployment says only how it ended.
var (
	EventTypeAppGitBuildBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-build-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppGitBuildProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-build-progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppGitBuildEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-build-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppGitBuildError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-build-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeAppGitDeployEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-deploy-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeAppGitDeployError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:git-deploy-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}
)

// event types for what happens to an app's containers when nobody asked for it, from the
// Docker watch. A container that crashed, turned unhealthy or keeps restarting says
// healthy once it has run for ten minutes without being unhealthy.
var (
	EventTypeAppContainerDied = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:container-died",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeContainerName,
			PropertyTypeContainerExitCode,
		},
	}

	EventTypeAppContainerUnhealthy = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:container-unhealthy",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeContainerName,
		},
	}

	EventTypeAppContainerRestarting = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:container-restarting",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeContainerName,
		},
	}

	EventTypeAppContainerHealthy = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "app:container-healthy",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeContainerName,
		},
	}
)

// resource alert properties: plain names, the same in every service that publishes them
var (
	PropertyTypeResourceItem = message_bus.PropertyType{
		Name:        "item",
		Description: utils.Ptr("stats id of the container"),
		Example:     utils.Ptr("app:jarvis:web:1"),
	}
	PropertyTypeResourceName = message_bus.PropertyType{
		Name:        "name",
		Description: utils.Ptr("what to call the container: `<application> / <service>` for an app container, its name for any other"),
		Example:     utils.Ptr("jarvis / web"),
	}
	PropertyTypeResourceKind = message_bus.PropertyType{
		Name:        "kind",
		Description: utils.Ptr("`app-container` or `container`"),
		Example:     utils.Ptr("app-container"),
	}
	PropertyTypeResourceSituation = message_bus.PropertyType{
		Name:        "situation",
		Description: utils.Ptr("`cpu_high` or `mem_high`"),
		Example:     utils.Ptr("cpu_high"),
	}
	PropertyTypeResourceValue = message_bus.PropertyType{
		Name:        "value",
		Description: utils.Ptr("percent that fired the alert, an integer; absent when resolved"),
		Example:     utils.Ptr("93"),
	}
	PropertyTypeResourceMinutes = message_bus.PropertyType{
		Name:        "minutes",
		Description: utils.Ptr("window of the rule in minutes"),
		Example:     utils.Ptr("10"),
	}
	PropertyTypeResourceSince = message_bus.PropertyType{
		Name:        "since",
		Description: utils.Ptr("when the alert fired, RFC 3339; a resolved event carries the time it fired"),
		Example:     utils.Ptr("2026-10-20T10:00:00Z"),
	}
	PropertyTypeResourceSource = message_bus.PropertyType{
		Name:        "source",
		Description: utils.Ptr("service that measured it: `app_management`"),
		Example:     utils.Ptr("app_management"),
	}

	resourceAlertProperties = []message_bus.PropertyType{
		PropertyTypeResourceItem, PropertyTypeResourceName, PropertyTypeResourceKind, PropertyTypeResourceSituation,
		PropertyTypeResourceValue, PropertyTypeResourceMinutes, PropertyTypeResourceSince, PropertyTypeResourceSource,
	}
)

// event types for a container's resources held too high for too long, from the stats
// sampler: one firing, then one resolved.
var (
	EventTypeResourceAlertFiring = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "resource:alert-firing",
		PropertyTypeList: resourceAlertProperties,
	}

	EventTypeResourceAlertResolved = message_bus.EventType{
		SourceID:         AppManagementServiceName,
		Name:             "resource:alert-resolved",
		PropertyTypeList: resourceAlertProperties,
	}
)

// event types for image
var (
	EventTypeImagePullBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeImagePullProgress = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-progress",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeImagePullEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeImageUpdated,
		},
	}

	EventTypeImagePullError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:pull-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}

	EventTypeImageRemoveBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeImageRemoveEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
		},
	}

	EventTypeImageRemoveError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:image:remove-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeAppName,
			PropertyTypeMessage,
		},
	}
)

// event types for container
var (
	EventTypeContainerCreateBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerName,
		},
	}

	EventTypeContainerCreateEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
		},
	}

	EventTypeContainerCreateError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:create-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerName,
			PropertyTypeMessage,
		},
	}

	EventTypeContainerStartBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerStartEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerStartError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:start-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
		},
	}

	EventTypeContainerStopBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerStopEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerStopError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:stop-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
		},
	}

	EventTypeContainerRenameBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
		},
	}

	EventTypeContainerRenameEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
		},
	}

	EventTypeContainerRenameError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:rename-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeContainerName,
			PropertyTypeMessage,
		},
	}

	EventTypeContainerRemoveBegin = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-begin",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerRemoveEnd = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-end",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
		},
	}

	EventTypeContainerRemoveError = message_bus.EventType{
		SourceID: AppManagementServiceName,
		Name:     "docker:container:remove-error",
		PropertyTypeList: []message_bus.PropertyType{
			PropertyTypeContainerID,
			PropertyTypeMessage,
		},
	}
)
