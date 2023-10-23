package wintray

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const (
	DPI_AWARENESS_CONTEXT_SYSTEM_AWARE = ^uintptr(1)

	pWMAPP_NOTIFYCALLBACK = iota + win.WM_APP + 1
	pWMAPP_MESSAGE

	pMESSAGE_SET_ICON_FROM_BYTES = iota
	pMESSAGE_SET_TIP
	pMESSAGE_ADD_MENU_ITEM
	pMESSAGE_ADD_MENU_SEPARATOR
	pMESSAGE_SHOW_NOTIFICATION
)

var (
	newIconId = atomic.Uint32{}

	user32                        = windows.MustLoadDLL("User32.dll")
	pAppendMenuW                  = user32.MustFindProc("AppendMenuW")
	pSetThreadDpiAwarenessContext *windows.Proc
)

func init() {
	p, _ := user32.FindProc("SetThreadDpiAwarenessContext")
	if p != nil {
		pSetThreadDpiAwarenessContext = p
	}
}

type pMessage struct {
	Type int
	Data any
}

type pDataAddMenuItem struct {
	Text string
	Fn   func()
}

type pDataShowNotification struct {
	Info      string
	InfoTitle string
}

// WinTray provides a single icon in the system tray. A separate goroutine is
// used for running all of the API functions
type WinTray struct {
	hwnd        win.HWND
	messageChan chan *pMessage
	returnChan  chan error
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

func copyToUint16Buffer(buff any, text string) {
	var (
		tBuff = reflect.TypeOf(buff).Elem()
		vBuff = reflect.ValueOf(buff).Elem()
	)
	for i, v := range mustUTF16FromString(text) {
		if i == tBuff.Len() {
			vBuff.Index(i - 1).Set(reflect.Zero(tBuff.Elem()))
			break
		}
		vBuff.Index(i).Set(reflect.ValueOf(v))
	}
}

func (w *WinTray) createTrayIcon(hwnd win.HWND, iconId uint32) {
	win.Shell_NotifyIcon(win.NIM_ADD, &win.NOTIFYICONDATA{
		HWnd:             hwnd,
		UID:              iconId,
		UFlags:           win.NIF_MESSAGE,
		UCallbackMessage: pWMAPP_NOTIFYCALLBACK,
	})
}

func (w *WinTray) destroyTrayIcon(hwnd win.HWND, iconId uint32) {
	win.Shell_NotifyIcon(win.NIM_DELETE, &win.NOTIFYICONDATA{
		HWnd: hwnd,
		UID:  iconId,
	})
}

func (w *WinTray) setVersion(hwnd win.HWND, iconId uint32) {
	win.Shell_NotifyIcon(win.NIM_SETVERSION, &win.NOTIFYICONDATA{
		HWnd:     hwnd,
		UID:      iconId,
		UVersion: win.NOTIFYICON_VERSION_4,
	})
}

func (w *WinTray) setIcon(hwnd win.HWND, iconId uint32, b []byte) error {

	// Create a temporary file with the image contents
	f, err := os.CreateTemp("", "*.ico")
	if err != nil {
		return err
	}
	defer func() {
		os.Remove(f.Name())
	}()
	io.Copy(f, bytes.NewReader(b))
	f.Close()

	// Now attempt to load the icon
	h := win.LoadImage(
		0,
		mustUTF16PtrFromString(f.Name()),
		win.IMAGE_ICON,
		0,
		0,
		win.LR_DEFAULTSIZE|win.LR_LOADFROMFILE,
	)
	if h == 0 {
		return errors.New("unable to load icon")
	}

	hicon := win.HICON(h)

	// Set the icon
	nid := &win.NOTIFYICONDATA{
		HWnd:   hwnd,
		UID:    iconId,
		UFlags: win.NIF_ICON,
		HIcon:  hicon,
	}
	if !win.Shell_NotifyIcon(win.NIM_MODIFY, nid) {
		return errors.New("unable to change icon")
	}

	return nil
}

func (w *WinTray) setTip(hwnd win.HWND, iconId uint32, text string) error {
	nid := &win.NOTIFYICONDATA{
		CbSize: uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:   hwnd,
		UID:    iconId,
		UFlags: win.NIF_TIP | win.NIF_SHOWTIP,
	}
	copyToUint16Buffer(&nid.SzTip, text)
	if !win.Shell_NotifyIcon(win.NIM_MODIFY, nid) {
		return errors.New("unable to change tooltip")
	}
	return nil
}

func (w *WinTray) addMenuItem(hmenu win.HMENU, id uint32, text string) error {
	if ret, _, err := pAppendMenuW.Call(
		uintptr(hmenu),
		0,
		uintptr(id),
		uintptr(unsafe.Pointer(mustUTF16PtrFromString(text))),
	); ret == 0 {
		return err
	}
	return nil
}

func (w *WinTray) addMenuSeparator(hmenu win.HMENU) error {
	if ret, _, err := pAppendMenuW.Call(
		uintptr(hmenu),
		uintptr(win.MF_SEPARATOR),
		0,
		0,
	); ret == 0 {
		return err
	}
	return nil
}

func (w *WinTray) showNotification(hwnd win.HWND, iconId uint32, info, infoTitle string) error {
	nid := &win.NOTIFYICONDATA{
		CbSize: uint32(unsafe.Sizeof(win.NOTIFYICONDATA{})),
		HWnd:   hwnd,
		UID:    iconId,
		UFlags: win.NIF_INFO,
	}
	copyToUint16Buffer(&nid.SzInfo, info)
	copyToUint16Buffer(&nid.SzInfoTitle, infoTitle)
	if !win.Shell_NotifyIcon(win.NIM_MODIFY, nid) {
		return errors.New("unable to display notification")
	}
	return nil
}

func (w *WinTray) showMenu(hwnd win.HWND, hmenu win.HMENU, pt *win.POINT) uint32 {

	// Set the foreground window
	win.SetForegroundWindow(hwnd)

	// Get the correct alignment
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
		pt.X,
		pt.Y,
		0,
		hwnd,
		nil,
	)
}

func (w *WinTray) run(hwndChan chan<- win.HWND) {

	// Signal termination when the method ends
	defer close(w.closedChan)

	// Lock this goroutine to an OS thread until termination
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// If we are running on Windows 10, set the thread DPI awareness
	if pSetThreadDpiAwarenessContext != nil {
		pSetThreadDpiAwarenessContext.Call(
			uintptr(DPI_AWARENESS_CONTEXT_SYSTEM_AWARE),
		)
	}

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

		// Destroy the icon during shutdown
		case win.WM_QUIT:
			w.destroyTrayIcon(hwnd, iconId)
			return 0

		// The context menu was activated
		case pWMAPP_NOTIFYCALLBACK:
			if win.LOWORD(uint32(lparam)) == win.WM_RBUTTONUP {

				// Get the cursor position
				pt := win.POINT{}
				win.GetCursorPos(&pt)

				// Show the menu at that position and invoke the callback for
				// the item that is selected
				id := w.showMenu(hwnd, hmenu, &pt)
				if fn, ok := menuFns[id]; ok {
					go fn()
				}

				return 0
			}

		// A message was sent from another thread requesting an action
		case pWMAPP_MESSAGE:
			m := <-w.messageChan
			switch m.Type {
			case pMESSAGE_SET_ICON_FROM_BYTES:
				w.returnChan <- w.setIcon(hwnd, iconId, m.Data.([]byte))
			case pMESSAGE_SET_TIP:
				w.returnChan <- w.setTip(hwnd, iconId, m.Data.(string))
			case pMESSAGE_ADD_MENU_ITEM:
				var (
					d  = m.Data.(*pDataAddMenuItem)
					id = newMenuId()
				)
				menuFns[id] = d.Fn
				w.returnChan <- w.addMenuItem(hmenu, id, d.Text)
			case pMESSAGE_ADD_MENU_SEPARATOR:
				w.returnChan <- w.addMenuSeparator(hmenu)
			case pMESSAGE_SHOW_NOTIFICATION:
				d := m.Data.(*pDataShowNotification)
				w.returnChan <- w.showNotification(hwnd, iconId, d.Info, d.InfoTitle)
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
			returnChan:  make(chan error),
			closedChan:  make(chan any),
		}
		hwndChan = make(chan win.HWND)
	)
	go w.run(hwndChan)
	w.hwnd = <-hwndChan
	return w
}

// SetIconFromBytes reads an ICO file from a byte array.
func (w *WinTray) SetIconFromBytes(b []byte) error {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_SET_ICON_FROM_BYTES,
		Data: b,
	}
	return <-w.returnChan
}

// SetTip sets the tooltip for the icon.
func (w *WinTray) SetTip(text string) error {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_SET_TIP,
		Data: text,
	}
	return <-w.returnChan
}

// AddMenuItem adds an item to the menu that will invoke the provided function
// when selected.
func (w *WinTray) AddMenuItem(text string, fn func()) error {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_ADD_MENU_ITEM,
		Data: &pDataAddMenuItem{
			Text: text,
			Fn:   fn,
		},
	}
	return <-w.returnChan
}

// AddMenuSeparator inserts a menu separator after the last item.
func (w *WinTray) AddMenuSeparator() error {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_ADD_MENU_SEPARATOR,
	}
	return <-w.returnChan
}

// ShowNotification displays a balloon notification with the provided message
// and title.
func (w *WinTray) ShowNotification(info, infoTitle string) error {
	win.PostMessage(w.hwnd, pWMAPP_MESSAGE, 0, 0)
	w.messageChan <- &pMessage{
		Type: pMESSAGE_SHOW_NOTIFICATION,
		Data: &pDataShowNotification{
			Info:      info,
			InfoTitle: infoTitle,
		},
	}
	return <-w.returnChan
}

// Close removes the icon and shuts down the event loop.
func (w *WinTray) Close() {
	win.PostMessage(w.hwnd, win.WM_QUIT, 0, 0)
	<-w.closedChan
}
