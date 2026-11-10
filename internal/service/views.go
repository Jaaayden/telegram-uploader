package service

import (
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
	"github.com/Jaaayden/telegram-uploader/internal/model"
)

// JobView is one queue row as the front end sees it. It is a comparable value
// so unchanged rows can be detected with ==.
type JobView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	State     string `json:"state"`
	Uploaded  int64  `json:"uploaded"`
	Error     string `json:"error,omitempty"`
	MessageID int    `json:"messageId,omitempty"`
	// Truncated marks an MP4 whose media data is shorter than declared.
	Truncated bool `json:"truncated,omitempty"`
	// Removing marks an active job that is being cancelled for removal.
	Removing bool `json:"removing,omitempty"`
}

// JobsDelta carries only what changed since the previous delta. Order is nil
// when neither membership nor order changed; rows missing from Order have
// been removed.
type JobsDelta struct {
	Rev    uint64    `json:"rev"`
	Upsert []JobView `json:"upsert"`
	Order  []string  `json:"order"`
}

// ProgressEvent is the lightweight update sent while a file uploads.
type ProgressEvent struct {
	ID         string  `json:"id"`
	Uploaded   int64   `json:"uploaded"`
	Speed      float64 `json:"speed"`
	DoneBytes  int64   `json:"doneBytes"`
	TotalBytes int64   `json:"totalBytes"`
	QueueSpeed float64 `json:"queueSpeed"`
	ETASeconds int64   `json:"etaSeconds"`
}

type queueStats struct {
	Runnable bool
	Oversize int
}

func computeStats(jobs []model.Job) queueStats {
	var stats queueStats
	for index := range jobs {
		switch jobs[index].State {
		case model.JobQueued, model.JobInterrupted:
			stats.Runnable = true
		case model.JobOversize:
			stats.Oversize++
		}
	}
	return stats
}

func jobView(job model.Job, removing bool) JobView {
	return JobView{
		ID:        job.ID,
		Name:      job.Name,
		Path:      job.Path,
		Size:      job.Size,
		State:     string(job.State),
		Uploaded:  job.Uploaded,
		Error:     job.Error,
		MessageID: job.MessageID,
		Truncated: job.Metadata.TruncatedMediaData,
		Removing:  removing,
	}
}

// diffJobsLocked compares the current snapshot with what the front end
// already has and returns the difference, or nil when nothing changed. One
// pass over the queue: O(n) time, and the payload is proportional to the
// number of changed rows rather than to the queue length.
func (s *Service) diffJobsLocked() *JobsDelta {
	jobs := s.snapshot.Jobs
	removing := make(map[string]struct{}, len(s.snapshot.PendingRemovalIDs))
	for _, id := range s.snapshot.PendingRemovalIDs {
		removing[id] = struct{}{}
	}
	var upsert []JobView
	orderChanged := len(jobs) != len(s.sentOrder)
	for index := range jobs {
		_, isRemoving := removing[jobs[index].ID]
		view := jobView(jobs[index], isRemoving)
		if previous, ok := s.sentJobs[view.ID]; !ok || previous != view {
			upsert = append(upsert, view)
			s.sentJobs[view.ID] = view
		}
		if !orderChanged && s.sentOrder[index] != view.ID {
			orderChanged = true
		}
	}
	if !orderChanged && len(upsert) == 0 {
		return nil
	}
	s.jobsRev++
	delta := &JobsDelta{Rev: s.jobsRev, Upsert: upsert}
	if orderChanged {
		order := make([]string, len(jobs))
		current := make(map[string]struct{}, len(jobs))
		for index := range jobs {
			order[index] = jobs[index].ID
			current[jobs[index].ID] = struct{}{}
		}
		for id := range s.sentJobs {
			if _, ok := current[id]; !ok {
				delete(s.sentJobs, id)
			}
		}
		s.sentOrder = order
		delta.Order = order
	}
	if delta.Upsert == nil {
		delta.Upsert = []JobView{}
	}
	return delta
}

// fullJobsLocked resets the diff baseline and returns the whole queue. It is
// used when a front end (re)initializes.
func (s *Service) fullJobsLocked() *JobsDelta {
	clear(s.sentJobs)
	s.sentOrder = nil
	delta := s.diffJobsLocked()
	if delta == nil {
		s.jobsRev++
		delta = &JobsDelta{Rev: s.jobsRev, Upsert: []JobView{}}
	}
	if delta.Order == nil {
		delta.Order = []string{}
	}
	return delta
}

// StateView is everything the interface shows apart from the queue rows.
type StateView struct {
	App        AppView        `json:"app"`
	Settings   SettingsView   `json:"settings"`
	Connection ConnectionView `json:"connection"`
	Channel    ChannelView    `json:"channel"`
	Queue      QueueView      `json:"queue"`
	Schedule   ScheduleView   `json:"schedule"`
	Move       MoveView       `json:"move"`
	Update     UpdateView     `json:"update"`
	Scanning   bool           `json:"scanning"`
}

type AppView struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Commit        string `json:"commit,omitempty"`
	RepositoryURL string `json:"repositoryUrl"`
	OS            string `json:"os"`
	LogsDir       string `json:"logsDir"`
}

type SettingsView struct {
	APIID            int    `json:"apiId"`
	HasAPIHash       bool   `json:"hasApiHash"`
	HasBotToken      bool   `json:"hasBotToken"`
	HasProxyPassword bool   `json:"hasProxyPassword"`
	ProxyEnabled     bool   `json:"proxyEnabled"`
	ProxyAddress     string `json:"proxyAddress"`
	ProxyUsername    string `json:"proxyUsername"`
	Concurrency      int    `json:"concurrency"`
	LimitKBps        int    `json:"limitKBps"`
	AutoUpdateCheck  bool   `json:"autoUpdateCheck"`
	LastFolder       string `json:"lastFolder"`
	// Problem explains why stored settings or credentials could not be read.
	Problem string `json:"problem,omitempty"`
}

// ConnectionView.Status is one of: unconfigured, connecting, connected,
// retrying, error, disconnected.
type ConnectionView struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	Bot      string `json:"bot,omitempty"`
	MaxBytes int64  `json:"maxBytes,omitempty"`
	MaxExact bool   `json:"maxExact,omitempty"`
	// RetryAt and FloodWaitUntil are Unix milliseconds, zero when unset.
	RetryAt        int64 `json:"retryAt,omitempty"`
	FloodWaitUntil int64 `json:"floodWaitUntil,omitempty"`
}

type ChannelView struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type QueueView struct {
	Running        bool    `json:"running"`
	Paused         bool    `json:"paused"`
	PauseRequested bool    `json:"pauseRequested"`
	ActiveID       string  `json:"activeId,omitempty"`
	DoneBytes      int64   `json:"doneBytes"`
	TotalBytes     int64   `json:"totalBytes"`
	Speed          float64 `json:"speed"`
	ETASeconds     int64   `json:"etaSeconds"`
	LastError      string  `json:"lastError,omitempty"`
}

type ScheduleView struct {
	// At is Unix milliseconds, zero when no start is scheduled.
	At  int64 `json:"at"`
	Due bool  `json:"due"`
	// Waiting explains what a due schedule is waiting for.
	Waiting string `json:"waiting,omitempty"`
}

type MoveView struct {
	Active     bool  `json:"active"`
	DoneBytes  int64 `json:"doneBytes"`
	TotalBytes int64 `json:"totalBytes"`
}

// State assembles the current state. It is cheap: no queue rows are copied.
func (s *Service) State() StateView {
	at, set, due := s.scheduler.State()

	s.mu.Lock()
	defer s.mu.Unlock()
	view := StateView{
		App: AppView{
			Name:          buildinfo.Name,
			Version:       buildinfo.Version,
			Commit:        buildinfo.Commit,
			RepositoryURL: buildinfo.RepositoryURL,
			OS:            runtime.GOOS,
			LogsDir:       filepath.Join(s.paths.Root, "logs"),
		},
		Settings: SettingsView{
			APIID:            s.settings.APIID,
			HasAPIHash:       s.cached.apiHash != "",
			HasBotToken:      s.cached.botToken != "",
			HasProxyPassword: s.cached.proxyPassword != "",
			ProxyEnabled:     s.settings.ProxyEnabled,
			ProxyAddress:     s.settings.ProxyAddress,
			ProxyUsername:    s.settings.ProxyUsername,
			Concurrency:      s.settings.UploadConcurrency,
			LimitKBps:        s.settings.UploadLimitKBps,
			AutoUpdateCheck:  !s.settings.DisableUpdateCheck,
			LastFolder:       s.settings.LastFolder,
		},
		Connection: s.connectionViewLocked(),
		Channel:    ChannelView{ID: s.snapshot.Channel.ID, Title: channelTitle(s.snapshot.Channel)},
		Queue: QueueView{
			Running:        s.snapshot.Running,
			Paused:         s.snapshot.Paused,
			PauseRequested: s.snapshot.PauseRequested,
			ActiveID:       s.snapshot.ActiveID,
			DoneBytes:      s.snapshot.DoneBytes,
			TotalBytes:     s.snapshot.TotalBytes,
			Speed:          s.snapshot.BytesPerSecond,
			ETASeconds:     int64(s.snapshot.ETA / time.Second),
			LastError:      s.snapshot.LastError,
		},
		Move:     MoveView{Active: s.moveActive, DoneBytes: s.moveDone, TotalBytes: s.moveTotal},
		Update:   s.updateViewLocked(),
		Scanning: s.scanning,
	}
	switch {
	case s.settingsLoadErr != nil:
		view.Settings.Problem = "设置文件无法读取，请检查后重新保存：" + s.settingsLoadErr.Error()
	case s.credentialErr != nil:
		view.Settings.Problem = "无法读取系统凭据库，请检查系统权限后重试：" + s.credentialErr.Error()
	}
	if set {
		view.Schedule = ScheduleView{At: at.UnixMilli(), Due: due}
		if due {
			view.Schedule.Waiting = s.scheduleWaitingLocked()
		}
	}
	return view
}

func channelTitle(channel model.Channel) string {
	if channel.ID == 0 {
		return ""
	}
	if title := strings.TrimSpace(channel.Title); title != "" {
		return title
	}
	return "频道 " + formatInt(channel.ID)
}

func (s *Service) scheduleWaitingLocked() string {
	switch {
	case s.snapshot.Running:
		return "队列正在运行"
	case s.snapshot.Paused || s.snapshot.PauseRequested:
		return "等待手动继续"
	case !s.connected:
		return "等待 Telegram 连接"
	case s.snapshot.Channel.ID == 0:
		return "等待绑定频道"
	case !s.stats.Runnable:
		return "等待队列里有待上传的视频"
	case s.moveActive:
		return "等待文件移动完成"
	default:
		return "正在准备开始"
	}
}
