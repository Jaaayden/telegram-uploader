package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const settingsSchemaVersion = 1

type Paths struct {
	Root     string
	Settings string
	Queue    string
	Session  string
}

func DefaultPaths() (Paths, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("读取系统配置目录失败：%w", err)
	}
	root := filepath.Join(configDir, "TelegramVideoUploader")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Paths{}, fmt.Errorf("创建应用数据目录失败：%w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return Paths{}, fmt.Errorf("保护应用数据目录失败：%w", err)
	}
	return Paths{
		Root:     root,
		Settings: filepath.Join(root, "settings.json"),
		Queue:    filepath.Join(root, "queue.json"),
		Session:  filepath.Join(root, "telegram-session.enc"),
	}, nil
}

type Settings struct {
	APIID              int    `json:"api_id"`
	ProxyEnabled       bool   `json:"proxy_enabled"`
	ProxyAddress       string `json:"proxy_address,omitempty"`
	ProxyUsername      string `json:"proxy_username,omitempty"`
	LastFolder         string `json:"last_folder,omitempty"`
	ScheduledStartUnix int64  `json:"scheduled_start_unix,omitempty"`
}

type settingsDocument struct {
	SchemaVersion int      `json:"schema_version"`
	Settings      Settings `json:"settings"`
}

func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("读取设置失败：%w", err)
	}
	var document settingsDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return Settings{}, fmt.Errorf("解析设置失败：%w", err)
	}
	if document.SchemaVersion != settingsSchemaVersion {
		return Settings{}, fmt.Errorf("不支持的设置文件版本：%d", document.SchemaVersion)
	}
	return document.Settings, nil
}

func SaveSettings(path string, settings Settings) error {
	payload, err := json.MarshalIndent(settingsDocument{
		SchemaVersion: settingsSchemaVersion,
		Settings:      settings,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码设置失败：%w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建设置目录失败：%w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时设置文件失败：%w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换设置文件失败：%w", err)
	}
	committed = true
	if err := syncSettingsDirectory(dir); err != nil {
		return fmt.Errorf("同步设置目录失败：%w", err)
	}
	return nil
}

func syncSettingsDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		message := strings.ToLower(syncErr.Error())
		if !strings.Contains(message, "invalid argument") &&
			!strings.Contains(message, "not supported") &&
			!strings.Contains(message, "function not implemented") {
			return syncErr
		}
	}
	return closeErr
}
