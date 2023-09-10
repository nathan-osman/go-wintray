package wintray

import (
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	pWMAPP_NOTIFYCALLBACK = iota + win.WM_APP + 1
	pWMAPP_MESSAGE

	pMESSAGE_SET_ICON = iota
	pMESSAGE_SET_TIP
	pMESSAGE_ADD_MENU_ITEM
)

var (
	newIconId = atomic.Uint32{}

	user32          = windows.MustLoadDLL("User32.dll")
	pAppendMenuW    = user32.MustFindProc("AppendMenuW")
	pGetShellWindow = user32.MustFindProc("GetShellWindow")
)

type pMessage struct {
	Type int
	Data any
}

type pDataAddMenuItem struct {
	Text string
	Fn   func()
}

// WinTray provides a single icon in the system tray. A separate goroutine is
// used for running all of the API functions
type WinTray struct {
	threadId    uint32
	hwnd        win.HWND
	messageChan chan *pMessage
	closedChan  chan any
}

func mustUTF16FromString(v string) []uint16 {
	p, err := syscall.UTF16FromString(v)
	if err != nil {
		panic(err)
	}
	return p
}

func mustUTF16PtrFromString(v string) *uint16 {
	p, err := syscall.UTF16PtrFromString(v)
	if err != nil {
		panic(err)
	}
	return p
}

func (w *WinTray) createTrayIcon(hwnd win.HWND, iconId uint32) {
	win.Shell_NotifyIcon(win.NIM_ADD, &win.NOTIFYICONDATA{
		HWnd:             hwnd,
		UID:              iconId,
		UFlags:           win.NIF_MESSAGE,
		UCallbackMessage: pWMAPP_NOTIFYCALLBACK,
	})
}

func (w *WinTray) setVersion(hwnd win.HWND, iconId uint32) {
	win.Shell_NotifyIcon(win.NIM_SETVERSION, &win.NOTIFYICONDATA{
		HWnd:     hwnd,
		UID:      iconId,
		UVersion: win.NOTIFYICON_VERSION_4,
	})
}

func (w *WinTray) setTip(hwnd win.HWND, iconId uint32, text string) {
	nid := &win.NOTIFYICONDATA{
		HWnd:   hwnd,
		UID:    iconId,
		UFlags: win.NIF_TIP,
	}
	for i, v := range mustUTF16FromString(text) {
		if i == len(nid.SzTip) {
			break
		}
		nid.SzTip[i] = v
	}
	win.Shell_NotifyIcon(win.NIM_MODIFY, nid)
}

func (w *WinTray) addMenuItem(hmenu win.HMENU, id uint32, text string) {
	pAppendMenuW.Call(
		uintptr(hmenu),
		0,
		uintptr(id),
		uintptr(unsafe.Pointer(mustUTF16PtrFromString(text))),
	)
}

func (w *WinTray) showMenu(hwnd win.HWND, hmenu win.HMENU, pt *win.POINT) uint32 {

	// Set the foreground window
	win.SetForegroundWindow(hwnd)

	// Obtain the HWND of the shell desktop
	shellHwnd, _, _ := pGetShellWindow.Call()

	// Calculate the correct scaling factor
	scale := float32(win.GetDpiForWindow(
		win.HWND(shellHwnd),
	)) / 96

	// Avoid a division-by-zero error
	if scale == 0 {
		scale = 1
	}

	// Set the correct alignment
	var extraFlags uint32
	if win.GetSystemMetrics(win.SM_MENUDROPALIGNMENT) == 0 {
		extraFlags = win.TPM_LEFTALIGN
	} else {
		extraFlags = win.TPM_RIGHTALIGN
	}

	// Show the popup
	return win.TrackPopupMenu(
		hmenu,
		win.TPM_RETURNCMD|extraFlags,
		int32(float32(pt.X)/scale),
		int32(float32(pt.Y)/scale),
		0,
		hwnd,
		nil,
	)
}

func (w *WinTray) run(threadIdChan chan<- uint32, hwndChan chan<- win.HWND) {

	// Signal termination when the method ends
	defer close(w.closedChan)

	// Lock this goroutine to an OS thread until termination
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Send the current thread ID to the host goroutine
	threadIdChan <- win.GetCurrentThreadId()
	close(threadIdChan)

	// Generate a unique ID for this particular tray icon and create an empty
	// context menu
	var (
		iconId         = newIconId.Add(1)
		hmenu          = win.CreatePopupMenu()
		menuIds uint32 = 100
		menuFns        = make(map[uint32]func())
	)

	newMenuId := func() (v uint32) {
		v = menuIds
		menuIds += 1
		return
	}

	wndProc := func(hwnd win.HWND, msg uint32, wparam, lparam uintptr) uintptr {

		switch msg {

		// Initialize the icon and set the version (for event handling)
		case win.WM_CREATE:
			w.createTrayIcon(hwnd, iconId)
			w.setVersion(hwnd, iconId)
			return 0

		// The context menu was activated
		case pWMAPP_NOTIFYCALLBACK:
			if win.LOWORD(uint32(lparam)) == win.WM_CONTEXTMENU {
				id := w.showMenu(
					hwnd,
					hmenu,
					&win.POINT{
						X: win.GET_X_LPARAM(wparam),
						Y: win.GET_Y_LPARAM(wparam),
					},
				)
				if fn, ok := menuFns[id]; ok {
					fn()
				}
			}
			return 0

		// A message was sent from another thread requesting an action
		case pWMAPP_MESSAGE:
			m := <-w.messageChan
			switch m.Type {
			case pMESSAGE_SET_TIP:
				w.setTip(hwnd, iconId, m.Data.(string))
			case pMESSAGE_ADD_MENU_ITEM:
				var (
					d  = m.Data.(*pDataAddMenuItem)
					id = newMenuId()
				)
				menuFns[id] = d.Fn
				w.addMenuItem(hmenu, id, d.Text)
			}
			return 0
		}

		return win.DefWindowProc(hwnd, msg, wparam, lparam)
	}

	var (
		CLASS_NAME = "WndClass"
		hinstance  = win.GetModuleHandle(nil)
	)

	// Register the window class
	win.RegisterClassEx(&win.WNDCLASSEX{
		CbSize:        uint32(unsafe.Sizeof(win.WNDCLASSEX{})),
		LpfnWndProc:   syscall.NewCallback(wndProc),
		HInstance:     hinstance,
		LpszClassName: mustUTF16PtrFromString(CLASS_NAME),
	})

	// Create the hidden window
	hwndChan <- win.CreateWindowEx(
		0,
		mustUTF16PtrFromString(CLASS_NAME),
		mustUTF16PtrFromString("System Tray Window"),
		0,
		0,
		0,
		0,
		0,
		win.HWND_MESSAGE,
		0,
		hinstance,
		nil,
	)
	close(hwndChan)

	// Run the event loop
	msg := win.MSG{}
	for win.GetMessage(&msg, 0, 0, 0) == win.TRUE {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
}

// New creates a new WinTray icon.
func New() *WinTray {
	var (
		w = &WinTray{
			messageChan: make(chan *pMessage),
			closedChan:  make(chan any),
		}
		threadIdChan = make(chan uint32)
		hwndChan     = make(chan win.HWND)
	)
	go w.run(threadIdChan, hwndChan)
	w.threadId = <-threadIdChan
	w.hwnd = <-hwndChan
	return w
}

// SetTip sets the tooltip for the icon.
func (w *WinTray) SetTip(text string) {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_SET_TIP,
		Data: text,
	}
}

// AddMenuItem adds an item to the menu that will invoke the provided function
// when selected.
func (w *WinTray) AddMenuItem(text string, fn func()) {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_ADD_MENU_ITEM,
		Data: &pDataAddMenuItem{
			Text: text,
			Fn:   fn,
		},
	}
}

// Close removes the icon and shuts down the event loop.
func (w *WinTray) Close() {
	win.PostMessage(w.hwnd, win.WM_QUIT, 0, 0)
	<-w.closedChan
}