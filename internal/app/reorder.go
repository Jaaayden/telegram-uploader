package app

import (
	"errors"
	"fmt"

	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// ReorderAction selects how selected queue entries are moved.
type ReorderAction string

const (
	ReorderUp     ReorderAction = "up"
	ReorderDown   ReorderAction = "down"
	ReorderTop    ReorderAction = "top"
	ReorderBottom ReorderAction = "bottom"
	ReorderBefore ReorderAction = "before"
	ReorderAfter  ReorderAction = "after"
)

func validReorderAction(action ReorderAction) bool {
	switch action {
	case ReorderUp, ReorderDown, ReorderTop, ReorderBottom, ReorderBefore, ReorderAfter:
		return true
	default:
		return false
	}
}

func isPinnedState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobConfirming, model.JobSent, model.JobMoving, model.JobMoved:
		return true
	default:
		return false
	}
}

type reorderUnit struct {
	groupID  string
	jobs     []model.Job
	selected bool
}

// ReorderJobs changes the queue order of movable jobs while keeping active,
// confirming, sent, moving, and moved entries pinned at their existing slots.
// Merged albums move together as indivisible units unless a strict subset of
// one album's members is reordered within that group.
func (c *Controller) ReorderJobs(ids []string, action ReorderAction, targetID string) (bool, error) {
	if !validReorderAction(action) {
		return false, fmt.Errorf("不支持的排序操作：%q", action)
	}
	if (action == ReorderBefore || action == ReorderAfter) && targetID == "" {
		return false, errors.New("目标条目不能为空")
	}
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			requested[id] = struct{}{}
		}
	}
	if len(requested) == 0 {
		return false, nil
	}
	if !c.opMu.TryLock() {
		return false, errors.New("扫描或移动操作正在进行，请稍后重试")
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

		isJobPinned := func(job model.Job) bool {
			if isPinnedState(job.State) {
				return true
			}
			if _, active := activeSet[job.ID]; active {
				return true
			}
			if _, pending := pendingRemoval[job.ID]; pending {
				return true
			}
			return false
		}
		pinnedGroups := make(map[string]bool)
		for _, job := range nextJobs {
			if job.GroupID != "" && isJobPinned(job) {
				pinnedGroups[job.GroupID] = true
			}
		}

		var movableIndices []int
		var movable []model.Job
		movablePosByID := make(map[string]int, len(nextJobs))
		originalPosByID := make(map[string]int, len(nextJobs))
		for i, job := range nextJobs {
			originalPosByID[job.ID] = i
			if isJobPinned(job) || (job.GroupID != "" && pinnedGroups[job.GroupID]) {
				continue
			}
			movablePosByID[job.ID] = len(movable)
			movableIndices = append(movableIndices, i)
			movable = append(movable, job)
		}

		selectedSet := make(map[string]struct{}, len(requested))
		for id := range requested {
			if _, ok := movablePosByID[id]; ok {
				selectedSet[id] = struct{}{}
			}
		}
		if len(selectedSet) == 0 {
			return false, nil
		}

		reordered := append([]model.Job(nil), movable...)
		handledIntra := false

		// Check if every selected movable job belongs to one group and is a
		// strict subset of that group's movable members.
		singleGroup, ok := singleSelectedGroup(movable, selectedSet)
		if ok {
			var gPos []int
			var gMembers []model.Job
			targetInGroup := false
			for idx, job := range movable {
				if job.GroupID == singleGroup {
					gPos = append(gPos, idx)
					gMembers = append(gMembers, job)
					if job.ID == targetID {
						targetInGroup = true
					}
				}
			}
			if len(selectedSet) < len(gMembers) {
				switch action {
				case ReorderBefore, ReorderAfter:
					if targetInGroup {
						gUnits := make([]reorderUnit, len(gMembers))
						targetUnitIdx := -1
						for i, m := range gMembers {
							_, sel := selectedSet[m.ID]
							gUnits[i] = reorderUnit{jobs: []model.Job{m}, selected: sel}
							if m.ID == targetID {
								targetUnitIdx = i
							}
						}
						if targetUnitIdx >= 0 && !gUnits[targetUnitIdx].selected {
							gUnits = reorderUnitSlice(gUnits, action, targetUnitIdx)
							for i, u := range gUnits {
								reordered[gPos[i]] = u.jobs[0]
							}
						}
						handledIntra = true
					}
				case ReorderUp:
					if _, firstSelected := selectedSet[gMembers[0].ID]; !firstSelected {
						gUnits := make([]reorderUnit, len(gMembers))
						for i, m := range gMembers {
							_, sel := selectedSet[m.ID]
							gUnits[i] = reorderUnit{jobs: []model.Job{m}, selected: sel}
						}
						gUnits = reorderUnitSlice(gUnits, ReorderUp, -1)
						for i, u := range gUnits {
							reordered[gPos[i]] = u.jobs[0]
						}
						handledIntra = true
					}
				case ReorderDown:
					if _, lastSelected := selectedSet[gMembers[len(gMembers)-1].ID]; !lastSelected {
						gUnits := make([]reorderUnit, len(gMembers))
						for i, m := range gMembers {
							_, sel := selectedSet[m.ID]
							gUnits[i] = reorderUnit{jobs: []model.Job{m}, selected: sel}
						}
						gUnits = reorderUnitSlice(gUnits, ReorderDown, -1)
						for i, u := range gUnits {
							reordered[gPos[i]] = u.jobs[0]
						}
						handledIntra = true
					}
				}
			}
		}

		if !handledIntra {
			units := buildReorderUnits(movable, selectedSet)
			effectiveAction := action
			targetUnitIdx := -1
			if action == ReorderBefore || action == ReorderAfter {
				for uIdx, u := range units {
					for _, job := range u.jobs {
						if job.ID == targetID {
							targetUnitIdx = uIdx
							break
						}
					}
					if targetUnitIdx >= 0 {
						break
					}
				}
				if targetUnitIdx < 0 {
					pinnedIdx, exists := originalPosByID[targetID]
					if !exists {
						return false, nil
					}
					// Dropping near a pinned job resolves to the first movable
					// unit after pinnedIdx, or the bottom if none follow it.
					foundAfter := -1
					for uIdx, u := range units {
						if originalPosByID[u.jobs[0].ID] > pinnedIdx {
							foundAfter = uIdx
							break
						}
					}
					if foundAfter >= 0 {
						targetUnitIdx = foundAfter
						effectiveAction = ReorderBefore
					} else {
						effectiveAction = ReorderBottom
					}
				}
				if (effectiveAction == ReorderBefore || effectiveAction == ReorderAfter) &&
					(targetUnitIdx < 0 || units[targetUnitIdx].selected) {
					return false, nil
				}
			}
			units = reorderUnitSlice(units, effectiveAction, targetUnitIdx)
			reordered = reordered[:0]
			for _, u := range units {
				reordered = append(reordered, u.jobs...)
			}
		}

		changed := false
		for i, job := range reordered {
			if movable[i].ID != job.ID {
				changed = true
			}
			nextJobs[movableIndices[i]] = job
		}
		if !changed {
			return false, nil
		}

		reindexJobs(nextJobs)
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return false, err
		}
		if c.commitCandidate(nextJobs, folder, revision) {
			return true, nil
		}
	}
}

func singleSelectedGroup(movable []model.Job, selectedSet map[string]struct{}) (string, bool) {
	groupID := ""
	for _, job := range movable {
		if _, ok := selectedSet[job.ID]; !ok {
			continue
		}
		if job.GroupID == "" {
			return "", false
		}
		if groupID == "" {
			groupID = job.GroupID
		} else if groupID != job.GroupID {
			return "", false
		}
	}
	return groupID, groupID != ""
}

func buildReorderUnits(movable []model.Job, selectedSet map[string]struct{}) []reorderUnit {
	units := make([]reorderUnit, 0, len(movable))
	groupUnit := make(map[string]int)
	for _, job := range movable {
		_, sel := selectedSet[job.ID]
		if job.GroupID == "" {
			units = append(units, reorderUnit{jobs: []model.Job{job}, selected: sel})
			continue
		}
		if idx, exists := groupUnit[job.GroupID]; exists {
			units[idx].jobs = append(units[idx].jobs, job)
			if sel {
				units[idx].selected = true
			}
			continue
		}
		groupUnit[job.GroupID] = len(units)
		units = append(units, reorderUnit{groupID: job.GroupID, jobs: []model.Job{job}, selected: sel})
	}
	return units
}

func reorderUnitSlice(units []reorderUnit, action ReorderAction, targetUnitIdx int) []reorderUnit {
	switch action {
	case ReorderTop:
		out := make([]reorderUnit, 0, len(units))
		for _, u := range units {
			if u.selected {
				out = append(out, u)
			}
		}
		for _, u := range units {
			if !u.selected {
				out = append(out, u)
			}
		}
		return out
	case ReorderBottom:
		out := make([]reorderUnit, 0, len(units))
		for _, u := range units {
			if !u.selected {
				out = append(out, u)
			}
		}
		for _, u := range units {
			if u.selected {
				out = append(out, u)
			}
		}
		return out
	case ReorderUp:
		out := append([]reorderUnit(nil), units...)
		for i := 1; i < len(out); i++ {
			if out[i].selected && !out[i-1].selected {
				out[i], out[i-1] = out[i-1], out[i]
			}
		}
		return out
	case ReorderDown:
		out := append([]reorderUnit(nil), units...)
		for i := len(out) - 2; i >= 0; i-- {
			if out[i].selected && !out[i+1].selected {
				out[i], out[i+1] = out[i+1], out[i]
			}
		}
		return out
	case ReorderBefore, ReorderAfter:
		if targetUnitIdx < 0 || targetUnitIdx >= len(units) || units[targetUnitIdx].selected {
			return units
		}
		var selected, unselected []reorderUnit
		newTargetPos := 0
		for i, u := range units {
			if u.selected {
				selected = append(selected, u)
				continue
			}
			if i == targetUnitIdx {
				newTargetPos = len(unselected)
			}
			unselected = append(unselected, u)
		}
		insertAt := newTargetPos
		if action == ReorderAfter {
			insertAt = newTargetPos + 1
		}
		out := make([]reorderUnit, 0, len(units))
		out = append(out, unselected[:insertAt]...)
		out = append(out, selected...)
		out = append(out, unselected[insertAt:]...)
		return out
	default:
		return units
	}
}
