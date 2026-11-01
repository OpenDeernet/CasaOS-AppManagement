package service

import (
	"path/filepath"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/restic"
	"github.com/ReCasaOS/CasaOS-Common/utils/constants"
)

func TestApplyDataPath(t *testing.T) {
	prevStats, prevGit, prevLog, prevHold, prevSched, prevImg := StatsDir, gitAppsDir, BackupLogPath, BackupHoldPath, BackupSchedulePath, imageUpdateStatePath
	prevKey, prevCache, prevChecks := restic.KeyDir, restic.CacheDir, ResticCheckPath
	t.Cleanup(func() {
		StatsDir, gitAppsDir, BackupLogPath, BackupHoldPath, BackupSchedulePath, imageUpdateStatePath = prevStats, prevGit, prevLog, prevHold, prevSched, prevImg
		restic.KeyDir, restic.CacheDir, ResticCheckPath = prevKey, prevCache, prevChecks
	})

	root := t.TempDir()
	got := ApplyDataPath(root)
	if got != root {
		t.Fatalf("ApplyDataPath = %q, want %q", got, root)
	}
	want := map[string]string{
		"StatsDir":             filepath.Join(root, "stats", "app_management"),
		"gitAppsDir":           filepath.Join(root, "git_apps"),
		"BackupLogPath":        filepath.Join(root, "backups", "runs.json"),
		"BackupHoldPath":       filepath.Join(root, "backups", "holding.json"),
		"BackupSchedulePath":   filepath.Join(root, "backups", "schedules.json"),
		"imageUpdateStatePath": filepath.Join(root, "image_updates.json"),
		"ResticCheckPath":      filepath.Join(root, "backups", "restic-checks.json"),
	}
	gotPaths := map[string]string{
		"StatsDir":             StatsDir,
		"gitAppsDir":           gitAppsDir,
		"BackupLogPath":        BackupLogPath,
		"BackupHoldPath":       BackupHoldPath,
		"BackupSchedulePath":   BackupSchedulePath,
		"imageUpdateStatePath": imageUpdateStatePath,
		"ResticCheckPath":      ResticCheckPath,
	}
	for name, w := range want {
		if gotPaths[name] != w {
			t.Errorf("%s = %q, want %q", name, gotPaths[name], w)
		}
	}
	if gitAppsDataRoot != "/DATA/AppData" {
		t.Errorf("gitAppsDataRoot changed: %q", gitAppsDataRoot)
	}
	if osUpgradeBusyPath != "/var/lib/casaos/os-upgrade/busy" {
		t.Errorf("osUpgradeBusyPath changed: %q", osUpgradeBusyPath)
	}
	if restic.KeyDir != filepath.Join(root, "backups", "restic") {
		t.Errorf("KeyDir = %q, want under root", restic.KeyDir)
	}
	if restic.CacheDir != filepath.Join(root, "cache", "restic") {
		t.Errorf("CacheDir = %q, want under root", restic.CacheDir)
	}
}

func TestApplyDataPathEmptyUsesDefault(t *testing.T) {
	prev := StatsDir
	prevKey, prevCache, prevChecks := restic.KeyDir, restic.CacheDir, ResticCheckPath
	t.Cleanup(func() {
		StatsDir = prev
		restic.KeyDir, restic.CacheDir, ResticCheckPath = prevKey, prevCache, prevChecks
	})
	got := ApplyDataPath("")
	if got != constants.DefaultDataPath {
		t.Fatalf("root = %q, want %q", got, constants.DefaultDataPath)
	}
	want := filepath.Join(constants.DefaultDataPath, "stats", "app_management")
	if StatsDir != want {
		t.Fatalf("StatsDir = %q, want %q", StatsDir, want)
	}
	if restic.KeyDir != filepath.Join(constants.DefaultDataPath, "backups", "restic") {
		t.Fatalf("KeyDir = %q", restic.KeyDir)
	}
	if restic.CacheDir != "/var/cache/casaos/restic" {
		t.Fatalf("CacheDir = %q, want packaged default", restic.CacheDir)
	}
	if ResticCheckPath != "/var/lib/casaos/backups/restic-checks.json" {
		t.Fatalf("ResticCheckPath = %q, want the packaged default", ResticCheckPath)
	}
}

func TestApplyDataPathDefaultRestoresCacheDir(t *testing.T) {
	prevKey, prevCache := restic.KeyDir, restic.CacheDir
	t.Cleanup(func() { restic.KeyDir, restic.CacheDir = prevKey, prevCache })
	ApplyDataPath(t.TempDir())
	ApplyDataPath("")
	if restic.CacheDir != "/var/cache/casaos/restic" {
		t.Fatalf("CacheDir = %q", restic.CacheDir)
	}
}

func TestApplyGitAppsDataRoot(t *testing.T) {
	prev := gitAppsDataRoot
	t.Cleanup(func() { gitAppsDataRoot = prev })

	ApplyGitAppsDataRoot("/tmp/x")
	if gitAppsDataRoot != "/tmp/x" {
		t.Fatalf("got %q", gitAppsDataRoot)
	}
	ApplyGitAppsDataRoot("")
	if gitAppsDataRoot != "/DATA/AppData" {
		t.Fatalf("empty got %q", gitAppsDataRoot)
	}
}
