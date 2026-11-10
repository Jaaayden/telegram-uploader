package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Jaaayden/telegram-uploader/internal/fsutil"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

const settingsSchemaVersion = 1

// DataDirEnv overrides the application data directory. It exists for
// development, demos and automated smoke tests; end users never need it.
const DataDirEnv = "TVU_DATA_DIR"

type Paths struct {
	Root     string
	Settings string
	Queue    string
	Session  string
}

func DefaultPaths() (Paths, error) {
	root := os.Getenv(DataDirEnv)
	if root == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("读取系统配置目录失败：%w", err)
		}
		root = filepath.Join(configDir, "TelegramVideoUploader")
	}
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
	UploadConcurrency  int    `json:"upload_concurrency"`
	// UploadLimitKBps caps the upload rate in KiB per second. Zero means
	// unlimited, which is also what settings files from older versions decode to.
	UploadLimitKBps int `json:"upload_limit_kbps,omitempty"`
	// DisableUpdateCheck turns off the automatic update check. It is stored
	// negated so that older settings files keep the default (checking enabled).
	DisableUpdateCheck bool `json:"disable_update_check,omitempty"`
}

// MaxUploadLimitKBps bounds the configurable limit (about 1 GiB/s) so a typo
// cannot overflow the byte-rate arithmetic.
const MaxUploadLimitKBps = 1 << 20

type settingsDocument struct {
	SchemaVersion int      `json:"schema_version"`
	Settings      Settings `json:"settings"`
}

func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalizeSettings(Settings{}), nil
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
	return normalizeSettings(document.Settings), nil
}

func SaveSettings(path string, settings Settings) error {
	settings = normalizeSettings(settings)
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
	if err := fsutil.AtomicWriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("保存设置文件失败：%w", err)
	}
	return nil
}

func normalizeSettings(settings Settings) Settings {
	if settings.UploadLimitKBps < 0 {
		settings.UploadLimitKBps = 0
	}
	if settings.UploadLimitKBps > MaxUploadLimitKBps {
		settings.UploadLimitKBps = MaxUploadLimitKBps
	}
	switch settings.UploadConcurrency {
	case tgtransport.UploadConcurrencyCompatibility,
		tgtransport.UploadConcurrencyBalanced,
		tgtransport.UploadConcurrencyFast:
		return settings
	default:
		settings.UploadConcurrency = tgtransport.DefaultUploadConcurrency
		return settings
	}
}
