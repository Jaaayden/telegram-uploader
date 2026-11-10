package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
)

// InitView is the answer to "init": the complete state and queue.
type InitView struct {
	State   StateView  `json:"state"`
	Jobs    *JobsDelta `json:"jobs"`
	Notices []string   `json:"notices"`
}

type idParams struct {
	ID string `json:"id"`
}

func decode[T any](params json.RawMessage) (T, error) {
	var value T
	if len(params) == 0 || string(params) == "null" {
		return value, nil
	}
	if err := json.Unmarshal(params, &value); err != nil {
		return value, fmt.Errorf("请求参数无效：%w", err)
	}
	return value, nil
}

// message wraps a user-facing summary so the front end can show it.
func message(text string, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]string{"message": text}, nil
}

// Handle executes one request from the front end. Every method returns
// quickly except those that wait for a native dialog; long work runs in the
// background and reports through events.
func (s *Service) Handle(method string, params json.RawMessage) (any, error) {
	if s.isClosed() {
		return nil, errors.New("应用正在退出")
	}
	switch method {
	case "init":
		return s.initFrontEnd(), nil

	// Connection and settings.
	case "saveConnection":
		input, err := decode[ConnectionInput](params)
		if err != nil {
			return nil, err
		}
		return nil, s.SaveConnection(input)
	case "reconnect":
		return nil, s.Reconnect()
	case "beginBinding":
		code, err := s.BeginBinding()
		if err != nil {
			return nil, err
		}
		return map[string]string{"code": code}, nil
	case "setConcurrency":
		input, err := decode[struct {
			Value int `json:"value"`
		}](params)
		if err != nil {
			return nil, err
		}
		applied, err := s.SetConcurrency(input.Value)
		if err != nil {
			return nil, err
		}
		return map[string]int{"value": applied}, nil
	case "setUploadLimit":
		input, err := decode[struct {
			KBps int `json:"kbps"`
		}](params)
		if err != nil {
			return nil, err
		}
		return nil, s.SetUploadLimit(input.KBps)

	// Queue.
	case "addFolder":
		return s.AddFolder()
	case "addCandidates":
		input, err := decode[struct {
			Token   string `json:"token"`
			Indexes []int  `json:"indexes"`
		}](params)
		if err != nil {
			return nil, err
		}
		return message(s.AddCandidates(input.Token, input.Indexes))
	case "discardScan":
		input, err := decode[struct {
			Token string `json:"token"`
		}](params)
		if err != nil {
			return nil, err
		}
		s.DiscardScan(input.Token)
		return nil, nil
	case "start":
		return nil, s.StartUploads()
	case "pause":
		return nil, s.PauseUploads()
	case "cancelAll":
		return message(s.CancelAll())
	case "cancelJob", "retryJob", "skipJob", "markSent":
		input, err := decode[idParams](params)
		if err != nil {
			return nil, err
		}
		switch method {
		case "cancelJob":
			return nil, s.controller.CancelJob(input.ID)
		case "retryJob":
			return nil, s.controller.Retry(input.ID)
		case "skipJob":
			return nil, s.controller.Skip(input.ID)
		default:
			return nil, s.controller.MarkSent(input.ID)
		}
	case "removeJobs":
		input, err := decode[struct {
			IDs []string `json:"ids"`
		}](params)
		if err != nil {
			return nil, err
		}
		return message(s.RemoveJobs(input.IDs))
	case "removeCompleted":
		return message(s.RemoveCompleted())
	case "clearQueue":
		return message(s.ClearQueue())
	case "resetJobs":
		input, err := decode[struct {
			Mode string   `json:"mode"`
			IDs  []string `json:"ids"`
		}](params)
		if err != nil {
			return nil, err
		}
		return message(s.ResetJobs(input.Mode, input.IDs))
	case "moveOversize":
		return nil, s.MoveOversize()
	case "cancelMove":
		s.CancelMove()
		return nil, nil

	// Schedule.
	case "setSchedule":
		input, err := decode[struct {
			Unix int64 `json:"unix"`
		}](params)
		if err != nil {
			return nil, err
		}
		return nil, s.SetSchedule(input.Unix)
	case "cancelSchedule":
		return nil, s.CancelSchedule()

	// Updates.
	case "checkUpdate":
		return nil, s.CheckUpdate()
	case "downloadUpdate":
		return nil, s.DownloadUpdate()
	case "cancelUpdateDownload":
		s.CancelUpdateDownload()
		return nil, nil
	case "installUpdate":
		return nil, s.InstallUpdate()
	case "setAutoUpdateCheck":
		input, err := decode[struct {
			Enabled bool `json:"enabled"`
		}](params)
		if err != nil {
			return nil, err
		}
		return nil, s.SetAutoUpdateCheck(input.Enabled)

	// Shell integration. Only fixed, known targets can be opened.
	case "openLogs":
		return nil, s.openLogs()
	case "openLink":
		input, err := decode[struct {
			Target string `json:"target"`
		}](params)
		if err != nil {
			return nil, err
		}
		return nil, s.openLink(input.Target)
	case "quit":
		// The user confirmed closing while something was pending.
		s.Shutdown(true)
		s.host.Quit()
		return nil, nil
	}
	return nil, fmt.Errorf("未知的请求：%s", method)
}

func (s *Service) initFrontEnd() InitView {
	state := s.State()
	s.mu.Lock()
	// Take the newest snapshot so the first paint is never behind the engine.
	s.frontReady = true
	jobs := s.fullJobsLocked()
	notices := s.notices
	s.notices = nil
	s.mu.Unlock()
	if notices == nil {
		notices = []string{}
	}
	s.markHealthyStart()
	return InitView{State: state, Jobs: jobs, Notices: notices}
}

func (s *Service) openLogs() error {
	directory := filepath.Join(s.paths.Root, "logs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建诊断日志目录失败：%w", err)
	}
	if err := s.host.OpenPath(directory); err != nil {
		return fmt.Errorf("打开诊断日志目录失败：%w", err)
	}
	return nil
}

func (s *Service) openLink(target string) error {
	var url string
	switch target {
	case "repository":
		url = buildinfo.RepositoryURL
	case "releases":
		url = buildinfo.RepositoryURL + "/releases/latest"
	case "release":
		s.mu.Lock()
		url = s.update.release.PageURL
		s.mu.Unlock()
		if url == "" {
			url = buildinfo.RepositoryURL + "/releases/latest"
		}
	case "telegram-apps":
		url = "https://my.telegram.org/apps"
	case "botfather":
		url = "https://t.me/BotFather"
	default:
		return errors.New("不支持打开这个链接")
	}
	return s.host.OpenURL(url)
}
