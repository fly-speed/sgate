package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmTray = 0x8001 // WM_APP + 1
	trayID = 1

	nimAdd        = 0
	nimDelete     = 2
	nimSetFocus   = 3
	nimSetVersion = 4

	trayOpen = 1
	trayExit = 2
)

type notifyIconData struct {
	size                uint32
	window              uintptr
	id, flags, callback uint32
	icon                uintptr
	tip                 [128]uint16
	state, stateMask    uint32
	info                [256]uint16
	version             uint32
	infoTitle           [64]uint16
	infoFlags           uint32
	guid                windows.GUID
	balloonIcon         uintptr
}

type point struct{ x, y int32 }

var (
	notifyIcon                          = windows.NewLazySystemDLL("shell32.dll").NewProc("Shell_NotifyIconW")
	taskbarCreated                      uintptr
	trayIcon                            uintptr
	inTray, trayIconAdded, trayVersion4 bool
)

func initializeTray(icon uintptr) {
	trayIcon = icon
	id, _, _ := user32.NewProc("RegisterWindowMessageW").Call(address("TaskbarCreated"))
	taskbarCreated = id
	// Explorer runs with lower integrity than the elevated VPN controller.
	filter := user32.NewProc("ChangeWindowMessageFilterEx")
	filter.Call(mainWindow, wmTray, 1, 0) // MSGFLT_ALLOW
	if taskbarCreated != 0 {
		filter.Call(mainWindow, taskbarCreated, 1, 0)
	}
}

func trayData() notifyIconData {
	return notifyIconData{size: uint32(unsafe.Sizeof(notifyIconData{})), window: mainWindow, id: trayID}
}

func addTrayIcon() bool {
	if trayIconAdded {
		return true
	}
	data := trayData()
	data.flags = 0x87 // NIF_MESSAGE | NIF_ICON | NIF_TIP | NIF_SHOWTIP
	data.callback = wmTray
	data.icon = trayIcon
	copy(data.tip[:], windows.StringToUTF16("sgate · 代理与 VPN"))
	ok, _, _ := notifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	if ok == 0 {
		return false
	}
	trayIconAdded = true
	data.version = 4
	ok, _, _ = notifyIcon.Call(nimSetVersion, uintptr(unsafe.Pointer(&data)))
	trayVersion4 = ok != 0
	return true
}

func removeTrayIcon() {
	if trayIconAdded {
		data := trayData()
		notifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
		trayIconAdded = false
	}
}

func minimizeToTray() {
	showExitChoice(false)
	if !addTrayIcon() {
		// Keep a taskbar entry if Explorer cannot create the tray icon.
		user32.NewProc("ShowWindow").Call(mainWindow, 6) // SW_MINIMIZE
		return
	}
	inTray = true
	refreshSetupToken()
	user32.NewProc("ShowWindow").Call(mainWindow, 0) // SW_HIDE
}

func restoreFromTray() {
	inTray = false
	refreshSetupToken()
	removeTrayIcon()
	user32.NewProc("ShowWindow").Call(mainWindow, 9) // SW_RESTORE
	user32.NewProc("SetForegroundWindow").Call(mainWindow)
}

func beginExit() {
	if inTray {
		restoreFromTray()
	}
	showExitChoice(false)
	closing = true
	refreshSetupToken()
	// EOF on the control pipe cancels the owned client. Its exit waits for VPN
	// worker cleanup. refresh keeps the GUI alive until cmd.Wait returns.
	controller.Stop()
	refresh()
}

func handleTrayMessage(lparam, wparam uintptr) {
	if !inTray {
		return
	}
	event := uint16(lparam)
	if trayVersion4 {
		if uint16(lparam>>16) != trayID {
			return
		}
	} else if wparam != trayID {
		return
	}
	switch event {
	case 0x0400, 0x0401: // NIN_SELECT, NIN_KEYSELECT
		restoreFromTray()
	case 0x0202: // WM_LBUTTONUP
		restoreFromTray()
	case 0x007b: // WM_CONTEXTMENU
		if trayVersion4 {
			showTrayMenu(int32(int16(wparam)), int32(int16(wparam>>16)))
		}
	case 0x0205: // WM_RBUTTONUP in legacy mode
		if !trayVersion4 {
			var cursor point
			user32.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&cursor)))
			showTrayMenu(cursor.x, cursor.y)
		}
	}
}

func showTrayMenu(x, y int32) {
	menu, _, _ := user32.NewProc("CreatePopupMenu").Call()
	if menu == 0 {
		return
	}
	defer user32.NewProc("DestroyMenu").Call(menu)
	appendMenu := user32.NewProc("AppendMenuW")
	appendMenu.Call(menu, 0, trayOpen, address("打开 sgate"))
	appendMenu.Call(menu, 0, trayExit, address("退出程序"))
	user32.NewProc("SetForegroundWindow").Call(mainWindow)
	command, _, _ := user32.NewProc("TrackPopupMenu").Call(menu, 0x102, uintptr(x), uintptr(y), 0, mainWindow, 0)
	user32.NewProc("PostMessageW").Call(mainWindow, 0, 0, 0)
	data := trayData()
	notifyIcon.Call(nimSetFocus, uintptr(unsafe.Pointer(&data)))
	switch command {
	case trayOpen:
		restoreFromTray()
	case trayExit:
		beginExit()
	}
}
