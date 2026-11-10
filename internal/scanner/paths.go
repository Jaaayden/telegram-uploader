package scanner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// PathsResult is the outcome of scanning a mixed list of files and folders.
type PathsResult struct {
	Jobs []model.Job
	// Ignored counts entries that were given explicitly but are not regular
	// MP4 files (other formats, symlinks, entries that vanished).
	Ignored int
}

// ScanPaths builds jobs from files and folders chosen or dropped together.
// A folder contributes its top-level MP4 files, exactly like Scan; a file is
// taken as it is. The same file reached twice (dropped twice, or together with
// its folder) yields one job. Jobs are ordered by folder, then naturally by
// name, so the result does not depend on the order the paths arrived in.
func ScanPaths(paths []string, maxBytes int64) (PathsResult, error) {
	var result PathsResult
	var files []fileEntry
	seen := make(map[string]struct{})
	add := func(entry fileEntry) {
		key := pathKey(entry.path)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		files = append(files, entry)
	}

	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		path = filepath.Clean(path)
		// Lstat on purpose: a symlink is never followed, as in Scan.
		info, err := lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			result.Ignored++
			continue
		}
		if err != nil {
			return PathsResult{}, fmt.Errorf("inspect %q: %w", path, err)
		}
		switch {
		case info.IsDir():
			jobs, err := Scan(path, 0)
			if err != nil {
				return PathsResult{}, err
			}
			for _, job := range jobs {
				add(fileEntry{name: job.Name, path: job.Path, size: job.Size, modTime: job.ModTime})
			}
		case info.Mode().IsRegular() && isMP4(info.Name()):
			add(fileEntry{name: info.Name(), path: path, size: info.Size(), modTime: info.ModTime()})
		default:
			result.Ignored++
		}
	}

	sort.SliceStable(files, func(i, j int) bool {
		left, right := filepath.Dir(files[i].path), filepath.Dir(files[j].path)
		if left != right {
			return compareNatural(left, right) < 0
		}
		return compareNatural(files[i].name, files[j].name) < 0
	})

	result.Jobs = make([]model.Job, len(files))
	for position, file := range files {
		id, randomID, err := newIDs()
		if err != nil {
			return PathsResult{}, fmt.Errorf("generate job identity for %q: %w", file.path, err)
		}
		state := model.JobQueued
		if maxBytes > 0 && file.size > maxBytes {
			state = model.JobOversize
		}
		result.Jobs[position] = model.Job{
			ID: id, Position: position, Path: file.path, Name: file.name,
			Size: file.size, ModTime: file.modTime, State: state, RandomID: randomID,
		}
	}
	return result, nil
}

func pathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
