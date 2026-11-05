package main

//go:generate go run github.com/tc-hib/go-winres@v0.3.3 make --in assets/winres.json --arch amd64,arm64,386

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"sgate/internal/desktop"
)

var (
	user32                                            = windows.NewLazySystemDLL("user32.dll")
	kernel32                                          = windows.NewLazySystemDLL("kernel32.dll")
	gdi32                                             = windows.NewLazySystemDLL("gdi32.dll")
	ole32                                             = windows.NewLazySystemDLL("ole32.dll")
	createWindow                                      = user32.NewProc("CreateWindowExW")
	defWindow                                         = user32.NewProc("DefWindowProcW")
	sendMessage                                       = user32.NewProc("SendMessageW")
	setText                                           = user32.NewProc("SetWindowTextW")
	enableWindow                                      = user32.NewProc("EnableWindow")
	destroyWindow                                     = user32.NewProc("DestroyWindow")
	controller                                        *desktop.Client
	mainWindow, statusWindow, startButton, stopButton uintptr
	closing                                           bool
	choosingExit                                      bool
	lastStatus                                        string
	instance                                          uintptr
	font                                              uintptr
	scale                                             = 96
)

type windowClass struct {
	Size, Style                        uint32
	Procedure                          uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Name                         *uint16
	SmallIcon                          uintptr
}
type message struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	X, Y           int32
	Private        uint32
}

func wide(s string) *uint16 { p, _ := windows.UTF16PtrFromString(s); return p }

// Keep UTF-16 allocations alive while Win32 consumes uintptr arguments.
// All accesses occur on the locked GUI thread.
var wideStrings = map[string]*uint16{}

func address(s string) uintptr {
	p := wideStrings[s]
	if p == nil {
		p = wide(s)
		wideStrings[s] = p
	}
	return uintptr(unsafe.Pointer(p))
}
func px(n int) uintptr { return uintptr(n * scale / 96) }

func showError(err error) {
	user32.NewProc("MessageBoxW").Call(mainWindow, address(err.Error()), address("sgate 客户端控制"), 0x10)
}
func shell(verb, file, args string) error {
	return windows.ShellExecute(windows.Handle(mainWindow), wide(verb), wide(file), wide(args), nil, 1)
}
func openEditor(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return shell("open", filepath.Join(os.Getenv("SystemRoot"), "System32", "notepad.exe"), syscall.EscapeArg(path))
}

func control(class, text string, style uintptr, x, y, w, h, id int) uintptr {
	hwnd, _, _ := createWindow.Call(0, address(class), address(text), 0x40000000|style, px(x), px(y), px(w), px(h), mainWindow, uintptr(id), instance, 0)
	if hwnd != 0 {
		sendMessage.Call(hwnd, 0x30, font, 1)
	}
	return hwnd
}
func refresh() {
	s := controller.Snapshot()
	labels := map[string]string{"stopped": "客户端已停止", "running": "客户端正在运行", "stopping": "正在停止客户端，等待 VPN 路由与 DNS 清理…", "failed": "客户端未能正常运行"}
	text := labels[s.State]
	if s.PID > 0 {
		text += fmt.Sprintf("\r\n进程 PID：%d", s.PID)
	}
	if s.Error != "" {
		text += "\r\n" + s.Error
	}
	if closing {
		text += "\r\n清理完成后窗口将自动关闭。"
	}
	if text != lastStatus {
		setText.Call(statusWindow, uintptr(unsafe.Pointer(wide(text))))
		lastStatus = text
	}
	active := s.State == "running" || s.State == "stopping"
	startEnabled := uintptr(0)
	if !active && !closing {
		startEnabled = 1
	}
	enableWindow.Call(startButton, startEnabled)
	stopEnabled := uintptr(0)
	if s.State == "running" && !closing {
		stopEnabled = 1
	}
	enableWindow.Call(stopButton, stopEnabled)
	updateStatusColor(s.State)
	if closing && !active {
		destroyWindow.Call(mainWindow)
	}
}
func procedure(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmTray:
		handleTrayMessage(lparam, wparam)
		return 0
	case 0x0014: // WM_ERASEBKGND
		paintBackground(hwnd, wparam)
		return 1
	case 0x0138: // WM_CTLCOLORSTATIC
		return styleLabel(wparam, lparam)
	case 0x0112: // WM_SYSCOMMAND
		if wparam&0xfff0 == 0xf020 { // SC_MINIMIZE
			if !closing {
				minimizeToTray()
			}
			return 0
		}
	case 0x111: // WM_COMMAND
		if closing {
			return 0
		}
		var err error
		switch wparam & 0xffff {
		case idCopySetupToken:
			copySetupToken()
		case idKeepInTray:
			minimizeToTray()
		case idExit:
			beginExit()
			return 0
		case idCancelExit:
			showExitChoice(false)
		case 101:
			err = controller.Start()
		case 102:
			controller.Stop()
		case 103:
			var link string
			link, err = controller.ManagementURL()
			if err == nil {
				err = shell("open", link, "")
			}
		case 104:
			err = openEditor(controller.Config)
		case 105:
			err = openEditor(controller.Log)
		}
		if err != nil {
			showError(err)
		}
		refresh()
		return 0
	case 0x113: // WM_TIMER
		if wparam == setupTimer {
			refreshSetupToken()
		} else {
			refresh()
		}
		return 0
	case 0x10:
		if !closing {
			showExitChoice(true)
		}
		return 0 // WM_CLOSE
	case 0x2: // WM_DESTROY
		removeTrayIcon()
		user32.NewProc("PostQuitMessage").Call(0)
		return 0 // WM_DESTROY
	}
	if msg == taskbarCreated && inTray {
		trayIconAdded = false
		if !addTrayIcon() {
			restoreFromTray()
		}
		return 0
	}
	r, _, _ := defWindow.Call(hwnd, msg, wparam, lparam)
	return r
}
func run() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The elevated controller gives its child permission to configure Wintun.
	if !windows.GetCurrentProcessToken().IsElevated() {
		return shell("runas", exe, "")
	}
	root := filepath.Dir(filepath.Dir(exe))
	sum := sha256.Sum256([]byte(strings.ToLower(root)))
	mutex, err := windows.CreateMutex(nil, false, wide(fmt.Sprintf("Local\\sgate-control-%x", sum[:12])))
	if err != nil {
		if mutex != 0 {
			windows.CloseHandle(mutex)
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return errors.New("此安装目录的控制窗口已经运行，请切回原窗口。")
		}
		return err
	}
	defer windows.CloseHandle(mutex)
	controller = desktop.New(root)
	defer controller.Stop()
	user32.NewProc("SetProcessDPIAware").Call()
	if p := user32.NewProc("GetDpiForSystem"); p.Find() == nil {
		n, _, _ := p.Call()
		if n > 0 {
			scale = int(n)
		}
	}
	instance, _, _ = kernel32.NewProc("GetModuleHandleW").Call(0)
	initializeAppearance()
	defer releaseAppearance()
	largeIcon, smallIcon := loadAppIcons()
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
	name := wide("sgateIndependentClientController")
	class := windowClass{Procedure: syscall.NewCallback(procedure), Instance: instance, Cursor: cursor, Icon: largeIcon, SmallIcon: smallIcon, Name: name}
	class.Size = uint32(unsafe.Sizeof(class))
	if n, _, e := user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class))); n == 0 {
		return e
	}
	mainWindow, _, err = createWindow.Call(0, uintptr(unsafe.Pointer(name)), address("sgate 客户端控制"), 0x00ca0000, 0x80000000, 0x80000000, px(windowWidth), px(windowHeight), 0, 0, instance, 0)
	if mainWindow == 0 {
		return err
	}
	buildDashboard(largeIcon)
	buildSetupPanel()
	initializeTray(smallIcon)
	refresh()
	refreshSetupToken()
	user32.NewProc("SetTimer").Call(mainWindow, 1, 300, 0)
	user32.NewProc("SetTimer").Call(mainWindow, setupTimer, 1000, 0)
	user32.NewProc("ShowWindow").Call(mainWindow, 1)
	user32.NewProc("UpdateWindow").Call(mainWindow)
	var m message
	for {
		n, _, e := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(n) == -1 {
			return e
		}
		if n == 0 {
			break
		}
		if choosingExit && m.Message == 0x100 && m.WParam == 0x1b { // Escape
			showExitChoice(false)
			continue
		}
		if handled, _, _ := user32.NewProc("IsDialogMessageW").Call(mainWindow, uintptr(unsafe.Pointer(&m))); handled == 0 {
			user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&m)))
			user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&m)))
		}
	}
	return nil
}
func main() {
	runtime.LockOSThread()
	// ShellExecute may use COM services to open the default browser.
	r, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 2)
	if int32(r) >= 0 {
		defer ole32.NewProc("CoUninitialize").Call()
	}
	if err := run(); err != nil {
		showError(err)
	}
}
