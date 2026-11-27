package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// MoveMode selects which movable jobs are relocated to a user-chosen directory.
type MoveMode uint8

const (
	MoveSelected MoveMode = iota + 1
	MoveOversize
	MoveFailed
	MoveSent
	MoveAttention
	MoveAllMovable
)

// IsMovableState reports whether a job in state can safely be moved to another
// folder. Active or uncertain (confirming) entries are excluded so no file in
// flight or waiting for manual verification disappears unexpectedly.
func IsMovableState(state model.JobState) bool {
	switch state {
	case model.JobOversize,
		model.JobFailed,
		model.JobCancelled,
		model.JobSkipped,
		model.JobInterrupted,
		model.JobQueued,
		model.JobSent,
		model.JobMoved:
		return true
	default:
		return false
	}
}

func matchesMoveMode(job model.Job, mode MoveMode, selected map[string]struct{}) bool {
	if !IsMovableState(job.State) {
		return false
	}
	switch mode {
	case MoveSelected:
		_, ok := selected[job.ID]
		return ok
	case MoveOversize:
		return job.State == model.JobOversize
	case MoveFailed:
		return job.State == model.JobFailed
	case MoveSent:
		return job.State == model.JobSent
	case MoveAttention:
		switch job.State {
		case model.JobOversize, model.JobFailed, model.JobCancelled, model.JobSkipped:
			return true
		}
		return false
	case MoveAllMovable:
		return true
	default:
		return false
	}
}

func canonicalDestKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func emptyMoveError(mode MoveMode) error {
	switch mode {
	case MoveOversize:
		return errors.New("没有超限文件需要移动")
	case MoveFailed:
		return errors.New("没有失败任务需要移动")
	default:
		return errors.New("没有符合条件的可移动文件")
	}
}

// MoveOversize moves every oversize video in the queue into destinationDir.
func (c *Controller) MoveOversize(ctx context.Context, destinationDir string, onProgress func(model.Progress)) error {
	_, err := c.MoveJobs(ctx, MoveOversize, nil, destinationDir, onProgress)
	return err
}

// MoveJobs moves queued or completed video files matching mode (and ids for
// MoveSelected) into destinationDir. It returns the number of moved files.
func (c *Controller) MoveJobs(
	ctx context.Context,
	mode MoveMode,
	ids []string,
	destinationDir string,
	onProgress func(model.Progress),
) (int, error) {
	if !c.opMu.TryLock() {
		return 0, errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	directoryInfo, err := os.Stat(destinationDir)
	if err != nil {
		return 0, fmt.Errorf("读取目标文件夹失败：%w", err)
	}
	if !directoryInfo.IsDir() {
		return 0, errors.New("目标路径不是文件夹")
	}

	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			selected[id] = struct{}{}
		}
	}

	c.mu.RLock()
	if c.running {
		c.mu.RUnlock()
		return 0, errors.New("上传进行中，不能移动文件")
	}
	type item struct {
		index       int
		job         model.Job
		destination string
	}
	var items []item
	var total int64
	seenDest := make(map[string]struct{})
	for i, job := range c.jobs {
		if _, pending := c.pendingRemoval[job.ID]; pending {
			continue
		}
		if !matchesMoveMode(job, mode, selected) {
			continue
		}
		destination, pathErr := safeMoveDestination(destinationDir, job.Name)
		if pathErr != nil {
			c.mu.RUnlock()
			return 0, pathErr
		}
		destKey := canonicalDestKey(destination)
		if _, dup := seenDest[destKey]; dup {
			c.mu.RUnlock()
			return 0, fmt.Errorf("目标文件夹里存在重名文件冲突，不会覆盖：%s", job.Name)
		}
		seenDest[destKey] = struct{}{}
		if canonicalDestKey(job.Path) == destKey {
			// Already at the destination folder.
			continue
		}
		if _, statErr := os.Lstat(destination); statErr == nil {
			c.mu.RUnlock()
			return 0, fmt.Errorf("目标文件已存在，不会覆盖：%s", destination)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			c.mu.RUnlock()
			return 0, fmt.Errorf("检查目标文件失败：%w", statErr)
		}
		items = append(items, item{index: i, job: job, destination: destination})
		total += job.Size
	}
	c.mu.RUnlock()
	if len(items) == 0 {
		return 0, emptyMoveError(mode)
	}

	destFolder := filepath.Base(filepath.Clean(destinationDir))
	moved := 0
	var offset int64
	for _, current := range items {
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		info, err := os.Lstat(current.job.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != current.job.Size || !info.ModTime().Equal(current.job.ModTime) {
			if err == nil {
				err = errors.New("文件已被替换或修改")
			}
			return moved, fmt.Errorf("移动前检查失败：%w", err)
		}

		// Persist the move intent and original state before touching the source.
		// If the process stops after the filesystem move but before the final save,
		// Load deterministically recovers by checking source and destination.
		c.mu.Lock()
		if current.index >= len(c.jobs) || c.jobs[current.index].ID != current.job.ID || c.jobs[current.index].State != current.job.State {
			c.mu.Unlock()
			return moved, errors.New("队列已发生变化，请重新尝试移动")
		}
		c.jobs[current.index].State = model.JobMoving
		c.jobs[current.index].MoveDestination = current.destination
		c.jobs[current.index].MoveFromState = current.job.State
		c.jobs[current.index].Error = ""
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.mu.Lock()
			if current.index < len(c.jobs) && c.jobs[current.index].ID == current.job.ID {
				c.jobs[current.index].State = current.job.State
				c.jobs[current.index].MoveDestination = ""
				c.jobs[current.index].MoveFromState = ""
				c.queueRevision++
				c.notifyLocked()
			}
			c.mu.Unlock()
			return moved, fmt.Errorf("保存移动意图失败：%w", err)
		}

		itemOffset := offset
		err = c.mover.Move(ctx, current.job.Path, current.destination, func(progress model.Progress) {
			if onProgress != nil {
				progress.BytesDone += itemOffset
				progress.BytesTotal = total
				onProgress(progress)
			}
		})
		if err != nil {
			c.mu.Lock()
			if current.index < len(c.jobs) && c.jobs[current.index].ID == current.job.ID {
				c.jobs[current.index].State = current.job.State
				c.jobs[current.index].MoveDestination = ""
				c.jobs[current.index].MoveFromState = ""
				c.jobs[current.index].Error = err.Error()
				c.queueRevision++
				c.notifyLocked()
			}
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				return moved, errors.Join(err, fmt.Errorf("保存移动失败状态失败：%w", persistErr))
			}
			return moved, err
		}
		offset += current.job.Size
		moved++
		now := time.Now()
		c.mu.Lock()
		if current.index < len(c.jobs) && c.jobs[current.index].ID == current.job.ID {
			c.jobs[current.index].State = model.JobMoved
			c.jobs[current.index].Path = current.destination
			c.jobs[current.index].MoveDestination = ""
			c.jobs[current.index].MoveFromState = ""
			c.jobs[current.index].CompletedAt = &now
			c.jobs[current.index].Error = ""
			c.jobs[current.index].GroupID = ""
			c.jobs[current.index].Staged = nil
			clearResume(&c.jobs[current.index])
			c.queueRevision++
			c.notifyLocked()
		}
		c.mu.Unlock()
		_ = diagnostics.Logf("job_moved id=%s name=%q dest=%q", current.job.ID, current.job.Name, current.destination)
		c.emitLog(model.LogEntry{
			TimeMs:  now.UnixMilli(),
			Kind:    "moved",
			JobID:   current.job.ID,
			Name:    current.job.Name,
			Size:    current.job.Size,
			Summary: "已移动到 " + destFolder,
		})
		if err := c.persist(); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

func safeMoveDestination(destinationDir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("不安全的文件名，拒绝移动：%q", name)
	}
	directory, err := filepath.Abs(destinationDir)
	if err != nil {
		return "", fmt.Errorf("解析目标文件夹失败：%w", err)
	}
	destination := filepath.Join(directory, name)
	relative, err := filepath.Rel(directory, destination)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(os.PathSeparator) {
		return "", fmt.Errorf("文件名会逃逸目标文件夹，拒绝移动：%q", name)
	}
	return destination, nil
}
