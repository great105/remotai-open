//go:build windows

// Package desktopui opens the local web UI inside a native WebView2 window so
// the app looks like a real desktop application instead of a browser tab.
package desktopui

import (
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

var windowOpen atomic.Bool
var windowMu sync.Mutex
var activeWindow webview2.WebView

var user32 = syscall.NewLazyDLL("user32.dll")

const (
	spiGetWorkArea = 0x0030
	swRestore      = 9
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

// WindowOpen reports whether the native WebView2 window is currently shown.
// Автообновление по этому флагу откладывает рестарт, чтобы не выдёргивать
// окно из-под пользователя.
func WindowOpen() bool { return windowOpen.Load() }

// fitToWorkArea keeps the bottom buttons visible on compact laptop screens.
// 80 px are deliberately left for the taskbar/window chrome and breathing room.
func fitToWorkArea(width, height int) (int, int) {
	var r winRect
	ok, _, _ := user32.NewProc("SystemParametersInfoW").Call(
		spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0,
	)
	if ok == 0 || r.Right <= r.Left || r.Bottom <= r.Top {
		return width, height
	}
	maxW := int(r.Right-r.Left) - 80
	maxH := int(r.Bottom-r.Top) - 80
	if maxW >= 420 && width > maxW {
		width = maxW
	}
	if maxH >= 520 && height > maxH {
		height = maxH
	}
	return width, height
}

func restoreWindow(w webview2.WebView) {
	hwnd := uintptr(w.Window())
	user32.NewProc("ShowWindow").Call(hwnd, swRestore)
	user32.NewProc("SetForegroundWindow").Call(hwnd)
	user32.NewProc("SetFocus").Call(hwnd)
}

// Show opens a borderless-feeling native window at url and BLOCKS until the
// user closes it. Run it in its own goroutine.
//
//   - Only one window exists at a time; a concurrent call navigates, restores
//     and focuses that window instead of silently doing nothing.
//   - Returns false if WebView2 is unavailable (missing runtime, error). The
//     caller should fall back to opening a browser.
func Show(title, url string, width, height int) bool {
	width, height = fitToWorkArea(width, height)
	if !windowOpen.CompareAndSwap(false, true) {
		windowMu.Lock()
		w := activeWindow
		windowMu.Unlock()
		if w != nil {
			w.Dispatch(func() {
				w.SetTitle(title)
				w.SetSize(width, height, webview2.HintNone)
				w.Navigate(url)
				restoreWindow(w)
			})
		}
		return true
	}
	defer func() {
		windowMu.Lock()
		activeWindow = nil
		windowMu.Unlock()
		windowOpen.Store(false)
	}()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ok := true
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[GUI] webview unavailable (%v) — falling back to browser", r)
				ok = false
			}
		}()
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:     false,
			AutoFocus: true,
			WindowOptions: webview2.WindowOptions{
				Title:  title,
				Width:  uint(width),
				Height: uint(height),
				Center: true,
			},
		})
		if w == nil {
			ok = false
			return
		}
		defer w.Destroy()
		windowMu.Lock()
		activeWindow = w
		windowMu.Unlock()
		w.Navigate(url)
		w.Run() // blocks until the window is closed
	}()
	return ok
}
