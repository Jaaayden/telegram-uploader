package main

import (
	"fmt"
	"os"

	coreapp "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/credentials"
	"github.com/jayden/telegram-video-uploader/internal/mover"
	"github.com/jayden/telegram-video-uploader/internal/queue"
	"github.com/jayden/telegram-video-uploader/internal/ui"
)

func main() {
	paths, err := coreapp.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化应用目录失败：", err)
		return
	}

	secrets := credentials.NewStore()
	queueStore := queue.NewStore(paths.Queue)
	controller := coreapp.NewController(queueStore, mover.New())
	if err := controller.Load(); err != nil {
		// The UI can still start without a queue file.  The message contains no
		// credentials and gives the user a chance to rescan the folder.
		fmt.Fprintln(os.Stderr, "读取上传队列失败：", err)
	}

	ui.Run(controller, paths, secrets)
}
