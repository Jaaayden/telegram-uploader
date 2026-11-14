//go:build windows

package shell

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

func init() {
	// The window and its message loop must stay on the thread that created
	// them. Locking in init pins the main goroutine before main.main runs.
	runtime.LockOSThread()
}

const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmActivate      = 0x0006
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmSetIcon       = 0x0080
	wmMoving        = 0x0216
	wmApp           = 0x8000
	waInactive      = 0
	iconSmall       = 0
	iconBig         = 1
	swShow          = 5
	smCxScreen      = 0
	smCyScreen      = 1
	idcArrow        = 32512
	// wsOverlappedWindow is WS_OVERLAPPEDWINDOW.
	wsOverlappedWindow = 0x00CF0000
	// dpiPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4).
	dpiPerMonitorV2 = ^uintptr(3)
	mbIconError     = 0x10
	// dropMessage is what the page posts together with the dropped files.
	dropMessage = "tvu:drop"

	webView2Missing = "没有找到 Microsoft Edge WebView2 运行时，界面无法显示。\n\n" +
		"Windows 11 和已更新的 Windows 10 自带该组件。请从微软官网安装“WebView2 Runtime（常青版）”后重新打开：\n" +
		"https://go.microsoft.com/fwlink/p/?LinkId=2124703"

	// The page talks to Go through __tvuPost. Dropped files are handed over as
	// "additional objects": the only way a WebView2 page can give its host the
	// real paths of dropped files.
	bridgeScript = "window.__tvuPost=function(s){window.chrome.webview.postMessage(s)};" +
		"window.__tvuDropFiles=function(files){" +
		"if(window.chrome.webview.postMessageWithAdditionalObjects){" +
		"window.chrome.webview.postMessageWithAdditionalObjects('" + dropMessage + "',files);return true}return false};"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW            = user32.NewProc("PostThreadMessageW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procUpdateWindow                  = user32.NewProc("UpdateWindow")
	procSetFocus                      = user32.NewProc("SetFocus")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procMessageBoxW                   = user32.NewProc("MessageBoxW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	procIsIconic                      = user32.NewProc("IsIconic")
	procExtractIconExW                = shell32.NewProc("ExtractIconExW")
	procCoInitializeEx                = ole32.NewProc("CoInitializeEx")
	procCoUninitialize                = ole32.NewProc("CoUninitialize")
	procCoCreateInstance              = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree                 = ole32.NewProc("CoTaskMemFree")
	procGetCurrentThreadID            = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW              = kernel32.NewProc("GetModuleHandleW")

	mainWindow atomic.Uintptr
	// The callback must stay referenced for the life of the process.
	windowProcCallback = sync.OnceValue(func() uintptr { return windows.NewCallback(windowProc) })
	// active is only touched on the UI thread, except through its own locks.
	active *windowsWindow
)

var errProcessExited = errors.New("process exited unexpectedly")

type point struct{ x, y int32 }

type message struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type windowClass struct {
	size       uint32
	style      uint32
	windowProc uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSmall  uintptr
}

type minMaxInfo struct {
	reserved     point
	maxSize      point
	maxPosition  point
	minTrackSize point
	maxTrackSize point
}

type windowsWindow struct {
	hwnd       uintptr
	thread     uintptr
	chromium   *edge.Chromium
	options    Options
	minSize    point
	embedded   bool
	forceClose bool
	// trayIcon is the icon shown in the notification area; trayAdded is set
	// while the shell has accepted it.
	trayIcon  uintptr
	trayAdded bool
	// guard keeps a refused WebView2 call from ending the process.
	guard callGuard
	// dead is set when the WebView2 browser process is gone: nothing can be
	// shown or asked any more.
	dead         bool
	reported     map[string]struct{}
	dispatchLock sync.Mutex
	dispatchList []func()
}

// dispatch runs fn on the UI thread.
func (w *windowsWindow) dispatch(fn func()) {
	w.dispatchLock.Lock()
	w.dispatchList = append(w.dispatchList, fn)
	w.dispatchLock.Unlock()
	_, _, _ = procPostThreadMessageW.Call(w.thread, wmApp, 0, 0)
}

// report passes a WebView2 error to the application. A repeated harmless
// error is reported once, so a dead view cannot flood the log.
func (w *windowsWindow) report(operation string, err error, fatal bool) {
	if !fatal {
		key := operation + "\x00" + err.Error()
		if _, seen := w.reported[key]; seen {
			return
		}
		if w.reported == nil {
			w.reported = make(map[string]struct{})
		}
		w.reported[key] = struct{}{}
	}
	if w.options.OnError != nil {
		w.options.OnError(operation, err, fatal)
	}
}

// try runs a call into the binding that may fail without consequences. It
// must be called on the UI thread.
func (w *windowsWindow) try(operation string, fn func()) {
	if w.dead {
		return
	}
	if err := w.guard.run(fn); err != nil {
		w.report(operation, err, false)
	}
}

func (w *windowsWindow) minimized() bool {
	iconic, _, _ := procIsIconic.Call(w.hwnd)
	return iconic != 0
}

// focus moves the keyboard focus into the page. WebView2 refuses this in
// several ordinary situations (a minimized or not yet visible view, a window
// that lost the foreground again); that is never worth more than a log line.
func (w *windowsWindow) focus() {
	if w.dead || !w.embedded || w.minimized() {
		return
	}
	controller := w.chromium.GetController()
	if controller == nil {
		return
	}
	if err := controller.MoveFocus(edge.COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC); err != nil {
		w.report(ErrorFocus, err, false)
	}
}

func (w *windowsWindow) resize() {
	if !w.embedded || w.minimized() {
		return
	}
	w.try(ErrorResize, w.chromium.Resize)
}

func (w *windowsWindow) Eval(script string) {
	w.dispatch(func() { w.try(ErrorEval, func() { w.chromium.Eval(script) }) })
}

func (w *windowsWindow) Close() {
	w.dispatch(func() {
		w.forceClose = true
		_, _, _ = procPostMessageW.Call(w.hwnd, wmClose, 0, 0)
	})
}

func windowProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	w := active
	if w == nil || (w.hwnd != 0 && w.hwnd != hwnd) {
		result, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return result
	}
	// Messages registered by name have no constant values.
	switch {
	case msg == uintptr(showRequestMessage()) && msg != 0:
		w.showFromBackground()
		return 0
	case msg == uintptr(taskbarCreated()) && msg != 0:
		// Explorer restarted and lost the icon.
		w.addTray()
		return 0
	}
	switch msg {
	case wmTray:
		w.handleTray(lParam)
	case wmCommand:
		w.handleCommand(wParam & 0xffff)
	case wmMove, wmMoving:
		if w.embedded && !w.dead {
			_ = w.chromium.NotifyParentWindowPositionChanged()
		}
	case wmSize:
		w.resize()
	case wmActivate:
		// The low word is the activation state; the high word is set while
		// the window is minimized.
		if wParam&0xffff != waInactive && wParam>>16 == 0 {
			w.focus()
		}
	case wmGetMinMaxInfo:
		if w.minSize.x > 0 && w.minSize.y > 0 {
			// lParam carries a pointer owned by the system for this message.
			info := *(**minMaxInfo)(unsafe.Pointer(&lParam))
			info.minTrackSize = w.minSize
		}
	case wmClose:
		if !w.forceClose && w.options.RunInBackground && w.trayAdded {
			// The icon is the way back, so closing the window only hides it.
			w.hideToBackground()
			return 0
		}
		// Ask before abandoning an upload, unless Go already decided to close.
		// A dead page cannot show the question, so it must not block closing.
		if !w.forceClose && !w.dead && w.options.OnCloseRequest != nil && !w.options.OnCloseRequest() {
			return 0
		}
		w.chromium.ShuttingDown()
		_, _, _ = procDestroyWindow.Call(hwnd)
	case wmDestroy:
		w.removeTray()
		_, _, _ = procPostQuitMessage.Call(0)
	default:
		result, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return result
	}
	return 0
}

// dpiScale makes the process DPI aware and returns the system scale factor,
// so the window has the intended size in device-independent pixels.
func dpiScale() float64 {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		_, _, _ = procSetProcessDpiAwarenessContext.Call(dpiPerMonitorV2)
	}
	if procGetDpiForSystem.Find() != nil {
		return 1
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi < 96 {
		return 1
	}
	return float64(dpi) / 96
}

// executableIcons returns the icon embedded in the executable, used for the
// title bar and the taskbar.
func executableIcons() (large, small uintptr) {
	path, err := os.Executable()
	if err != nil {
		return 0, 0
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	_, _, _ = procExtractIconExW.Call(uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
	return large, small
}

// droppedPaths reads the file-system paths of the files the page passed along
// with a drop message.
func droppedPaths(args *edge.ICoreWebView2WebMessageReceivedEventArgs) []string {
	objects, err := args.GetAdditionalObjects()
	if err != nil || objects == nil {
		return nil
	}
	defer objects.Release()
	count, err := objects.GetCount()
	if err != nil {
		return nil
	}
	paths := make([]string, 0, count)
	for index := uint32(0); index < count; index++ {
		value, err := objects.GetValueAtIndex(index)
		if err != nil || value == nil {
			continue
		}
		file := (*edge.ICoreWebView2File)(unsafe.Pointer(value))
		path, err := file.GetPath()
		file.Release()
		if err == nil && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// Run shows the window and blocks until it closes. It must be called from the
// main goroutine.
func Run(options Options) error {
	// Check first: the WebView2 binding exits the process on its own when the
	// runtime is missing, which would leave the user without an explanation.
	if version, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString(""); err != nil || version == "" {
		return errors.New(webView2Missing)
	}
	scale := dpiScale()
	scaled := func(value int) int32 { return int32(float64(value)*scale + 0.5) }

	instance, _, _ := procGetModuleHandleW.Call(0)
	thread, _, _ := procGetCurrentThreadID.Call()
	w := &windowsWindow{thread: thread, options: options, chromium: edge.NewChromium()}
	w.minSize = point{scaled(options.MinWidth), scaled(options.MinHeight)}
	active = w
	defer func() { active = nil }()

	largeIcon, smallIcon := executableIcons()
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	className, _ := windows.UTF16PtrFromString(windowClassName)
	class := windowClass{
		windowProc: windowProcCallback(),
		instance:   instance,
		icon:       largeIcon,
		iconSmall:  smallIcon,
		cursor:     cursor,
		className:  className,
	}
	class.size = uint32(unsafe.Sizeof(class))
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class)))

	width, height := scaled(options.Width), scaled(options.Height)
	screenWidth, _, _ := procGetSystemMetrics.Call(smCxScreen)
	screenHeight, _, _ := procGetSystemMetrics.Call(smCyScreen)
	left := max((int32(screenWidth)-width)/2, 0)
	top := max((int32(screenHeight)-height)/2, 0)
	title, _ := windows.UTF16PtrFromString(options.Title)
	hwnd, _, createErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(left), uintptr(top), uintptr(width), uintptr(height),
		0, 0, instance, 0,
	)
	if hwnd == 0 {
		return errors.New("无法创建窗口：" + createErr.Error())
	}
	w.hwnd = hwnd
	mainWindow.Store(hwnd)
	defer mainWindow.Store(0)
	if largeIcon != 0 {
		_, _, _ = procSendMessageW.Call(hwnd, wmSetIcon, iconBig, largeIcon)
	}
	if smallIcon != 0 {
		_, _, _ = procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, smallIcon)
	}
	w.trayIcon = smallIcon
	w.addTray()
	defer w.removeTray()

	// Show the window before embedding: a WebView2 created inside a hidden
	// parent considers itself invisible and suspends the page's timers.
	_, _, _ = procShowWindow.Call(hwnd, swShow)
	_, _, _ = procUpdateWindow.Call(hwnd)
	_, _, _ = procSetFocus.Call(hwnd)

	chromium := w.chromium
	chromium.DataPath = options.DataDir
	chromium.Debug = options.Debug
	chromium.SetErrorCallback(func(err error) {
		// Inside try this does not return: the call is abandoned, not the
		// process.
		w.guard.fail(err)
		// Otherwise the binding calls os.Exit right after this returns; the
		// application records why and tells the user.
		w.report(ErrorFatal, err, true)
	})
	chromium.ProcessFailedCallback = func(_ *edge.ICoreWebView2, args *edge.ICoreWebView2ProcessFailedEventArgs) {
		kind, err := args.GetProcessFailedKind()
		if err != nil {
			return
		}
		switch kind {
		case edge.COREWEBVIEW2_PROCESS_FAILED_KIND_BROWSER_PROCESS_EXITED:
			// The view is closed for good; uploads keep running.
			w.dead = true
			w.report(ErrorBrowserExited, errProcessExited, false)
		case edge.COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_EXITED:
			// WebView2 shows an error page; load the interface again once this
			// event has been handled. The page asks for its state on load.
			w.report(ErrorRendererExited, errProcessExited, false)
			w.dispatch(func() {
				w.try(ErrorReload, func() { chromium.NavigateToString(options.HTML) })
			})
		}
	}
	chromium.MessageCallback = func(text string, _ *edge.ICoreWebView2, _ *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		if options.OnMessage != nil && text != dropMessage {
			go options.OnMessage(text)
		}
	}
	chromium.MessageWithAdditionalObjectsCallback = func(text string, _ *edge.ICoreWebView2, args *edge.ICoreWebView2WebMessageReceivedEventArgs) {
		if text != dropMessage {
			if options.OnMessage != nil {
				go options.OnMessage(text)
			}
			return
		}
		if paths := droppedPaths(args); len(paths) > 0 && options.OnFileDrop != nil {
			go options.OnFileDrop(paths)
		}
	}
	if !chromium.Embed(hwnd) {
		return errors.New(webView2Missing)
	}
	w.embedded = true
	if settings, err := chromium.GetSettings(); err == nil {
		_ = settings.PutAreDefaultContextMenusEnabled(options.Debug)
		_ = settings.PutAreDevToolsEnabled(options.Debug)
		_ = settings.PutIsStatusBarEnabled(false)
		_ = settings.PutIsZoomControlEnabled(false)
	}
	chromium.Init(bridgeScript)
	w.resize()
	_ = chromium.Show()
	chromium.NavigateToString(options.HTML)
	w.focus()
	if options.OnReady != nil {
		options.OnReady(w)
	}

	var msg message
	for {
		result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(result) <= 0 { // WM_QUIT or an error
			return nil
		}
		if msg.hwnd == 0 && msg.message == wmApp {
			w.dispatchLock.Lock()
			pending := w.dispatchList
			w.dispatchList = nil
			w.dispatchLock.Unlock()
			for _, fn := range pending {
				fn()
			}
			continue
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// ShowError shows a modal message box. It works without a window.
func ShowError(title, text string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	textPtr, _ := windows.UTF16PtrFromString(text)
	_, _, _ = procMessageBoxW.Call(mainWindow.Load(), uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), mbIconError)
}

// The choosers are the Vista-style IFileOpenDialog, driven through its COM
// vtable so no cgo or extra dependency is needed.
var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog   = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	clsctxInprocServer      = 0x1
	fosPickFolders          = 0x20
	fosForceFileSystem      = 0x40
	fosAllowMultiSelect     = 0x200
	fosPathMustExist        = 0x800
	fosFileMustExist        = 0x1000
	sigdnFileSysPath        = 0x80058000
	hresultCancelled        = 0x800704C7

	// Vtable slots: IUnknown (0-2), IModalWindow::Show (3), IFileDialog
	// (4-26), IFileOpenDialog::GetResults (27).
	vtblRelease      = 2
	vtblShow         = 3
	vtblSetFileTypes = 4
	vtblSetOptions   = 9
	vtblGetOptions   = 10
	vtblSetTitle     = 17
	vtblGetResult    = 20
	vtblGetResults   = 27
	// IShellItem and IShellItemArray.
	vtblItemGetDisplayName = 5
	vtblArrayGetCount      = 7
	vtblArrayGetItemAt     = 8
)

type comObject struct{ vtbl *[32]uintptr }

func (o *comObject) call(slot int, args ...uintptr) uint32 {
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	result, _, _ := syscall.SyscallN(o.vtbl[slot], all...)
	return uint32(result)
}

func failed(hresult uint32) bool { return hresult&0x80000000 != 0 }

type filterSpec struct {
	name *uint16
	spec *uint16
}

// PickFolder shows the system folder chooser and returns "" when the user
// cancels. It must not be called from the UI thread.
func PickFolder(title string) (string, error) {
	paths, err := runDialog(title, true)
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

// PickFiles shows the system file chooser for one or more MP4 files and
// returns an empty list when the user cancels. It must not be called from the
// UI thread.
func PickFiles(title string) ([]string, error) {
	return runDialog(title, false)
}

func runDialog(title string, folder bool) ([]string, error) {
	type outcome struct {
		paths []string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		// COM apartments are per thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		paths, err := dialogOnThread(title, folder)
		done <- outcome{paths, err}
	}()
	result := <-done
	return result.paths, result.err
}

func itemPath(item *comObject) string {
	var name *uint16
	if failed(item.call(vtblItemGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name)))) || name == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	return windows.UTF16PtrToString(name)
}

func dialogOnThread(title string, folder bool) ([]string, error) {
	unavailable := errors.New("无法打开选择窗口")
	hresult, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded|coinitDisableOLE1DDE)
	if failed(uint32(hresult)) {
		return nil, unavailable
	}
	defer procCoUninitialize.Call()

	var dialog *comObject
	hresult, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dialog)),
	)
	if failed(uint32(hresult)) || dialog == nil {
		return nil, unavailable
	}
	defer dialog.call(vtblRelease)

	var flags uint32
	if failed(dialog.call(vtblGetOptions, uintptr(unsafe.Pointer(&flags)))) {
		return nil, unavailable
	}
	flags |= fosForceFileSystem | fosPathMustExist
	if folder {
		flags |= fosPickFolders
	} else {
		flags |= fosAllowMultiSelect | fosFileMustExist
		name, _ := windows.UTF16PtrFromString("MP4 视频 (*.mp4)")
		spec, _ := windows.UTF16PtrFromString("*.mp4")
		filters := [1]filterSpec{{name, spec}}
		dialog.call(vtblSetFileTypes, 1, uintptr(unsafe.Pointer(&filters[0])))
		runtime.KeepAlive(filters)
	}
	if failed(dialog.call(vtblSetOptions, uintptr(flags))) {
		return nil, unavailable
	}
	if titlePtr, err := windows.UTF16PtrFromString(title); err == nil {
		dialog.call(vtblSetTitle, uintptr(unsafe.Pointer(titlePtr)))
	}
	switch shown := dialog.call(vtblShow, mainWindow.Load()); {
	case shown == hresultCancelled:
		return nil, nil
	case failed(shown):
		return nil, unavailable
	}

	if folder {
		var item *comObject
		if failed(dialog.call(vtblGetResult, uintptr(unsafe.Pointer(&item)))) || item == nil {
			return nil, unavailable
		}
		defer item.call(vtblRelease)
		if path := itemPath(item); path != "" {
			return []string{path}, nil
		}
		return nil, unavailable
	}

	var items *comObject
	if failed(dialog.call(vtblGetResults, uintptr(unsafe.Pointer(&items)))) || items == nil {
		return nil, unavailable
	}
	defer items.call(vtblRelease)
	var count uint32
	if failed(items.call(vtblArrayGetCount, uintptr(unsafe.Pointer(&count)))) {
		return nil, unavailable
	}
	paths := make([]string, 0, count)
	for index := uint32(0); index < count; index++ {
		var item *comObject
		if failed(items.call(vtblArrayGetItemAt, uintptr(index), uintptr(unsafe.Pointer(&item)))) || item == nil {
			continue
		}
		if path := itemPath(item); path != "" {
			paths = append(paths, path)
		}
		item.call(vtblRelease)
	}
	return paths, nil
}
