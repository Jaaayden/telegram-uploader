package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

func (c *Controller) Scan(folder string, maxBytes int64) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.RLock()
	running := c.running
	c.mu.RUnlock()
	if running {
		return errors.New("上传进行中，不能重新扫描文件夹")
	}
	jobs, err := scanner.Scan(folder, maxBytes)
	if err != nil {
		return err
	}
	for {
		c.mu.RLock()
		channel := c.channel
		paused := c.paused
		revision := c.queueRevision
		c.mu.RUnlock()
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		if c.commitCandidate(jobs, folder, revision) {
			return nil
		}
	}
}

// AddResult summarizes how many candidates were added to the queue and how
// they were grouped for sending.
type AddResult struct {
	Added   int
	Groups  int
	Singles int
}

// AddJobs appends scanned candidates to the durable queue, including while an
// upload is active. Candidates are de-duplicated by normalized absolute path. An
// existing path always wins, so a previously sent/failed job keeps its ID,
// RandomID and state instead of being replaced by a newly scanned copy.
//
// The scanner already returns candidates in natural filename order. When
// adding candidates from another folder, their order is preserved and the
// new group is appended after the existing queue. Positions are rebuilt for
// the complete queue after every successful append.
func (c *Controller) AddJobs(candidates []model.Job) (int, error) {
	result, err := c.addJobs(candidates, false)
	return result.Added, err
}

// AddMergedJobs appends candidates like AddJobs, grouping newly queued videos
// into consecutive albums of up to model.MaxAlbumSize (10) items. Oversize
// videos and a single remaining trailing video stay ungrouped.
func (c *Controller) AddMergedJobs(candidates []model.Job) (AddResult, error) {
	return c.addJobs(candidates, true)
}

func (c *Controller) addJobs(candidates []model.Job, merge bool) (AddResult, error) {
	if len(candidates) == 0 {
		return AddResult{}, nil
	}
	if !c.opMu.TryLock() {
		return AddResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		c.mu.RUnlock()

		nextJobs := append([]model.Job(nil), current...)
		paths := make(map[string]struct{}, len(current)+len(candidates))
		ids := make(map[string]struct{}, len(current)+len(candidates))
		randomIDs := make(map[int64]struct{}, len(current)+len(candidates))
		existingGroups := make(map[string]struct{}, len(current))
		for _, job := range current {
			if key := jobPathKey(job.Path); key != "" {
				paths[key] = struct{}{}
			}
			if job.ID != "" {
				ids[job.ID] = struct{}{}
			}
			if job.RandomID != 0 {
				randomIDs[job.RandomID] = struct{}{}
			}
			if job.GroupID != "" {
				existingGroups[job.GroupID] = struct{}{}
			}
		}
		var result AddResult
		var queuedIndices []int
		for _, candidate := range candidates {
			if strings.TrimSpace(candidate.Path) == "" {
				return AddResult{}, errors.New("追加任务失败：文件路径不能为空")
			}
			key := jobPathKey(candidate.Path)
			if _, exists := paths[key]; exists {
				continue
			}
			if strings.TrimSpace(candidate.ID) == "" {
				return AddResult{}, fmt.Errorf("追加任务 %q 失败：Job ID 不能为空", candidate.Name)
			}
			if _, exists := ids[candidate.ID]; exists {
				return AddResult{}, fmt.Errorf("追加任务 %q 失败：Job ID 重复", candidate.Name)
			}
			if candidate.RandomID == 0 {
				return AddResult{}, fmt.Errorf("追加任务 %q 失败：Telegram RandomID 不能为空", candidate.Name)
			}
			if _, exists := randomIDs[candidate.RandomID]; exists {
				return AddResult{}, fmt.Errorf("追加任务 %q 失败：Telegram RandomID 重复", candidate.Name)
			}
			if strings.TrimSpace(candidate.Name) == "" || candidate.Size < 0 {
				return AddResult{}, errors.New("追加任务失败：文件名或大小无效")
			}
			if candidate.State == "" {
				candidate.State = model.JobQueued
			}
			if candidate.State != model.JobQueued && candidate.State != model.JobOversize {
				return AddResult{}, fmt.Errorf("追加任务 %q 失败：不允许的初始状态 %q", candidate.Name, candidate.State)
			}
			if merge {
				candidate.GroupID = ""
				candidate.Staged = nil
			}
			if candidate.State == model.JobQueued {
				queuedIndices = append(queuedIndices, len(nextJobs))
			}
			nextJobs = append(nextJobs, candidate)
			paths[key] = struct{}{}
			ids[candidate.ID] = struct{}{}
			randomIDs[candidate.RandomID] = struct{}{}
			result.Added++
		}
		if result.Added == 0 {
			return AddResult{}, nil
		}
		if merge {
			for start := 0; start < len(queuedIndices); start += model.MaxAlbumSize {
				end := min(start+model.MaxAlbumSize, len(queuedIndices))
				chunk := queuedIndices[start:end]
				if len(chunk) < 2 {
					nextJobs[chunk[0]].GroupID = ""
					result.Singles++
					continue
				}
				groupID, err := newGroupID(existingGroups)
				if err != nil {
					return AddResult{}, err
				}
				existingGroups[groupID] = struct{}{}
				for _, idx := range chunk {
					nextJobs[idx].GroupID = groupID
				}
				result.Groups++
			}
		}
		reindexJobs(nextJobs)
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return AddResult{}, err
		}
		if c.commitCandidate(nextJobs, folder, revision) {
			return result, nil
		}
	}
}

func newGroupID(existing map[string]struct{}) (string, error) {
	var raw [8]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("生成合并组标识失败：%w", err)
		}
		id := "grp-" + hex.EncodeToString(raw[:])
		if _, exists := existing[id]; !exists {
			return id, nil
		}
	}
}

func isRemovableJobState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobConfirming, model.JobMoving:
		return false
	default:
		return true
	}
}

func pendingRemovalEligibleState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobSent, model.JobCancelled, model.JobInterrupted, model.JobFailed:
		return true
	default:
		return false
	}
}

func (c *Controller) requestPendingRemovalLocked(id string) context.CancelFunc {
	if c.pendingRemoval == nil {
		c.pendingRemoval = make(map[string]struct{})
	}
	c.pendingRemoval[id] = struct{}{}
	if c.activeID == id {
		c.cancelJobID = id
		return c.activeCancel
	}
	return nil
}

// requestActiveRemovalLocked records a cancellation only after any structural
// queue edit in the same user operation has been durably committed. A nil
// selected set means every active job is selected (the ClearQueue case).
// c.mu must be held by the caller.
func (c *Controller) requestActiveRemovalLocked(selected map[string]struct{}) ([]string, context.CancelFunc) {
	if len(c.activeBatch) > 0 {
		var pending []string
		for _, id := range c.activeBatch {
			if id == "" {
				continue
			}
			if selected != nil {
				if _, ok := selected[id]; !ok {
					continue
				}
			}
			index := findJobIndexLocked(c.jobs, id)
			if index < 0 {
				continue
			}
			state := c.jobs[index].State
			if !pendingRemovalEligibleState(state) && !(state == model.JobQueued && c.retryWaitActive) {
				continue
			}
			if c.pendingRemoval == nil {
				c.pendingRemoval = make(map[string]struct{})
			}
			c.pendingRemoval[id] = struct{}{}
			pending = append(pending, id)
		}
		if len(pending) == 0 {
			return nil, nil
		}
		var cancel context.CancelFunc
		// Never interrupt an in-flight SendAlbum RPC; let the quick RPC finish
		// and remove cleanly after its terminal state is persisted. Only cancel
		// when waiting on retry backoff.
		if c.retryWaitActive {
			cancel = c.activeCancel
		}
		c.notifyLocked()
		return pending, cancel
	}
	id := c.activeID
	if id == "" {
		return nil, nil
	}
	if selected != nil {
		if _, ok := selected[id]; !ok {
			return nil, nil
		}
	}
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 || !pendingRemovalEligibleState(c.jobs[index].State) {
		return nil, nil
	}
	cancel := c.requestPendingRemovalLocked(id)
	c.notifyLocked()
	return []string{id}, cancel
}

// RemoveJobs removes local records while a queue is running as well. An active
// upload is cancelled first and removed only after its outcome is known.
func (c *Controller) RemoveJobs(ids []string) (RemovalResult, error) {
	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return RemovalResult{}, nil
	}
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.Lock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			_, selectedJob := selected[job.ID]
			return selectedJob && isRemovableJobState(job.State) && !c.isActiveLocked(job.ID)
		})
		if removed == 0 {
			pending, cancel := c.requestActiveRemovalLocked(selected)
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return RemovalResult{PendingRemovalIDs: pending}, nil
		}
		c.mu.Unlock()
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		committed, pending, cancel := c.commitRemovalCandidate(
			nextJobs,
			folderIfJobs(nextJobs, folder),
			revision,
			selected,
		)
		if committed {
			if cancel != nil {
				cancel()
			}
			return RemovalResult{Removed: removed, PendingRemovalIDs: pending}, nil
		}
	}
}

func folderIfJobs(jobs []model.Job, current string) string {
	if len(jobs) == 0 {
		return ""
	}
	return current
}

// ClearQueue removes every removable local record. The active upload follows
// the same cancel-then-remove path as RemoveJobs; confirming/moving records are
// retained because their external outcome is not safely reversible.
func (c *Controller) ClearQueue() (RemovalResult, error) {
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.Lock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			return isRemovableJobState(job.State) && !c.isActiveLocked(job.ID)
		})
		if removed == 0 {
			pending, cancel := c.requestActiveRemovalLocked(nil)
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return RemovalResult{PendingRemovalIDs: pending}, nil
		}
		c.mu.Unlock()
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		committed, pending, cancel := c.commitRemovalCandidate(
			nextJobs,
			folderIfJobs(nextJobs, folder),
			revision,
			nil,
		)
		if committed {
			if cancel != nil {
				cancel()
			}
			return RemovalResult{Removed: removed, PendingRemovalIDs: pending}, nil
		}
	}
}

// RemoveCompleted can run concurrently with uploads and removes only jobs
// whose Telegram/file operation has already reached a terminal success state.
func (c *Controller) RemoveCompleted() (RemovalResult, error) {
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		c.mu.RUnlock()
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			return job.State == model.JobSent || job.State == model.JobMoved
		})
		result := RemovalResult{Removed: removed}
		if removed == 0 {
			return result, nil
		}
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		if c.commitCandidate(nextJobs, folderIfJobs(nextJobs, folder), revision) {
			return result, nil
		}
	}
}

// ResetJobs returns recoverable terminal jobs to the queued state. It is safe
// to invoke while the queue is active; the current upload is never reset and
// newly queued jobs are picked up by the next selection pass.
//
// The complete next queue is persisted before it is published to observers,
// so a storage failure cannot leave the UI and queue.json disagreeing.
func (c *Controller) ResetJobs(mode ResetMode, ids []string) (int, error) {
	if !validResetMode(mode) {
		return 0, errors.New("未知的任务重置方式")
	}

	selected := make(map[string]struct{}, len(ids))
	if mode == ResetSelected {
		for _, id := range ids {
			if id != "" {
				selected[id] = struct{}{}
			}
		}
		if len(selected) == 0 {
			return 0, nil
		}
	}
	if !c.opMu.TryLock() {
		return 0, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
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

		nextJobs := append([]model.Job(nil), current...)
		resetCount := 0
		for i := range nextJobs {
			job := &nextJobs[i]
			_, isActive := activeSet[job.ID]
			_, pending := pendingRemoval[job.ID]
			if isActive || pending || !matchesResetMode(*job, mode, selected) {
				continue
			}
			job.State = model.JobQueued
			resetPendingJobProgress(job)
			resetCount++
		}
		if resetCount == 0 {
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
			return resetCount, nil
		}
	}
}

func validResetMode(mode ResetMode) bool {
	switch mode {
	case ResetSelected, ResetCancelled, ResetFailed, ResetSkipped, ResetAllRecoverable:
		return true
	default:
		return false
	}
}

func matchesResetMode(job model.Job, mode ResetMode, selected map[string]struct{}) bool {
	if !isRecoverableJobState(job.State) {
		return false
	}
	switch mode {
	case ResetSelected:
		_, ok := selected[job.ID]
		return ok
	case ResetCancelled:
		return job.State == model.JobCancelled
	case ResetFailed:
		return job.State == model.JobFailed || job.State == model.JobInterrupted
	case ResetSkipped:
		return job.State == model.JobSkipped
	case ResetAllRecoverable:
		return true
	default:
		return false
	}
}

func isRecoverableJobState(state model.JobState) bool {
	switch state {
	case model.JobCancelled, model.JobFailed, model.JobInterrupted, model.JobSkipped:
		return true
	default:
		return false
	}
}

func filteredJobs(jobs []model.Job, remove func(model.Job) bool) ([]model.Job, int) {
	kept := make([]model.Job, 0, len(jobs))
	removed := 0
	for _, job := range jobs {
		if remove(job) {
			removed++
			continue
		}
		kept = append(kept, job)
	}
	if removed > 0 {
		reindexJobs(kept)
	}
	return kept, removed
}

func findJobIndexLocked(jobs []model.Job, id string) int {
	if id == "" {
		return -1
	}
	for index := range jobs {
		if jobs[index].ID == id {
			return index
		}
	}
	return -1
}

// persistCandidate serializes writes and validates the queue revision before
// and after Save. The disk write is deliberately outside c.mu so a slow fsync
// cannot stall upload progress callbacks; callers retry with a fresh candidate
// when a concurrent durable update is detected.
func (c *Controller) persistCandidate(jobs []model.Job, channel model.Channel, paused bool, revision uint64) error {
	if c.store == nil {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.RLock()
	if c.queueRevision != revision {
		c.mu.RUnlock()
		return errQueueChanged
	}
	copyJobs := cloneJobsForPersistence(jobs)
	c.preserveActiveProgressLocked(copyJobs)
	c.mu.RUnlock()
	if err := c.store.Save(copyJobs, channel, paused); err != nil {
		return err
	}
	c.mu.RLock()
	changed := c.queueRevision != revision
	c.mu.RUnlock()
	if changed {
		return errQueueChanged
	}
	return nil
}

func cloneJobsForPersistence(jobs []model.Job) []model.Job {
	copyJobs := append([]model.Job(nil), jobs...)
	for i := range copyJobs {
		if jobs[i].StartedAt != nil {
			started := *jobs[i].StartedAt
			copyJobs[i].StartedAt = &started
		}
		if jobs[i].CompletedAt != nil {
			completed := *jobs[i].CompletedAt
			copyJobs[i].CompletedAt = &completed
		}
		if jobs[i].UploadPartsAt != nil {
			partsAt := *jobs[i].UploadPartsAt
			copyJobs[i].UploadPartsAt = &partsAt
		}
		if jobs[i].Staged != nil {
			copyJobs[i].Staged = cloneStagedMedia(jobs[i].Staged)
		}
	}
	return copyJobs
}

func cloneStagedMedia(staged *model.StagedMedia) *model.StagedMedia {
	if staged == nil {
		return nil
	}
	copyStaged := *staged
	if staged.FileReference != nil {
		copyStaged.FileReference = append([]byte(nil), staged.FileReference...)
	}
	return &copyStaged
}

// commitCandidate publishes a candidate only if the revision used to persist
// it is still current. The caller must not hold c.mu.
func (c *Controller) commitCandidate(jobs []model.Job, folder string, revision uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queueRevision != revision {
		return false
	}
	c.preserveActiveProgressLocked(jobs)
	c.jobs = cloneJobsForPersistence(jobs)
	if folder != "" || len(jobs) == 0 {
		c.folder = folder
	}
	c.queueRevision++
	c.lastError = ""
	c.notifyLocked()
	return true
}

// commitRemovalCandidate atomically publishes a durably saved structural
// deletion and records cancellation of the selected active job. Keeping these
// actions under one lock prevents an active job from reaching a terminal state
// in the gap between the queue commit and the pending-removal marker.
func (c *Controller) commitRemovalCandidate(
	jobs []model.Job,
	folder string,
	revision uint64,
	selected map[string]struct{},
) (bool, []string, context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queueRevision != revision {
		return false, nil, nil
	}
	c.preserveActiveProgressLocked(jobs)
	c.jobs = cloneJobsForPersistence(jobs)
	if folder != "" || len(jobs) == 0 {
		c.folder = folder
	}
	c.queueRevision++
	c.lastError = ""
	pending, cancel := c.requestActiveRemovalLocked(selected)
	if len(pending) == 0 {
		c.notifyLocked()
	}
	return true, pending, cancel
}

func (c *Controller) preserveActiveProgressLocked(jobs []model.Job) {
	// Progress callbacks intentionally do not advance queueRevision. Preserve
	// the current active record when a structural edit was prepared from an
	// earlier snapshot, otherwise an edit completing just after Save could
	// roll Uploaded/BytesPerSecond/attempt state back to stale values.
	if c.activeID != "" {
		if currentIndex := findJobIndexLocked(c.jobs, c.activeID); currentIndex >= 0 {
			for index := range jobs {
				if jobs[index].ID == c.activeID {
					// Only progress fields are intentionally omitted from
					// queueRevision. Keep the candidate's Position and all
					// durable state so deletion/reindex edits remain coherent.
					jobs[index].Uploaded = c.jobs[currentIndex].Uploaded
					jobs[index].BytesPerSecond = c.jobs[currentIndex].BytesPerSecond
					break
				}
			}
		}
	}
}

func reindexJobs(jobs []model.Job) bool {
	changed := false
	for index := range jobs {
		if jobs[index].Position != index {
			changed = true
		}
		jobs[index].Position = index
	}
	return changed
}

func jobPathKey(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err == nil {
		path = absPath
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// ApplyUploadLimit updates only jobs whose eligibility can still change. It
// is used when a folder was scanned before Telegram reported the current Bot
// upload limit, and deliberately preserves sent, failed and active history.
func (c *Controller) ApplyUploadLimit(maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("上传上限必须大于 0")
	}
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	for {
		c.mu.RLock()
		jobs := cloneJobsForPersistence(c.jobs)
		channel := c.channel
		paused := c.paused
		folder := c.folder
		revision := c.queueRevision
		c.mu.RUnlock()

		changed := false
		for i := range jobs {
			job := &jobs[i]
			switch job.State {
			case model.JobQueued, model.JobInterrupted, model.JobOversize:
			default:
				continue
			}
			if job.Size > maxBytes {
				if job.State != model.JobOversize || job.Uploaded != 0 || job.GroupID != "" || job.Staged != nil {
					job.State = model.JobOversize
					job.GroupID = ""
					resetPendingJobProgress(job)
					changed = true
				}
			} else if job.State == model.JobOversize {
				job.State = model.JobQueued
				resetPendingJobProgress(job)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		if c.commitCandidate(jobs, folder, revision) {
			return nil
		}
	}
}

func resetPendingJobProgress(job *model.Job) {
	job.Uploaded = 0
	job.BytesPerSecond = 0
	job.MessageID = 0
	job.ChannelID = 0
	job.Metadata = model.VideoMetadata{}
	job.Error = ""
	job.StartedAt = nil
	job.CompletedAt = nil
	job.MoveDestination = ""
	job.Staged = nil
	clearResume(job)
}

// clearResume forgets a partial upload, so the next attempt starts over.
func clearResume(job *model.Job) {
	job.UploadFileID = 0
	job.UploadParts = 0
	job.UploadPartSize = 0
	job.UploadPartsAt = nil
}

// resumeToken returns the job's partial-upload state.
func resumeToken(job *model.Job) tgtransport.ResumeToken {
	token := tgtransport.ResumeToken{FileID: job.UploadFileID, Parts: job.UploadParts, PartSize: job.UploadPartSize, Size: job.Size}
	if job.UploadPartsAt != nil {
		token.At = *job.UploadPartsAt
	}
	return token
}

func (c *Controller) SetChannel(channel model.Channel) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.RLock()
		if c.running {
			c.mu.RUnlock()
			return errors.New("上传进行中，不能切换频道")
		}
		prevChannel := c.channel
		jobs := cloneJobsForPersistence(c.jobs)
		paused := c.paused
		revision := c.queueRevision
		c.mu.RUnlock()
		if channel.ID != prevChannel.ID {
			for i := range jobs {
				job := &jobs[i]
				if (job.State == model.JobQueued || job.State == model.JobInterrupted) && job.Staged != nil {
					job.Staged = nil
					job.Uploaded = 0
				}
			}
		}
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.channel = channel
		c.jobs = cloneJobsForPersistence(jobs)
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return nil
	}
}
