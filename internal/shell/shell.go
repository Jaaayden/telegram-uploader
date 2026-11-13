// Package shell hosts the interface in the operating system's own web view
// (WKWebView on macOS, WebView2 on Windows). Nothing is served over the
// network: the page is loaded from a string and talks to Go through the web
// view's native message channel.
//
// Page → Go: window.__tvuPost(string)
// Dropped files: the page shows its own drop hint from the DOM drag events;
// the real paths never reach the page and arrive through OnFileDrop.
// Go → page: Window.Eval, which the application uses to call
// window.__tvuReceive(...)
package shell

import (
	"errors"
	"os/exec"
	"runtime"
)

// ErrUnsupported is returned on platforms without a window implementation.
var ErrUnsupported = errors.New("shell: this platform has no window support")

// Options describes the single application window.
type Options struct {
	Title               string
	Width, Height       int
	MinWidth, MinHeight int
	// HTML is the complete, self-contained page.
	HTML string
	// DataDir stores the web view's profile (used on Windows).
	DataDir string
	// Debug enables the web inspector.
	Debug bool
	// OnMessage receives strings posted by the page. It is called on its own
	// goroutine and may block.
	OnMessage func(message string)
	// OnFileDrop receives the paths of files and folders dragged onto the
	// window. It is called on its own goroutine.
	OnFileDrop func(paths []string)
	// OnCloseRequest runs on the UI thread when the user closes the window or
	// quits. Returning false keeps the window open.
	OnCloseRequest func() bool
	// OnReady runs once the window exists, before the event loop starts.
	OnReady func(Window)
	// OnError receives failures of the embedded web view (Windows). operation
	// is one of the Error constants. It runs on the UI thread. With fatal the
	// process ends as soon as it returns; otherwise the window keeps working
	// and OnError must not block.
	OnError func(operation string, err error, fatal bool)
}

// Operations reported to Options.OnError.
const (
	// ErrorFatal is a failure the web view cannot continue after.
	ErrorFatal = "fatal"
	// ErrorBrowserExited means the web view's browser process is gone: the
	// window stays blank until the application is restarted.
	ErrorBrowserExited = "browser-exited"
	// ErrorRendererExited means the page's process ended; it is reloaded.
	ErrorRendererExited = "renderer-exited"
	ErrorReload         = "reload"
	ErrorFocus          = "focus"
	ErrorResize         = "resize"
	ErrorEval           = "eval"
)

// Window is the handle given to OnReady. Its methods may be called from any
// goroutine.
type Window interface {
	// Eval runs JavaScript in the page.
	Eval(script string)
	// Close closes the window without asking OnCloseRequest and ends Run.
	Close()
}

// OpenPath reveals a local directory in the system file manager.
func OpenPath(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("/usr/bin/open", path).Start()
	case "windows":
		return exec.Command("explorer.exe", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// OpenURL opens a web page in the default browser.
func OpenURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("/usr/bin/open", url).Start()
	case "windows":
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
