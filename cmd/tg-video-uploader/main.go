// Command tg-video-uploader is the desktop application.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/demo"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/mover"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
	"github.com/Jaaayden/telegram-uploader/internal/queue"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
	"github.com/Jaaayden/telegram-uploader/internal/service"
	"github.com/Jaaayden/telegram-uploader/internal/shell"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
	"github.com/Jaaayden/telegram-uploader/internal/update"
	"github.com/Jaaayden/telegram-uploader/internal/webui"
)

const windowTitle = "Telegram 视频顺序上传器"

type options struct {
	demo    bool
	smoke   bool
	debug   bool
	version bool
	devHTTP string
	waitPID int
}

func main() {
	var opts options
	flag.BoolVar(&opts.version, "version", false, "print the version and exit")
	flag.BoolVar(&opts.demo, "demo", false, "run with sample videos and a simulated Telegram (no credentials, no network)")
	flag.BoolVar(&opts.smoke, "smoke", false, "self-test: open the window in demo mode, check that the interface loads, exit")
	flag.BoolVar(&opts.debug, "debug", false, "enable the web inspector")
	flag.StringVar(&opts.devHTTP, "dev-http", "", "development only: serve the interface on this loopback address instead of opening a window")
	flag.IntVar(&opts.waitPID, "wait-pid", 0, "wait for this process to exit before starting (used when restarting after an update)")
	flag.Parse()
	if opts.version {
		fmt.Println(buildinfo.Version)
		return
	}
	os.Exit(run(opts))
}

func run(opts options) int {
	if opts.waitPID > 0 {
		platform.WaitProcessExit(opts.waitPID, 20*time.Second)
	}
	if opts.smoke {
		opts.demo = true
	}
	if opts.demo {
		// Never touch the real queue, settings or keychain in demo mode.
		directory, err := os.MkdirTemp("", "tvu-demo-*")
		if err != nil {
			return fatal("创建演示目录失败", err)
		}
		defer os.RemoveAll(directory)
		_ = os.Setenv(coreapp.DataDirEnv, filepath.Join(directory, "data"))
	}

	paths, err := coreapp.DefaultPaths()
	if err != nil {
		return fatal("初始化应用目录失败", err)
	}
	session, diagnosticErr := diagnostics.Start(paths.Root)
	if diagnosticErr != nil {
		// Diagnostics must never become a startup dependency.
		fmt.Fprintln(os.Stderr, "初始化诊断日志失败：", diagnosticErr)
	}
	cleanExit := false
	if session != nil {
		_ = session.Logf("application_start name=%q version=%s commit=%s previous_run_unclean=%t",
			buildinfo.Name, buildinfo.Version, buildinfo.Commit, session.PreviousRunUnclean())
		defer func() {
			if !cleanExit {
				// Keep run-state.json unclean so the next launch can tell this
				// apart from a normal close.
				_ = session.Logf("application_end clean=false")
				return
			}
			if err := session.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "关闭诊断日志失败：", err)
			}
		}()
	}

	controller := coreapp.NewController(queue.NewStore(paths.Queue), mover.New())
	var notices []string
	if err := controller.Load(); err != nil {
		// The interface can still start; the message contains no credentials.
		_ = diagnostics.LogError(err, "queue_load_failed")
		notices = append(notices, "读取上传队列失败："+err.Error())
	} else {
		_ = diagnostics.Logf("queue_loaded jobs=%d", len(controller.Snapshot().Jobs))
	}

	host := &desktopHost{}
	serviceOptions := service.Options{
		Controller: controller,
		Paths:      paths,
		Secrets:    credentials.NewStore(),
		Host:       host,
		Notices:    notices,
		Updater: &update.Updater{
			Repository: buildinfo.Repository,
			Current:    buildinfo.Version,
			StageDir:   filepath.Join(paths.Root, "updates"),
		},
	}
	if opts.demo {
		folder, err := seedDemo(controller, paths)
		if err != nil {
			return fatal("准备演示数据失败", err)
		}
		host.demoFolder = folder
		serviceOptions.Secrets = demo.NewSecrets()
		serviceOptions.Updater = nil
		serviceOptions.NewClient = func(config tgtransport.Config, events tgtransport.Events) (service.Client, error) {
			return demo.NewClient(config, events), nil
		}
	}
	app, err := service.New(serviceOptions)
	if err != nil {
		return fatal("启动失败", err)
	}

	if opts.devHTTP != "" {
		host.headless = true
		return serveDev(opts.devHTTP, app)
	}

	smoke := newSmokeCheck(opts.smoke)
	var window atomic.Pointer[shell.Window]
	send := func(message any) {
		target := window.Load()
		if target == nil {
			return
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return
		}
		// encoding/json escapes <, >, & and the line separators, so the result
		// is safe to embed in a script.
		(*target).Eval("window.__tvuReceive(" + string(encoded) + ")")
	}
	app.SetEmitter(func(event string, payload any) {
		send(map[string]any{"event": event, "payload": payload})
	})

	runErr := shell.Run(shell.Options{
		Title:     windowTitle,
		Width:     1080,
		Height:    720,
		MinWidth:  760,
		MinHeight: 520,
		HTML:      webui.Page(),
		DataDir:   filepath.Join(paths.Root, "webview"),
		Debug:     opts.debug,
		OnMessage: func(raw string) {
			var request struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				return
			}
			smoke.stage("message from page: " + request.Method)
			if request.Method == "__error" {
				// A script error in the page: keep it for diagnosis.
				_ = diagnostics.Logf("ui_script_error detail=%s", string(request.Params))
				fmt.Fprintln(os.Stderr, "interface script error:", string(request.Params))
				return
			}
			if smoke.handle(request.Method, request.Params) {
				return
			}
			result, err := app.Handle(request.Method, request.Params)
			if err != nil {
				send(map[string]any{"id": request.ID, "error": err.Error()})
				return
			}
			send(map[string]any{"id": request.ID, "result": result})
			if request.Method == "init" {
				smoke.probe(window.Load())
			}
		},
		OnError: func(operation string, err error, fatal bool) {
			// A system error code says more than its localized text.
			code := uint64(0)
			var errno syscall.Errno
			if errors.As(err, &errno) {
				code = uint64(errno)
			}
			_ = diagnostics.Logf("webview_error operation=%s fatal=%t code=%#x detail=%q", operation, fatal, code, err.Error())
			fmt.Fprintf(os.Stderr, "web view error: %s (fatal=%t): %v\n", operation, fatal, err)
			if opts.smoke {
				// An unattended self-test must fail, not wait on a dialog.
				return
			}
			switch {
			case fatal:
				shell.ShowError(windowTitle, fmt.Sprintf("界面组件（WebView2）出错，应用需要退出。\n\n%v\n\n重新打开后，未完成的上传会从断点继续。", err))
			case operation == shell.ErrorBrowserExited:
				// Not on the UI thread: the window must keep handling messages.
				go shell.ShowError(windowTitle, "界面进程意外退出，窗口无法继续显示。\n\n正在进行的上传不受影响；关闭窗口并重新打开应用即可恢复界面。")
			}
		},
		OnFileDrop:     app.DropPaths,
		OnCloseRequest: app.CloseRequested,
		OnReady: func(created shell.Window) {
			window.Store(&created)
			host.window.Store(&created)
			app.Start()
			smoke.arm(created)
		},
	})
	if runErr != nil {
		_ = diagnostics.LogError(runErr, "window_failed")
		app.Shutdown(false)
		fmt.Fprintln(os.Stderr, runErr)
		if !opts.smoke {
			// An unattended self-test must fail, not wait on a dialog.
			shell.ShowError(windowTitle, runErr.Error())
		}
		return 1
	}
	cleanExit = app.Shutdown(false)
	_ = diagnostics.Logf("ui_event_loop_returned clean=%t", cleanExit)
	return smoke.exitCode()
}

func fatal(context string, err error) int {
	message := fmt.Sprintf("%s：%v", context, err)
	fmt.Fprintln(os.Stderr, message)
	shell.ShowError(windowTitle, message)
	return 1
}

// desktopHost connects the service to the native window.
type desktopHost struct {
	window     atomic.Pointer[shell.Window]
	headless   bool
	demoFolder string
}

func (h *desktopHost) PickFolder(title string) (string, error) {
	if h.headless {
		// A browser cannot show a native folder chooser; development mode
		// uses the demo folder or an explicit override.
		if folder := os.Getenv("TVU_DEV_FOLDER"); folder != "" {
			return folder, nil
		}
		return h.demoFolder, nil
	}
	return shell.PickFolder(title)
}

func (h *desktopHost) PickFiles(title string) ([]string, error) {
	if h.headless {
		// As above: development mode offers the demo folder's files.
		entries, err := filepath.Glob(filepath.Join(h.demoFolder, "*.mp4"))
		if h.demoFolder == "" {
			return nil, nil
		}
		return entries, err
	}
	return shell.PickFiles(title)
}

func (h *desktopHost) OpenPath(path string) error { return shell.OpenPath(path) }
func (h *desktopHost) OpenURL(url string) error   { return shell.OpenURL(url) }

func (h *desktopHost) Quit() {
	if window := h.window.Load(); window != nil {
		(*window).Close()
		return
	}
	if h.headless {
		os.Exit(0)
	}
}

// seedDemo fills the queue with sample videos and returns a second folder for
// the "add folder" flow.
func seedDemo(controller *coreapp.Controller, paths coreapp.Paths) (string, error) {
	videos := filepath.Join(filepath.Dir(paths.Root), "课程视频")
	extras := filepath.Join(filepath.Dir(paths.Root), "番外")
	if err := demo.WriteSamples(videos, demo.QueueSamples); err != nil {
		return "", err
	}
	if err := demo.WriteSamples(extras, demo.FolderSamples); err != nil {
		return "", err
	}
	jobs, err := scanner.Scan(videos, demo.MaxUploadBytes)
	if err != nil {
		return "", err
	}
	if _, err := controller.AddJobs(jobs); err != nil {
		return "", err
	}
	if err := controller.SetChannel(demo.Channel); err != nil {
		return "", err
	}
	if err := coreapp.SaveSettings(paths.Settings, coreapp.Settings{APIID: 1234567, LastFolder: videos}); err != nil {
		return "", err
	}
	return extras, nil
}

// smokeCheck implements --smoke: it proves that the packaged application can
// create its window, load the page and exchange messages in both directions.
type smokeCheck struct {
	enabled bool
	once    sync.Once
	passed  atomic.Bool
	window  atomic.Pointer[shell.Window]
}

func newSmokeCheck(enabled bool) *smokeCheck { return &smokeCheck{enabled: enabled} }

// stage records progress of the self-test, so a failure shows how far it got.
func (s *smokeCheck) stage(text string) {
	if s.enabled {
		fmt.Fprintln(os.Stderr, "smoke:", text)
	}
}

func (s *smokeCheck) arm(window shell.Window) {
	if !s.enabled {
		return
	}
	s.stage("window created")
	s.window.Store(&window)
	time.AfterFunc(45*time.Second, func() {
		fmt.Fprintln(os.Stderr, "SMOKE FAIL: the interface did not report within 45 seconds")
		window.Close()
	})
}

// probe asks the page to report what it rendered once the queue is visible.
func (s *smokeCheck) probe(window *shell.Window) {
	if !s.enabled || window == nil {
		return
	}
	s.stage("page requested its state; asking it to report")
	(*window).Eval(`(function(){var n=0,t=setInterval(function(){var rows=document.querySelectorAll('.row-item').length;` +
		`if(rows>0||++n>100){clearInterval(t);window.__tvuPost(JSON.stringify({id:0,method:'__smoke',params:{rows:rows,` +
		`title:document.getElementById('connTitle').textContent}}));}},100);})()`)
}

func (s *smokeCheck) handle(method string, params json.RawMessage) bool {
	if method != "__smoke" {
		return false
	}
	if !s.enabled {
		return true
	}
	s.once.Do(func() {
		var report struct {
			Rows  int    `json:"rows"`
			Title string `json:"title"`
		}
		_ = json.Unmarshal(params, &report)
		if report.Rows > 0 {
			s.passed.Store(true)
			fmt.Printf("SMOKE OK: %d queue rows rendered, status %q\n", report.Rows, report.Title)
		} else {
			fmt.Fprintln(os.Stderr, "SMOKE FAIL: the page loaded but rendered no queue rows")
		}
		if window := s.window.Load(); window != nil {
			(*window).Close()
		}
	})
	return true
}

func (s *smokeCheck) exitCode() int {
	if s.enabled && !s.passed.Load() {
		return 1
	}
	return 0
}
