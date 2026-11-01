package service

import (
	"path/filepath"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
	"github.com/ReCasaOS/CasaOS-Common/utils/constants"
)

// ApplyDataPath points stats, git-app state, backups and image-update
// cache at root. Empty root is /var/lib/casaos. gitAppsDataRoot and
// os-upgrade/busy stay on the box paths. restic.KeyDir follows root
// (same path as the packaged default). restic.CacheDir stays
// /var/cache/casaos/restic unless root is not the default.
func ApplyDataPath(root string) string {
	if root == "" {
		root = constants.DefaultDataPath
	}
	StatsDir = filepath.Join(root, "stats", "app_management")
	gitAppsDir = filepath.Join(root, "git_apps")
	BackupLogPath = filepath.Join(root, "backups", "runs.json")
	BackupHoldPath = filepath.Join(root, "backups", "holding.json")
	BackupSchedulePath = filepath.Join(root, "backups", "schedules.json")
	imageUpdateStatePath = filepath.Join(root, "image_updates.json")
	restic.KeyDir = filepath.Join(root, "backups", "restic")
	if root == constants.DefaultDataPath {
		restic.CacheDir = "/var/cache/casaos/restic"
	} else {
		restic.CacheDir = filepath.Join(root, "cache", "restic")
	}
	return root
}

func ApplyGitAppsDataRoot(v string) {
	if v == "" {
		gitAppsDataRoot = "/DATA/AppData"
		return
	}
	gitAppsDataRoot = v
}
