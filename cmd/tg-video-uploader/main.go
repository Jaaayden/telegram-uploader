package main

import (
	"fmt"
	"log/slog"
	"os"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/applog"
	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/mover"
	"github.com/Jaaayden/telegram-uploader/internal/queue"
	"github.com/Jaaayden/telegram-uploader/internal/ui"
)

func main() {
	os.Exit(run())
}

func run() int {
	paths, err := coreapp.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化应用目录失败：", err)
		ui.ShowFatal(fmt.Sprintf("初始化应用目录失败：%v", err))
		return 1
	}
	logFile := applog.Init(paths.Root)
	defer func() { _ = logFile.Close() }()
	slog.Info("starting", "version", buildinfo.Version)

	secrets := credentials.NewStore()
	queueStore := queue.NewStore(paths.Queue)
	controller := coreapp.NewController(queueStore, mover.New())

	var notices []error
	if err := controller.Load(); err != nil {
		// The UI can still start without a queue file. The message contains no
		// credentials and gives the user a chance to rescan the folder.
		slog.Error("load upload queue failed", "error", err)
		notices = append(notices, fmt.Errorf("读取上传队列失败：%w", err))
	}

	ui.Run(controller, paths, secrets, notices...)
	return 0
}
