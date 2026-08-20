package main

import (
	"fmt"
	"os"

	coreapp "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/buildinfo"
	"github.com/jayden/telegram-video-uploader/internal/credentials"
	"github.com/jayden/telegram-video-uploader/internal/diagnostics"
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
	if err := controller.Load(); err != nil {
		// The UI can still start without a queue file.  The message contains no
		// credentials and gives the user a chance to rescan the folder.
		fmt.Fprintln(os.Stderr, "读取上传队列失败：", err)
		_ = diagnostics.LogError(err, "queue_load_failed")
	} else {
		_ = diagnostics.Logf("queue_loaded jobs=%d", len(controller.Snapshot().Jobs))
	}

	cleanExit = ui.Run(controller, paths, secrets)
	_ = diagnostics.Logf("ui_event_loop_returned clean=%t", cleanExit)
}
