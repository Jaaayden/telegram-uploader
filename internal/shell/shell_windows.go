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

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

func init() {
	// The window and its message loop must stay on the thread that created
	// them. Locking in init pins the main goroutine before main.main runs.
	runtime.LockOSThread()
}

const (
	wmClose   = 0x0010
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1
	// gwlpWndProc is GWLP_WNDPROC (-4) as an unsigned word.
	gwlpWndProc = ^uintptr(3)
	// dpiPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4).
	dpiPerMonitorV2 = ^uintptr(3)
	mbIconError     = 0x10

	webView2Missing = "没有找到 Microsoft Edge WebView2 运行时，界面无法显示。\n\n" +
		"Windows 11 和已更新的 Windows 10 自带该组件。请从微软官网安装“WebView2 Runtime（常青版）”后重新打开：\n" +
		"https://go.microsoft.com/fwlink/p/?LinkId=2124703"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")

	procSetWindowLongPtrW             = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW               = user32.NewProc("CallWindowProcW")
	procSendMessageW                  = user32.NewProc("SendMessageW")
	procMessageBoxW                   = user32.NewProc("MessageBoxW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	procExtractIconExW                = shell32.NewProc("ExtractIconExW")
	procCoInitializeEx                = ole32.NewProc("CoInitializeEx")
	procCoUninitialize                = ole32.NewProc("CoUninitialize")
	procCoCreateInstance              = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree                 = ole32.NewProc("CoTaskMemFree")

	mainWindow   atomic.Uintptr
	forceClose   atomic.Bool
	previousProc uintptr
	closeRequest func() bool
	// The callback must stay referenced for the life of the process.
	subclassProc = sync.OnceValue(func() uintptr { return windows.NewCallback(windowProc) })
)

type windowsWindow struct{ view webview2.WebView }

func (w windowsWindow) Eval(script string) {
	w.view.Dispatch(func() { w.view.Eval(script) })
}

func (w windowsWindow) Close() {
	w.view.Dispatch(func() {
		forceClose.Store(true)
		w.view.Destroy()
	})
}

func windowProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if message == wmClose && !forceClose.Load() && closeRequest != nil && !closeRequest() {
		return 0
	}
	result, _, _ := procCallWindowProcW.Call(previousProc, hwnd, message, wParam, lParam)
	return result
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

// setWindowIcon reuses the icon embedded in the executable for the title bar
// and the taskbar.
func setWindowIcon(hwnd uintptr) {
	path, err := os.Executable()
	if err != nil {
		return
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	var large, small uintptr
	_, _, _ = procExtractIconExW.Call(uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
	if large != 0 {
		_, _, _ = procSendMessageW.Call(hwnd, wmSetIcon, iconBig, large)
	}
	if small != 0 {
		_, _, _ = procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, small)
	}
}

// Run shows the window and blocks until it closes. It must be called from the
// main goroutine.
func Run(options Options) error {
	scale := dpiScale()
	scaled := func(value int) int { return int(float64(value)*scale + 0.5) }
	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     options.Debug,
		DataPath:  options.DataDir,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  options.Title,
			Width:  uint(scaled(options.Width)),
			Height: uint(scaled(options.Height)),
			Center: true,
		},
	})
	if view == nil {
		return errors.New(webView2Missing)
	}
	hwnd := uintptr(view.Window())
	mainWindow.Store(hwnd)
	defer mainWindow.Store(0)
	view.SetSize(scaled(options.MinWidth), scaled(options.MinHeight), webview2.HintMin)
	setWindowIcon(hwnd)

	// Intercept the close button so an upload is not abandoned silently.
	closeRequest = options.OnCloseRequest
	previousProc, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, subclassProc())

	if options.OnMessage != nil {
		if err := view.Bind("__tvuPost", func(message string) { go options.OnMessage(message) }); err != nil {
			return err
		}
	}
	view.SetHtml(options.HTML)
	if options.OnReady != nil {
		options.OnReady(windowsWindow{view: view})
	}
	view.Run()
	return nil
}

// ShowError shows a modal message box. It works without a window.
func ShowError(title, message string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	messagePtr, _ := windows.UTF16PtrFromString(message)
	_, _, _ = procMessageBoxW.Call(mainWindow.Load(), uintptr(unsafe.Pointer(messagePtr)), uintptr(unsafe.Pointer(titlePtr)), mbIconError)
}

// The folder chooser is the Vista-style IFileOpenDialog, driven through its
// COM vtable so no cgo or extra dependency is needed.
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
	fosPathMustExist        = 0x800
	sigdnFileSysPath        = 0x80058000
	hresultCancelled        = 0x800704C7

	// Vtable slots: IUnknown (0-2), IModalWindow::Show (3), IFileDialog (4-26).
	vtblRelease        = 2
	vtblShow           = 3
	vtblSetOptions     = 9
	vtblGetOptions     = 10
	vtblSetTitle       = 17
	vtblGetResult      = 20
	vtblGetDisplayName = 5 // IShellItem
)

type comObject struct{ vtbl *[32]uintptr }

func (o *comObject) call(slot int, args ...uintptr) uint32 {
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	result, _, _ := syscall.SyscallN(o.vtbl[slot], all...)
	return uint32(result)
}

func failed(hresult uint32) bool { return hresult&0x80000000 != 0 }

// PickFolder shows the system folder chooser and returns "" when the user
// cancels. It must not be called from the UI thread.
func PickFolder(title string) (string, error) {
	type outcome struct {
		path string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		// COM apartments are per thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		path, err := pickFolderOnThread(title)
		done <- outcome{path, err}
	}()
	result := <-done
	return result.path, result.err
}

func pickFolderOnThread(title string) (string, error) {
	unavailable := errors.New("无法打开文件夹选择窗口")
	hresult, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded|coinitDisableOLE1DDE)
	if failed(uint32(hresult)) {
		return "", unavailable
	}
	defer procCoUninitialize.Call()

	var dialog *comObject
	hresult, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dialog)),
	)
	if failed(uint32(hresult)) || dialog == nil {
		return "", unavailable
	}
	defer dialog.call(vtblRelease)

	var options uint32
	if failed(dialog.call(vtblGetOptions, uintptr(unsafe.Pointer(&options)))) {
		return "", unavailable
	}
	if failed(dialog.call(vtblSetOptions, uintptr(options|fosPickFolders|fosForceFileSystem|fosPathMustExist))) {
		return "", unavailable
	}
	if titlePtr, err := windows.UTF16PtrFromString(title); err == nil {
		dialog.call(vtblSetTitle, uintptr(unsafe.Pointer(titlePtr)))
	}
	switch shown := dialog.call(vtblShow, mainWindow.Load()); {
	case shown == hresultCancelled:
		return "", nil
	case failed(shown):
		return "", unavailable
	}

	var item *comObject
	if failed(dialog.call(vtblGetResult, uintptr(unsafe.Pointer(&item)))) || item == nil {
		return "", unavailable
	}
	defer item.call(vtblRelease)
	var name *uint16
	if failed(item.call(vtblGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name)))) || name == nil {
		return "", unavailable
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	return windows.UTF16PtrToString(name), nil
}
