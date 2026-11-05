package main

import "unsafe"

const (
	idKeepInTray = 111
	idExit       = 112
	idCancelExit = 113

	// COLORREF values are stored as 0x00BBGGRR.
	colorCanvas  = 0xf7f6f2
	colorHeader  = 0x3d3f10
	colorSurface = 0xffffff
	colorText    = 0x342a17
	colorMuted   = 0x625549
	colorMint    = 0xd3f4a3
	colorRunning = 0x4e6612
	colorWaiting = 0x215b82
	colorError   = 0x2e319c
)

type labelStyle struct {
	text  uintptr
	brush uintptr
}

type rectangle struct {
	left, top, right, bottom int32
}

var (
	canvasBrush, headerBrush, cardBrush uintptr
	titleFont, sectionFont, detailFont  uintptr
	setupFont                           uintptr
	labels                              = make(map[uintptr]labelStyle)
	dashboard, exitChoice               []uintptr
	confirmMinimize                     uintptr
)

func initializeAppearance() {
	canvasBrush, _, _ = gdi32.NewProc("CreateSolidBrush").Call(colorCanvas)
	headerBrush, _, _ = gdi32.NewProc("CreateSolidBrush").Call(colorHeader)
	cardBrush, _, _ = gdi32.NewProc("CreateSolidBrush").Call(colorSurface)
	font = createFont(16, 400, "Microsoft YaHei UI")
	titleFont = createFont(30, 700, "Microsoft YaHei UI")
	sectionFont = createFont(19, 600, "Microsoft YaHei UI")
	detailFont = createFont(14, 400, "Microsoft YaHei UI")
	setupFont = createFont(12, 400, "Consolas")
}

func createFont(size int, weight uintptr, face string) uintptr {
	height := -int(px(size))
	handle, _, _ := gdi32.NewProc("CreateFontW").Call(uintptr(height), 0, 0, 0, weight,
		0, 0, 0, 1, 0, 0, 5, 0, address(face))
	return handle
}

func releaseAppearance() {
	for _, object := range []uintptr{canvasBrush, headerBrush, cardBrush, font, titleFont, sectionFont, detailFont, setupFont} {
		if object != 0 {
			gdi32.NewProc("DeleteObject").Call(object)
		}
	}
}

func label(text string, x, y, w, h int, face, foreground, background uintptr, style uintptr) uintptr {
	handle := control("STATIC", text, style, x, y, w, h, 0)
	sendMessage.Call(handle, 0x30, face, 1) // WM_SETFONT
	labels[handle] = labelStyle{text: foreground, brush: background}
	return handle
}

func button(text string, x, y, w, id int) uintptr {
	return control("BUTTON", text, 0x10000, x, y, w, 38, id) // WS_TABSTOP
}

func showControls(controls []uintptr, visible bool) {
	command := uintptr(0) // SW_HIDE
	if visible {
		command = 5 // SW_SHOW
	}
	for _, handle := range controls {
		user32.NewProc("ShowWindow").Call(handle, command)
	}
}

func buildDashboard(icon uintptr) {
	iconControl := control("STATIC", "", 3, 24, 25, 46, 46, 0) // SS_ICON
	sendMessage.Call(iconControl, 0x0170, icon, 0)             // STM_SETICON
	showControls([]uintptr{
		iconControl,
		label("sgate", 80, 19, 565, 39, titleFont, colorSurface, headerBrush, 0),
		label("代理与 VPN  ·  Windows 客户端", 82, 60, 560, 26, font, colorMint, headerBrush, 0),
	}, true)

	statusWindow = label("", 42, 154, 590, 86, font, colorMuted, cardBrush, 0)
	startButton = button("启动客户端", 42, 255, 148, 101)
	stopButton = button("停止客户端", 202, 255, 148, 102)
	dashboard = []uintptr{
		label("运行状态", 42, 120, 560, 29, sectionFont, colorText, cardBrush, 0),
		statusWindow, startButton, stopButton,
		button("打开管理页面", 362, 255, 168, 103),
		button("编辑配置", 42, 305, 148, 104),
		button("查看日志", 202, 305, 148, 105),
		label("配置  "+controller.Config, 24, 367, 630, 24, detailFont, colorMuted, canvasBrush, 0x8000), // SS_ENDELLIPSIS
	}
	showControls(dashboard, true)

	confirmMinimize = button("最小化到托盘", 42, 255, 164, idKeepInTray)
	exitChoice = []uintptr{
		label("要退出 sgate 吗？", 42, 120, 565, 29, sectionFont, colorText, cardBrush, 0),
		label("最小化后，代理和 VPN 会继续运行。\r\n退出会停止此窗口管理的客户端，并等待 VPN 清理完成。", 42, 161, 575, 82, font, colorMuted, cardBrush, 0),
		confirmMinimize,
		button("退出程序", 218, 255, 148, idExit),
		button("取消", 378, 255, 108, idCancelExit),
	}
}

func showExitChoice(show bool) {
	if choosingExit == show {
		return
	}
	choosingExit = show
	showControls(dashboard, !show)
	showControls(exitChoice, show)
	refreshSetupToken()
	if show {
		user32.NewProc("SetFocus").Call(confirmMinimize)
	} else {
		user32.NewProc("SetFocus").Call(mainWindow)
	}
}

func paintBackground(hwnd, dc uintptr) {
	var client rectangle
	user32.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&client)))
	fill := user32.NewProc("FillRect")
	fill.Call(dc, uintptr(unsafe.Pointer(&client)), canvasBrush)
	header := rectangle{right: client.right, bottom: int32(px(96))}
	fill.Call(dc, uintptr(unsafe.Pointer(&header)), headerBrush)
	card := rectangle{left: int32(px(24)), top: int32(px(110)), right: client.right - int32(px(24)), bottom: int32(px(354))}
	fill.Call(dc, uintptr(unsafe.Pointer(&card)), cardBrush)
	if shownToken != "" {
		card.top, card.bottom = int32(px(397)), int32(px(488))
		fill.Call(dc, uintptr(unsafe.Pointer(&card)), cardBrush)
	}
}

func styleLabel(dc, handle uintptr) uintptr {
	style, ok := labels[handle]
	if !ok {
		if handle == setupField {
			return cardBrush
		}
		return headerBrush // The header's image control.
	}
	gdi32.NewProc("SetTextColor").Call(dc, style.text)
	gdi32.NewProc("SetBkMode").Call(dc, 1) // TRANSPARENT
	return style.brush
}

func updateStatusColor(state string) {
	foreground := uintptr(colorMuted)
	switch state {
	case "running":
		foreground = colorRunning
	case "stopping":
		foreground = colorWaiting
	case "failed":
		foreground = colorError
	}
	style := labels[statusWindow]
	if style.text != foreground {
		style.text = foreground
		labels[statusWindow] = style
		user32.NewProc("InvalidateRect").Call(statusWindow, 0, 1)
	}
}

func loadAppIcons() (large, small uintptr) {
	load := user32.NewProc("LoadImageW")
	const imageIcon, lrShared = 1, 0x8000
	large, _, _ = load.Call(instance, 1, imageIcon, px(48), px(48), lrShared)
	small, _, _ = load.Call(instance, 1, imageIcon, px(16), px(16), lrShared)
	if large == 0 {
		large, _, _ = user32.NewProc("LoadIconW").Call(0, 32512)
	}
	if small == 0 {
		small = large
	}
	return large, small
}
