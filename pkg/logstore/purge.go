package logstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FileInfo struct {
	Path string
	Day  time.Time // UTC midnight parsed from the name; zero for a file the layout does not know
	Size int64
}

// ListFiles returns every file under root; only <app>/<service>/<day>.*.log and _events/<day>.jsonl get a Day.
// A missing root is an empty store.
func ListFiles(root string) ([]FileInfo, error) {
	var out []FileInfo
	err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if de.IsDir() {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, FileInfo{Path: p, Day: dayOf(root, p), Size: info.Size()})
		return nil
	})
	return out, err
}

func dayOf(root, p string) time.Time {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return time.Time{}
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	name := parts[len(parts)-1]
	switch {
	case len(parts) == 2 && parts[0] == "_events" && strings.HasSuffix(name, ".jsonl") && len(name) == len("2006-01-02.jsonl"):
	case len(parts) == 3 && parts[0] != "_events" && strings.HasSuffix(name, ".log") && len(name) > 11 && name[10] == '.':
	default:
		return time.Time{}
	}
	day, err := time.Parse("2006-01-02", name[:10])
	if err != nil {
		return time.Time{}
	}
	return day
}

// PlanPurge returns the paths to remove: days older than the retention window, then whole oldest days
// until the rest fits maxBytes. Files with a zero Day are never planned. retentionDays<=0 and maxBytes<=0 disable their rule.
func PlanPurge(files []FileInfo, now time.Time, retentionDays int, maxBytes int64) []string {
	var plan []string
	var kept []FileInfo
	// retentionDays=7 keeps today and the 6 days before: a day <= today-7 goes.
	cutoff := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -retentionDays)
	for _, f := range files {
		if retentionDays > 0 && !f.Day.IsZero() && !f.Day.After(cutoff) {
			plan = append(plan, f.Path)
		} else {
			kept = append(kept, f)
		}
	}
	if maxBytes <= 0 {
		return plan
	}
	var total int64
	for _, f := range kept {
		total += f.Size
	}
	var dated []FileInfo
	for _, f := range kept {
		if !f.Day.IsZero() {
			dated = append(dated, f)
		}
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].Day.Before(dated[j].Day) })
	for i := 0; total > maxBytes && i < len(dated); i++ {
		day := dated[i].Day
		for ; i < len(dated) && dated[i].Day.Equal(day); i++ { // never a day partially
			plan = append(plan, dated[i].Path)
			total -= dated[i].Size
		}
		i--
	}
	return plan
}

// Purge applies PlanPurge to the store on disk, drops <app>/<service> and <app> directories it emptied,
// and returns the bytes of the files removed.
func Purge(root string, now time.Time, retentionDays int, maxBytes int64) (removed int64, err error) {
	files, err := ListFiles(root)
	if err != nil {
		return 0, err
	}
	size := make(map[string]int64, len(files))
	for _, f := range files {
		size[f.Path] = f.Size
	}
	return removePlanned(root, PlanPurge(files, now, retentionDays, maxBytes), size)
}

// removePlanned keeps going past a failing remove so one stuck file cannot stop the cap; it returns the first error.
func removePlanned(root string, plan []string, size map[string]int64) (removed int64, first error) {
	for _, p := range plan {
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) && first == nil {
				first = err
			}
			continue
		}
		removed += size[p]
		// only <app>/<service>/file qualifies; os.Remove refuses a non-empty directory, which is the check we want.
		if rel, _ := filepath.Rel(root, p); strings.Count(filepath.ToSlash(rel), "/") == 2 {
			svc := filepath.Dir(p)
			if os.Remove(svc) == nil {
				os.Remove(filepath.Dir(svc))
			}
		}
	}
	return removed, first
}

func Usage(root string) (bytes int64, oldest time.Time, err error) {
	files, err := ListFiles(root)
	for _, f := range files {
		bytes += f.Size
		if !f.Day.IsZero() && (oldest.IsZero() || f.Day.Before(oldest)) {
			oldest = f.Day
		}
	}
	return bytes, oldest, err
}
