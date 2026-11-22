package app

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// CaptionEdit updates one queue entry's (or album's) caption preference. When
// Custom is false, the uploader falls back to the filename-derived default.
type CaptionEdit struct {
	ID      string `json:"id"`
	Custom  bool   `json:"custom"`
	Caption string `json:"caption"`
}

// UTF16Len returns the number of UTF-16 code units in s, matching Telegram's
// caption length limit.
func UTF16Len(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// TruncateCaption trims s on rune boundaries so its UTF-16 length never exceeds
// maxUnits, appending "…" when truncated.
func TruncateCaption(s string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	if UTF16Len(s) <= maxUnits {
		return s
	}
	limit := maxUnits - 1
	units := 0
	cut := 0
	for idx, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > limit {
			break
		}
		units += w
		cut = idx + utf8.RuneLen(r)
	}
	return s[:cut] + "…"
}

func effectiveSingleCaption(job model.Job) string {
	if job.CaptionCustom {
		return TruncateCaption(job.CustomCaption, model.MaxCaptionUTF16)
	}
	return TruncateCaption(captionFromFilename(job.Name), model.MaxCaptionUTF16)
}

func effectiveAlbumCaption(members []model.Job) string {
	if len(members) == 0 {
		return ""
	}
	if len(members) == 1 {
		return effectiveSingleCaption(members[0])
	}
	if members[0].CaptionCustom {
		return TruncateCaption(members[0].CustomCaption, model.MaxCaptionUTF16)
	}
	for _, member := range members[1:] {
		if member.CaptionCustom {
			return TruncateCaption(member.CustomCaption, model.MaxCaptionUTF16)
		}
	}
	lines := make([]string, 0, len(members))
	for _, member := range members {
		if line := captionFromFilename(member.Name); line != "" {
			lines = append(lines, line)
		}
	}
	return TruncateCaption(strings.Join(lines, "\n"), model.MaxCaptionUTF16)
}

func isCaptionEditableState(state model.JobState) bool {
	switch state {
	case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobCancelled, model.JobSkipped:
		return true
	default:
		return false
	}
}

// SetCaptions updates custom captions for editable queued/recoverable entries.
// Editing any member of an editable album updates all editable members of that
// group so the shared album caption survives member reordering or cancellation.
func (c *Controller) SetCaptions(edits []CaptionEdit) (int, error) {
	if len(edits) == 0 {
		return 0, nil
	}
	for _, edit := range edits {
		if edit.ID == "" {
			return 0, errors.New("修改 Caption 失败：任务 ID 不能为空")
		}
		if edit.Custom {
			if length := UTF16Len(edit.Caption); length > model.MaxCaptionUTF16 {
				return 0, fmt.Errorf("Caption 长度超过 %d 字符上限（当前 %d）", model.MaxCaptionUTF16, length)
			}
		}
	}
	if !c.opMu.TryLock() {
		return 0, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	for {
		c.mu.RLock()
		nextJobs := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		activeSet := c.activeIDSetLocked()
		pendingRemoval := make(map[string]struct{}, len(c.pendingRemoval))
		for id := range c.pendingRemoval {
			pendingRemoval[id] = struct{}{}
		}
		c.mu.RUnlock()

		byID := make(map[string]int, len(nextJobs))
		groupIndices := make(map[string][]int)
		lockedGroups := make(map[string]bool)
		for i, job := range nextJobs {
			byID[job.ID] = i
			if job.GroupID != "" {
				groupIndices[job.GroupID] = append(groupIndices[job.GroupID], i)
				_, isActive := activeSet[job.ID]
				_, isPending := pendingRemoval[job.ID]
				if isActive || isPending || !isCaptionEditableState(job.State) {
					lockedGroups[job.GroupID] = true
				}
			}
		}

		changed := make(map[int]struct{})
		for _, edit := range edits {
			idx, ok := byID[edit.ID]
			if !ok {
				continue
			}
			job := nextJobs[idx]
			_, isActive := activeSet[job.ID]
			_, isPending := pendingRemoval[job.ID]
			if isActive || isPending || !isCaptionEditableState(job.State) {
				continue
			}
			if job.GroupID != "" && lockedGroups[job.GroupID] {
				continue
			}

			targets := []int{idx}
			if job.GroupID != "" && len(groupIndices[job.GroupID]) >= 2 {
				targets = groupIndices[job.GroupID]
			}
			newCustom := edit.Custom
			newCaption := ""
			if newCustom {
				newCaption = edit.Caption
			}
			for _, ti := range targets {
				if !isCaptionEditableState(nextJobs[ti].State) {
					continue
				}
				if nextJobs[ti].CaptionCustom != newCustom || nextJobs[ti].CustomCaption != newCaption {
					nextJobs[ti].CaptionCustom = newCustom
					nextJobs[ti].CustomCaption = newCaption
					changed[ti] = struct{}{}
				}
			}
		}

		if len(changed) == 0 {
			return 0, nil
		}
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return 0, err
		}
		if c.commitCandidate(nextJobs, folder, revision) {
			return len(changed), nil
		}
	}
}
