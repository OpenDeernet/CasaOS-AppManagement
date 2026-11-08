package service

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Turning an inventory into a concrete set of copies, and a manifest saying what
// they were.
//
// The layout is chosen for the restore rather than for the copy. A backup is
// written once and read on the worst day of somebody's month, by a person who does
// not remember how it was made, possibly with nothing but a file browser -- so
// every file lands somewhere a person could put back by hand, and the manifest
// says where each one came from.

// BackupOperation is one copy: one host path to one place under the backup root.
type BackupOperation struct {
	Kind BackupKind `json:"kind"`
	// Source on this host
	Source string `json:"source"`
	// Destination relative to the root of this backup, always with forward slashes
	Destination string `json:"destination"`
	// Directory is true for a folder, false for a single file. rclone copies the
	// two with different calls, and getting it wrong copies a file's parent.
	Directory bool `json:"directory"`
	// Volume is the compose name of the volume this came from, when it came from one
	Volume string `json:"volume,omitempty"`
	// Service that mounted it, and where
	Service string `json:"service,omitempty"`
	Target  string `json:"target,omitempty"`
}

// BackupManifest is written at the root of every backup, and is the only thing a
// restore needs to read.
type BackupManifest struct {
	// FormatVersion so a later reader knows what it is looking at. A backup outlives
	// the code that wrote it, which is the whole point of one.
	FormatVersion int    `json:"format_version"`
	App           string `json:"app"`
	// Operations, in the order they were planned
	Operations []BackupOperation `json:"operations"`
	// Bytes is the size of what the operations copied, measured on this host as
	// each one went: a file's size, the files under a folder added up. What the
	// destination holds may be less (restic keeps one copy of a block it has seen).
	Bytes int64 `json:"bytes,omitempty"`
	// Skipped says what was deliberately left out, with the reason, so the person
	// restoring finds out here rather than by noticing something missing later.
	Skipped []BackupEntry `json:"skipped,omitempty"`
	// ContainersStopped records whether the app was held still while this was
	// copied. A backup taken from a running database may not restore, and the only
	// thing worse than knowing that is not knowing it.
	ContainersStopped bool `json:"containers_stopped"`
	// Git is where the code of an app deployed from its repository comes from. Optional, so
	// the format version stays: an older reader ignores it.
	Git *BackupGit `json:"git,omitempty"`
}

// BackupGit is a git app's origin as a backup records it. Never a key or a token.
type BackupGit struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	// An app that follows tags: the mode, the tag the commit was deployed as, and the tag
	// filter. Omitted for a branch app, whose origin stays what older versions wrote.
	Follow      string `json:"follow,omitempty"`
	Tag         string `json:"tag,omitempty"`
	TagPattern  string `json:"tag_pattern,omitempty"`
	Prereleases bool   `json:"prereleases,omitempty"`
}

// BackupFormatVersion is bumped when the layout changes in a way a reader must
// know about.
const BackupFormatVersion = 1

// ManifestFileName sits at the root of a backup.
const ManifestFileName = "manifest.json"

// PlanBackup turns an app's inventory into the copies that would make it.
//
// `volumeMountpoints` maps a compose volume name to where Docker actually keeps
// it. A volume with no entry is one the daemon could not place, and it is moved to
// the skipped list rather than copied from a guessed path -- a guess that happens
// to name an empty folder produces a backup that reports success and restores
// nothing.
func PlanBackup(app string, inventory []BackupEntry, volumeMountpoints map[string]string) BackupManifest {
	manifest := BackupManifest{
		FormatVersion: BackupFormatVersion,
		App:           app,
		Operations:    []BackupOperation{},
		Skipped:       []BackupEntry{},
	}

	// Binds are numbered rather than named after their path: a host path is
	// arbitrary, can collide once flattened, and can contain anything a destination
	// would rather it did not. The manifest carries the real path.
	bind := 0

	for _, entry := range inventory {
		if entry.Skip != "" {
			manifest.Skipped = append(manifest.Skipped, entry)
			continue
		}

		switch entry.Kind {
		case BackupKindCompose:
			manifest.Operations = append(manifest.Operations, BackupOperation{
				Kind: entry.Kind, Source: entry.Path,
				Destination: "compose/" + path.Base(entry.Path),
			})

		case BackupKindEnv:
			manifest.Operations = append(manifest.Operations, BackupOperation{
				Kind: entry.Kind, Source: entry.Path,
				Destination: "compose/" + path.Base(entry.Path),
			})

		case BackupKindVolume:
			mountpoint, known := volumeMountpoints[entry.Volume]
			if !known || mountpoint == "" {
				entry.Skip = "Docker could not say where this volume lives"
				manifest.Skipped = append(manifest.Skipped, entry)
				continue
			}

			manifest.Operations = append(manifest.Operations, BackupOperation{
				Kind: entry.Kind, Source: mountpoint, Directory: true,
				Destination: "volumes/" + entry.Volume,
				Volume:      entry.Volume, Service: entry.Service, Target: entry.Target,
			})

		case BackupKindBind:
			bind++
			manifest.Operations = append(manifest.Operations, BackupOperation{
				Kind: entry.Kind, Source: entry.Path, Directory: true,
				Destination: fmt.Sprintf("binds/%d", bind),
				Service:     entry.Service, Target: entry.Target,
			})
		}
	}

	return manifest
}

// MarkBindFiles corrects the operations whose source turned out to be a single
// file rather than a folder.
//
// A bind mounts either, and the compose file does not say which -- `./php.ini` and
// `./config` look identical until something checks the disk. rclone needs to be
// told: a file copied as though it were a directory copies its whole parent, which
// on `/DATA/photos/one.jpg` means the entire photo library.
//
// `isFile` is passed in rather than called here so the planner stays testable
// without a filesystem.
func (m *BackupManifest) MarkBindFiles(isFile func(path string) bool) {
	for i, operation := range m.Operations {
		if operation.Kind != BackupKindBind {
			continue
		}

		if isFile(operation.Source) {
			m.Operations[i].Directory = false
			m.Operations[i].Destination = operation.Destination + "/" + path.Base(operation.Source)
		}
	}
}

// TotalBySource is what a plan addresses, deduplicated and sorted: two services
// mounting the same folder is one thing to copy, not two.
func (m *BackupManifest) TotalBySource() []string {
	seen := map[string]bool{}
	sources := []string{}

	for _, operation := range m.Operations {
		if seen[operation.Source] {
			continue
		}

		seen[operation.Source] = true
		sources = append(sources, operation.Source)
	}

	sort.Strings(sources)

	return sources
}

// RootFor is where one run of a backup goes on a destination.
//
// The app first, then the moment, so a destination holding several apps stays
// readable in a file browser and one app's history sits together. `stamp` is
// passed in rather than taken from the clock here: a plan that reads the clock
// cannot be tested, and the caller already knows when the run started.
func RootFor(app, stamp string) string {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, "/", "-")
		s = strings.ReplaceAll(s, "\\", "-")
		s = strings.ReplaceAll(s, ":", "-")

		return s
	}

	return clean(app) + "/" + clean(stamp)
}
