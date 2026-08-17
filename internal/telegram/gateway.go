package telegram

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/gotd/td/session"

	"github.com/jayden/telegram-video-uploader/internal/model"
)

var (
	ErrNotConnected       = errors.New("尚未连接 Telegram")
	ErrAlreadyRunning     = errors.New("Telegram 客户端已经在运行")
	ErrWrongBotSession    = errors.New("保存的会话属于另一个 Bot，请清除会话后重试")
	ErrChannelNotWritable = errors.New("Bot 没有该频道的发帖权限")
	ErrUploadData         = errors.New("视频数据上传未完成")
	ErrSendOutcomeUnknown = errors.New("频道消息的最终状态未知")
)

type ProxyConfig struct {
	Address  string
	Username string
	Password string
}

type Config struct {
	AppID          int
	APIHash        string
	BotToken       string
	Proxy          *ProxyConfig
	SessionStorage session.Storage
	// UploadConcurrency controls the number of parts transferred in parallel
	// for one video. Zero and unsupported values use the balanced default.
	UploadConcurrency int
}

const (
	// UploadConcurrencyCompatibility minimizes simultaneous Telegram
	// connections for restrictive proxies and unstable networks.
	UploadConcurrencyCompatibility = 4
	// UploadConcurrencyBalanced is the default. With 512 KiB parts it keeps up
	// to 4 MiB in flight, which is a better fit for high-bandwidth/high-latency
	// links than the previous four-part limit.
	UploadConcurrencyBalanced = 8
	// UploadConcurrencyFast trades additional connections and memory for a
	// larger in-flight window. The UI deliberately exposes no unbounded value.
	UploadConcurrencyFast    = 12
	DefaultUploadConcurrency = UploadConcurrencyBalanced
)

type ConnectionState string

const (
	StateConnecting   ConnectionState = "connecting"
	StateReady        ConnectionState = "ready"
	StateDisconnected ConnectionState = "disconnected"
)

type Identity struct {
	BotID          int64
	Username       string
	MaxUploadBytes int64
	MaxUploadExact bool
}

type BindingEvent struct {
	Channel model.Channel
	Err     error
}

type Events struct {
	OnConnectionState func(ConnectionState)
	OnFloodWait       func(time.Duration)
}

type UploadRequest struct {
	Channel  model.Channel
	Path     string
	File     *os.File
	Name     string
	Caption  string
	RandomID int64
	Metadata model.VideoMetadata
	// BeforeSend runs after every file part is confirmed but before the
	// messages.sendMedia request. Returning an error guarantees that no channel
	// message is submitted.
	BeforeSend func() error
}

// Gateway is the transport boundary used by the queue controller. The real
// implementation uses MTProto; tests can provide a deterministic fake.
type Gateway interface {
	Run(context.Context) error
	WaitReady(context.Context) (Identity, error)
	BeginChannelBinding() (string, error)
	BindingEvents() <-chan BindingEvent
	ValidateChannel(context.Context, model.Channel) (model.Channel, error)
	UploadVideo(context.Context, UploadRequest, func(model.Progress)) (int, error)
}
