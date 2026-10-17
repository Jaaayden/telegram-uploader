package main

import (
	"fmt"
	"os"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
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

	diagnosticSession, diagnosticErr := diagnostics.Start(paths.Root)
	if diagnosticErr != nil {
		// Diagnostics must never become a new startup dependency. The GUI can
		// still run, although Windows users will have less information if this
		// particular run ends unexpectedly.
		fmt.Fprintln(os.Stderr, "初始化诊断日志失败：", diagnosticErr)
	}
	cleanExit := false
	if diagnosticSession != nil {
		_ = diagnosticSession.Logf(
			"application_start name=%q version=%s build=%d repository=%s previous_run_unclean=%t",
			buildinfo.Name,
			buildinfo.Version,
			buildinfo.Build,
			buildinfo.RepositoryURL,
			diagnosticSession.PreviousRunUnclean(),
		)
		defer func() {
			if !cleanExit {
				// Keep run-state.json unclean and the crash sink installed until
				// process exit so the next launch can distinguish this path from a
				// user-confirmed close.
				_ = diagnosticSession.Logf("application_end clean=false")
				return
			}
			if err := diagnosticSession.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "关闭诊断日志失败：", err)
			}
		}()
	}

	secrets := credentials.NewStore()
	queueStore := queue.NewStore(paths.Queue)
	controller := coreapp.NewController(queueStore, mover.New())

	var notices []error
	if err := controller.Load(); err != nil {
		// The UI can still start without a queue file. The message contains no
		// credentials and gives the user a chance to rescan the folder.
		fmt.Fprintln(os.Stderr, "读取上传队列失败：", err)
		_ = diagnostics.LogError(err, "queue_load_failed")
		notices = append(notices, fmt.Errorf("读取上传队列失败：%w", err))
	} else {
		_ = diagnostics.Logf("queue_loaded jobs=%d", len(controller.Snapshot().Jobs))
	}

	cleanExit = ui.Run(controller, paths, secrets, notices...)
	_ = diagnostics.Logf("ui_event_loop_returned clean=%t", cleanExit)
	return 0
}
