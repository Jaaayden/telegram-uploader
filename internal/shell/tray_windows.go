//go:build windows

package shell

import (
	"errors"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowClassName is shared by the window and by a second launch looking for
// the running instance.
const windowClassName = "TelegramVideoUploaderWindow"

const (
	wmNull          = 0x0000
	wmCommand       = 0x0111
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmContextMenu   = 0x007B
	// wmTray is what the notification area icon sends to the window.
	wmTray = wmApp + 2

	nimAdd    = 0
	nimDelete = 2
	nifMsg    = 0x1
	nifIcon   = 0x2
	nifTip    = 0x4

	trayIconID  = 1
	cmdShow     = 1
	cmdQuit     = 2
	mfString    = 0x0
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmBottomAlign = 0x20
	tpmReturnCmd   = 0x100

	swHide    = 0
	swRestore = 9
	idiApp    = 32512

	errorAlreadyExists = windows.ERROR_ALREADY_EXISTS
)

// notifyIconData is NOTIFYICONDATAW (Windows Vista layout).
type notifyIconData struct {
	size            uint32
	hwnd            uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guid            windows.GUID
	balloonIcon     uintptr
}

// notifyIconIdentifier is NOTIFYICONIDENTIFIER.
type notifyIconIdentifier struct {
	size uint32
	hwnd uintptr
	id   uint32
	guid windows.GUID
}

type rect struct{ left, top, right, bottom int32 }

var (
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procShellNotifyIconGetRect   = shell32.NewProc("Shell_NotifyIconGetRect")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procSetMenuDefaultItem       = user32.NewProc("SetMenuDefaultItem")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procFindWindowW              = user32.NewProc("FindWindowW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procLoadIconW                = user32.NewProc("LoadIconW")

	// Messages every instance registers under the same name, so a second
	// launch can talk to the first and the shell can announce its restart.
	showRequestMessage = sync.OnceValue(func() uint32 { return registerMessage("TelegramVideoUploader.ShowWindow") })
	taskbarCreated     = sync.OnceValue(func() uint32 { return registerMessage("TaskbarCreated") })

	singleInstanceMutex windows.Handle
)

func registerMessage(name string) uint32 {
	text, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0
	}
	id, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(text)))
	return uint32(id)
}

func (w *windowsWindow) trayData() notifyIconData {
	data := notifyIconData{
		hwnd:            w.hwnd,
		id:              trayIconID,
		flags:           nifMsg | nifIcon | nifTip,
		callbackMessage: wmTray,
		icon:            w.trayIcon,
	}
	data.size = uint32(unsafe.Sizeof(data))
	if tip, err := windows.UTF16FromString(w.options.Title); err == nil {
		copy(data.tip[:len(data.tip)-1], tip)
	}
	return data
}

// addTray puts the icon into the notification area. It is a no-op unless the
// application runs in the background, and it records whether the shell took
// the icon: only then may closing the window hide it.
func (w *windowsWindow) addTray() {
	if !w.options.RunInBackground {
		return
	}
	if w.trayIcon == 0 {
		w.trayIcon, _, _ = procLoadIconW.Call(0, idiApp)
	}
	data := w.trayData()
	added, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	w.trayAdded = added != 0
}

func (w *windowsWindow) removeTray() {
	if !w.trayAdded {
		return
	}
	data := w.trayData()
	_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
	w.trayAdded = false
}

// hideToBackground hides the window (and with it the taskbar button).
func (w *windowsWindow) hideToBackground() {
	_, _, _ = procShowWindow.Call(w.hwnd, swHide)
}

// showFromBackground brings the window back and to the front.
func (w *windowsWindow) showFromBackground() {
	command := uintptr(swShow)
	if w.minimized() {
		command = swRestore
	}
	_, _, _ = procShowWindow.Call(w.hwnd, command)
	_, _, _ = procSetForegroundWindow.Call(w.hwnd)
	w.resize()
	w.focus()
}

func (w *windowsWindow) visible() bool {
	shown, _, _ := procIsWindowVisible.Call(w.hwnd)
	return shown != 0
}

// requestQuit is the menu's "quit". The application may want the user to
// confirm first, which needs the window on screen.
func (w *windowsWindow) requestQuit() {
	if !w.dead && w.options.OnCloseRequest != nil && !w.options.OnCloseRequest() {
		w.showFromBackground()
		return
	}
	w.forceClose = true
	_, _, _ = procPostMessageW.Call(w.hwnd, wmClose, 0, 0)
}

func (w *windowsWindow) handleCommand(id uintptr) {
	switch id {
	case cmdShow:
		w.showFromBackground()
	case cmdQuit:
		w.requestQuit()
	}
}

func (w *windowsWindow) showTrayMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	show, _ := windows.UTF16PtrFromString("显示窗口")
	quit, _ := windows.UTF16PtrFromString("退出")
	_, _, _ = procAppendMenuW.Call(menu, mfString, cmdShow, uintptr(unsafe.Pointer(show)))
	_, _, _ = procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenuW.Call(menu, mfString, cmdQuit, uintptr(unsafe.Pointer(quit)))
	_, _, _ = procSetMenuDefaultItem.Call(menu, cmdShow, 0)

	var cursor point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	// The menu only closes when clicking elsewhere if its owner is in the
	// foreground first.
	_, _, _ = procSetForegroundWindow.Call(w.hwnd)
	chosen, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmBottomAlign|tpmReturnCmd,
		uintptr(cursor.x), uintptr(cursor.y), 0, w.hwnd, 0)
	_, _, _ = procPostMessageW.Call(w.hwnd, wmNull, 0, 0)
	w.handleCommand(chosen)
}

// handleTray reacts to a mouse event on the icon (lParam is the mouse message).
func (w *windowsWindow) handleTray(event uintptr) {
	switch event & 0xffff {
	case wmLButtonUp, wmLButtonDblClk:
		w.showFromBackground()
	case wmRButtonUp, wmContextMenu:
		w.showTrayMenu()
	}
}

// AcquireSingleInstance reports whether another instance already runs. If so
// it asks that instance to show its window; the caller should then exit.
// When the check itself cannot be made, the application simply starts.
func AcquireSingleInstance(name string) bool {
	mutexName, err := windows.UTF16PtrFromString(`Local\` + name)
	if err != nil {
		return false
	}
	handle, err := windows.CreateMutex(nil, false, mutexName)
	if err == nil {
		// Held until the process ends.
		singleInstanceMutex = handle
		return false
	}
	if !errors.Is(err, errorAlreadyExists) {
		return false
	}
	if handle != 0 {
		_ = windows.CloseHandle(handle)
	}
	class, _ := windows.UTF16PtrFromString(windowClassName)
	// The other instance may still be creating its window.
	for attempt := 0; attempt < 30; attempt++ {
		hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), 0)
		if hwnd != 0 {
			var pid uint32
			_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pid))
			_, _, _ = procPostMessageW.Call(hwnd, uintptr(showRequestMessage()), 0, 0)
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// SelfTestBackground checks the background behaviour of a running window:
// the icon exists, closing hides the window, the icon brings it back. It ends
// by requesting a quit the way the icon's menu does, so the application is
// expected to exit shortly after it returns nil. ErrNoTray means this session
// has no notification area (the window then closes normally).
func SelfTestBackground(window Window) error {
	w, ok := window.(*windowsWindow)
	if !ok || !w.options.RunInBackground {
		return errors.New("the window does not run in the background")
	}
	if !w.trayAdded {
		return ErrNoTray
	}
	identifier := notifyIconIdentifier{hwnd: w.hwnd, id: trayIconID}
	identifier.size = uint32(unsafe.Sizeof(identifier))
	var area rect
	if result, _, _ := procShellNotifyIconGetRect.Call(uintptr(unsafe.Pointer(&identifier)), uintptr(unsafe.Pointer(&area))); result != 0 {
		return errors.New("the shell does not know the notification area icon")
	}
	send := func(message, wParam, lParam uintptr) {
		_, _, _ = procSendMessageW.Call(w.hwnd, message, wParam, lParam)
	}
	send(wmClose, 0, 0)
	if w.visible() {
		return errors.New("closing the window did not hide it")
	}
	send(wmTray, trayIconID, wmLButtonUp)
	if !w.visible() {
		return errors.New("clicking the icon did not show the window")
	}
	send(wmCommand, cmdQuit, 0)
	return nil
}
