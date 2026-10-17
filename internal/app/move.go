package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

func (c *Controller) MoveOversize(ctx context.Context, destinationDir string, onProgress func(model.Progress)) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	directoryInfo, err := os.Stat(destinationDir)
	if err != nil {
		return fmt.Errorf("读取目标文件夹失败：%w", err)
	}
	if !directoryInfo.IsDir() {
		return errors.New("超限文件的目标路径不是文件夹")
	}

	c.mu.RLock()
	if c.running {
		c.mu.RUnlock()
		return errors.New("上传进行中，不能移动超限文件")
	}
	type item struct {
		index       int
		job         model.Job
		destination string
	}
	var items []item
	var total int64
	for i, job := range c.jobs {
		if job.State == model.JobOversize {
			destination, pathErr := safeMoveDestination(destinationDir, job.Name)
			if pathErr != nil {
				c.mu.RUnlock()
				return pathErr
			}
			if _, statErr := os.Lstat(destination); statErr == nil {
				c.mu.RUnlock()
				return fmt.Errorf("目标文件已存在，不会覆盖：%s", destination)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				c.mu.RUnlock()
				return fmt.Errorf("检查目标文件失败：%w", statErr)
			}
			items = append(items, item{index: i, job: job, destination: destination})
			total += job.Size
		}
	}
	c.mu.RUnlock()
	if len(items) == 0 {
		return errors.New("没有超限文件需要移动")
	}

	var offset int64
	for _, current := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(current.job.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != current.job.Size || !info.ModTime().Equal(current.job.ModTime) {
			if err == nil {
				err = errors.New("文件已被替换或修改")
			}
			return fmt.Errorf("移动前检查失败：%w", err)
		}

		// Persist the move intent before touching the source. If the process
		// stops after the filesystem move but before the final save, Load can
		// deterministically recover by checking source and destination.
		c.mu.Lock()
		if current.index >= len(c.jobs) || c.jobs[current.index].ID != current.job.ID || c.jobs[current.index].State != model.JobOversize {
			c.mu.Unlock()
			return errors.New("超限文件队列已发生变化，请重新扫描")
		}
		c.jobs[current.index].State = model.JobMoving
		c.jobs[current.index].MoveDestination = current.destination
		c.jobs[current.index].Error = ""
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.mu.Lock()
			c.jobs[current.index].State = model.JobOversize
			c.jobs[current.index].MoveDestination = ""
			c.queueRevision++
			c.notifyLocked()
			c.mu.Unlock()
			return fmt.Errorf("保存移动意图失败：%w", err)
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
			c.jobs[current.index].State = model.JobOversize
			c.jobs[current.index].MoveDestination = ""
			c.jobs[current.index].Error = err.Error()
			c.queueRevision++
			c.notifyLocked()
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				return errors.Join(err, fmt.Errorf("保存移动失败状态失败：%w", persistErr))
			}
			return err
		}
		offset += current.job.Size
		now := time.Now()
		c.mu.Lock()
		c.jobs[current.index].State = model.JobMoved
		c.jobs[current.index].Path = current.destination
		c.jobs[current.index].MoveDestination = ""
		c.jobs[current.index].CompletedAt = &now
		c.jobs[current.index].Error = ""
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			return err
		}
	}
	return nil
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
