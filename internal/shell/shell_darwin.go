//go:build darwin && cgo

package shell

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>

void tvuRun(const char *title, int width, int height, int minWidth, int minHeight, const char *html, int debug, int background);
char *tvuSelfTestBackground(void);
void tvuRequestQuitAsync(void);
void tvuEval(const char *script);
void tvuClose(void);
char *tvuPickFolder(const char *title);
char *tvuPickFiles(const char *title);
void tvuAlert(const char *title, const char *message);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"unsafe"
)

func init() {
	// AppKit must run on the thread the process started on. Locking in init
	// pins the main goroutine to it before main.main runs.
	runtime.LockOSThread()
}

var (
	activeMu sync.Mutex
	active   *Options
)

type darwinWindow struct{}

func (darwinWindow) Eval(script string) {
	text := C.CString(script)
	defer C.free(unsafe.Pointer(text))
	C.tvuEval(text) // copies the script and returns immediately
}

func (darwinWindow) Close() { C.tvuClose() }

// Run shows the window and blocks until it closes. It must be called from the
// main goroutine.
func Run(options Options) error {
	activeMu.Lock()
	if active != nil {
		activeMu.Unlock()
		return errors.New("shell: a window is already running")
	}
	active = &options
	activeMu.Unlock()
	defer func() {
		activeMu.Lock()
		active = nil
		activeMu.Unlock()
	}()

	title := C.CString(options.Title)
	html := C.CString(options.HTML)
	defer C.free(unsafe.Pointer(title))
	defer C.free(unsafe.Pointer(html))
	debug := C.int(0)
	if options.Debug {
		debug = 1
	}
	background := C.int(0)
	if options.RunInBackground {
		background = 1
	}
	C.tvuRun(title, C.int(options.Width), C.int(options.Height), C.int(options.MinWidth), C.int(options.MinHeight), html, debug, background)
	return nil
}

// AcquireSingleInstance reports whether another instance already runs. On
// macOS, opening the application again makes LaunchServices reactivate the
// running one, so nothing needs to be done here.
func AcquireSingleInstance(string) bool { return false }

// SelfTestBackground checks the background behaviour of a running window:
// closing hides it and keeps the menu bar item, showing brings it back. It
// ends by requesting a quit the way the menu bar item does, so the
// application is expected to exit shortly after it returns nil.
func SelfTestBackground(Window) error {
	if failure := C.tvuSelfTestBackground(); failure != nil {
		defer C.free(unsafe.Pointer(failure))
		if message := C.GoString(failure); message != "no-tray" {
			return errors.New(message)
		}
		return ErrNoTray
	}
	C.tvuRequestQuitAsync()
	return nil
}

func current() *Options {
	activeMu.Lock()
	defer activeMu.Unlock()
	return active
}

//export tvuGoMessage
func tvuGoMessage(message *C.char) {
	options := current()
	if options == nil || options.OnMessage == nil {
		return
	}
	text := C.GoString(message)
	go options.OnMessage(text)
}

// tvuGoDrop receives a JSON array of dropped file-system paths.
//
//export tvuGoDrop
func tvuGoDrop(encoded *C.char) {
	options := current()
	if options == nil || options.OnFileDrop == nil {
		return
	}
	var paths []string
	if err := json.Unmarshal([]byte(C.GoString(encoded)), &paths); err != nil || len(paths) == 0 {
		return
	}
	go options.OnFileDrop(paths)
}

//export tvuGoCloseRequest
func tvuGoCloseRequest() C.int {
	options := current()
	if options == nil || options.OnCloseRequest == nil || options.OnCloseRequest() {
		return 1
	}
	return 0
}

//export tvuGoReady
func tvuGoReady() {
	if options := current(); options != nil && options.OnReady != nil {
		options.OnReady(darwinWindow{})
	}
}

// PickFolder shows the system folder chooser and returns "" when the user
// cancels. It must not be called from the UI thread.
func PickFolder(title string) (string, error) {
	text := C.CString(title)
	defer C.free(unsafe.Pointer(text))
	result := C.tvuPickFolder(text)
	if result == nil {
		return "", nil
	}
	defer C.free(unsafe.Pointer(result))
	return C.GoString(result), nil
}

// PickFiles shows the system file chooser for one or more MP4 files and
// returns an empty list when the user cancels. It must not be called from the
// UI thread.
func PickFiles(title string) ([]string, error) {
	text := C.CString(title)
	defer C.free(unsafe.Pointer(text))
	result := C.tvuPickFiles(text)
	if result == nil {
		return nil, nil
	}
	defer C.free(unsafe.Pointer(result))
	var paths []string
	if err := json.Unmarshal([]byte(C.GoString(result)), &paths); err != nil {
		return nil, err
	}
	return paths, nil
}

// ShowError shows a modal alert. It works before Run and must be called from
// the main goroutine.
func ShowError(title, message string) {
	cTitle := C.CString(title)
	cMessage := C.CString(message)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cMessage))
	C.tvuAlert(cTitle, cMessage)
}
