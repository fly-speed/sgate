package main

import "unsafe"

const (
	idCopySetupToken = 114
	setupTimer       = 2
	windowWidth      = 692
	windowHeight     = 396
	setupHeight      = 104
)

var (
	setupControls []uintptr
	setupField    uintptr
	shownToken    string
)

func buildSetupPanel() {
	const readOnlyEdit = 0x10000 | 0x00800000 | 0x0800 | 0x0080 // WS_TABSTOP | WS_BORDER | ES_READONLY | ES_AUTOHSCROLL
	setupField = control("EDIT", "", readOnlyEdit, 44, 405, 464, 30, 0)
	sendMessage.Call(setupField, 0x30, setupFont, 1) // WM_SETFONT
	setupControls = []uintptr{
		label("首次设置 · 管理员初始化令牌", 44, 366, 590, 26, sectionFont, colorText, cardBrush, 0),
		setupField,
		button("复制令牌", 520, 401, 114, idCopySetupToken),
	}
}

func refreshSetupToken() {
	var token string
	if !closing && !choosingExit && !inTray {
		token = controller.InitialSetupToken()
	}
	if token == shownToken {
		return
	}

	wasVisible := shownToken != ""
	shownToken = token
	setText.Call(setupField, uintptr(unsafe.Pointer(wide(token))))
	if visible := token != ""; visible != wasVisible {
		showControls(setupControls, visible)
		height := windowHeight
		if visible {
			height += setupHeight
		}
		resizeSetupWindow(height)
		user32.NewProc("InvalidateRect").Call(mainWindow, 0, 1)
	}
}

func resizeSetupWindow(height int) {
	const noMove, noZOrder, noActivate = 0x0002, 0x0004, 0x0010
	var x, y int32
	flags := uintptr(noMove | noZOrder | noActivate)
	if height > windowHeight {
		var bounds rectangle
		var monitor struct {
			size          uint32
			display, work rectangle
			flags         uint32
		} // MONITORINFO
		monitor.size = uint32(unsafe.Sizeof(monitor))
		handle, _, _ := user32.NewProc("MonitorFromWindow").Call(mainWindow, 2) // MONITOR_DEFAULTTONEAREST
		windowOK, _, _ := user32.NewProc("GetWindowRect").Call(mainWindow, uintptr(unsafe.Pointer(&bounds)))
		monitorOK, _, _ := user32.NewProc("GetMonitorInfoW").Call(handle, uintptr(unsafe.Pointer(&monitor)))
		if windowOK != 0 && monitorOK != 0 && bounds.top+int32(px(height)) > monitor.work.bottom {
			x, y = bounds.left, max(monitor.work.top, monitor.work.bottom-int32(px(height)))
			flags &^= noMove
		}
	}
	user32.NewProc("SetWindowPos").Call(mainWindow, 0, uintptr(x), uintptr(y), px(windowWidth), px(height), flags)
}

func copySetupToken() {
	refreshSetupToken()
	if shownToken != "" {
		sendMessage.Call(setupField, 0x00b1, 0, ^uintptr(0)) // EM_SETSEL, select all
		sendMessage.Call(setupField, 0x0301, 0, 0)           // WM_COPY
	}
}
