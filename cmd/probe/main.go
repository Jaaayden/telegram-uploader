// Command probe is a development-only protocol check. It is not included in
// end-user release packages.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	appcore "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/credentials"
	"github.com/jayden/telegram-video-uploader/internal/media"
	"github.com/jayden/telegram-video-uploader/internal/model"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "验证失败：", err)
		os.Exit(1)
	}
}

func run() error {
	appID, err := strconv.Atoi(os.Getenv("TG_APP_ID"))
	if err != nil || appID <= 0 {
		return fmt.Errorf("请设置有效的 TG_APP_ID")
	}
	apiHash := os.Getenv("TG_API_HASH")
	botToken := os.Getenv("TG_BOT_TOKEN")
	if apiHash == "" || botToken == "" {
		return fmt.Errorf("请设置 TG_API_HASH 和 TG_BOT_TOKEN")
	}
	paths, err := appcore.DefaultPaths()
	if err != nil {
		return err
	}
	secrets := credentials.NewStore()
	client, err := tgtransport.NewClient(tgtransport.Config{
		AppID:          appID,
		APIHash:        apiHash,
		BotToken:       botToken,
		SessionStorage: credentials.NewSessionStorage(secrets, paths.Session),
	}, tgtransport.Events{
		OnConnectionState: func(state tgtransport.ConnectionState) {
			fmt.Println("连接状态：", state)
		},
		OnFloodWait: func(_ time.Duration) {
			fmt.Println("Telegram 要求等待，验证程序将自动遵守。")
		},
	})
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Run(ctx) }()
	identity, err := client.WaitReady(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Bot 已连接：@%s，当前上限 %.2f GiB\n", identity.Username, float64(identity.MaxUploadBytes)/(1<<30))

	code, err := client.BeginChannelBinding()
	if err != nil {
		return err
	}
	fmt.Println("请在目标频道发送下面这条临时消息：")
	fmt.Println(code)
	var channel model.Channel
	select {
	case event := <-client.BindingEvents():
		if event.Err != nil {
			return event.Err
		}
		channel = event.Channel
		fmt.Println("已绑定频道：", channel.Title)
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	videoPath := os.Getenv("TG_VIDEO")
	if videoPath == "" {
		fmt.Println("未设置 TG_VIDEO，频道绑定验证已完成。")
		return nil
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		return err
	}
	if info.Size() > identity.MaxUploadBytes {
		return fmt.Errorf("测试视频超过 Bot 当前上传上限")
	}
	metadata, err := media.ParseMP4Metadata(videoPath)
	if err != nil {
		return err
	}
	randomID, err := newRandomID()
	if err != nil {
		return err
	}
	name := filepath.Base(videoPath)
	messageID, err := client.UploadVideo(ctx, tgtransport.UploadRequest{
		Channel:  channel,
		Path:     videoPath,
		Name:     name,
		Caption:  name,
		RandomID: randomID,
		Metadata: metadata,
	}, func(progress model.Progress) {
		if progress.BytesTotal > 0 {
			fmt.Printf("\r上传 %.1f%%  %.1f MiB/s", 100*float64(progress.BytesDone)/float64(progress.BytesTotal), progress.BytesPerSecond/(1<<20))
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("\n发送成功，消息 ID：%d\n", messageID)
	return nil
}

func newRandomID() (int64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	id := int64(binary.BigEndian.Uint64(raw[:]) & ((uint64(1) << 63) - 1))
	if id == 0 {
		id = 1
	}
	return id, nil
}
