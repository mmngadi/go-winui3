package winui

// Windows / WinUI3 native DLL dynamic wrapper.
// This package provides Go wrappers around the exported functions of WinUI3Native.dll
// produced by the native project in native/WinUI3Native.
//
// The wrapper purposefully avoids cgo by using the syscall + golang.org/x/sys/windows
// packages to dynamically load the DLL at runtime. This keeps the build simple and
// allows swapping Debug/Release binaries without rebuilding Go code. Ensure the
// directory containing WinUI3Native.dll is on PATH or explicitly pass the path to
// Load(). The build.ps1 script already arranges the output DLLs under bin/<arch>/<cfg>
// and sets CGO_LDFLAGS for any future cgo needs, but this wrapper does not depend on it.
//
// Threading: All exported native functions that mutate UI marshal to the WinUI thread.
// From Go you simply call the wrapper functions; they are non-blocking except for
// creation functions that synchronously return a control handle (the native layer
// internally blocks until creation finishes). Event polling is pull-based via PollEvents.
//
// Safety: The wrapper attempts to convert Go strings to UTF-16 and passes pointers
// valid for the duration of the call. Callers must retain any returned Control handles
// (opaque uintptr) but must NOT attempt to free them.
//
// Call sequence:
//  1. Call Load(dllDir) optionally (or rely on PATH).
//  2. Call Init().
//  3. Create window via CreateWindow("Title") (or the native side auto-creates one).
//  4. Create controls / set properties.
//  5. Periodically call PollEvents() (e.g. in a loop) to process input/resize.
//  6. Call Shutdown() before exiting.
//
// NOTE: Because WinUI requires STA, ensure your main Go goroutine is locked to
// an OS thread if you plan to host any message pumping logic there (runtime.LockOSThread()).
// The native DLL spins its own UI thread so basic usage does not strictly require it.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Handle represents an opaque UI element reference (void* from native side).
// 0 means invalid / null.
type Handle uintptr

// Event kinds & actions matching native documentation.
const (
	EventKindKey     = 1
	EventKindMouse   = 2
	EventKindResize  = 3
	EventKindClosed  = 4
	EventKindCreated = 5

	ActionDown = 1
	ActionUp   = 2
	ActionChar = 3 // (currently only for key events if ever surfaced)
	// Define idxEx locally in ToggleFullscreen
	// Add window APIs: GetWindowHandle, IsWindowFullscreen, ShowWindow/HideWindow, CloseWindow, and min/max size hint storage.
)

// Event mirrors the native WinUIEvent struct layout (ensure field order & types).
type Event struct {
	Kind   int32
	Code   int32
	Action int32
	Mods   int32
	X      int32
	Y      int32
	W      float64
	H      float64
}

// ResizeHandler invoked when native resize callback fires.
// Width and height are rounded to nearest integers.
type ResizeHandler func(width, height int)

// InputHandler invoked for low-level immediate callbacks (distinct from polled events).
// kind:1=key 2=mouse; action:1=down 2=up 3=char; mods bitmask (side-specific); x,y for mouse.
type InputHandler func(kind, code, action, mods, x, y int)

// -----------------------------------------------------------------------------
// Input state tracking (keyboard focused). This layer provides convenience
// helpers similar to popular game frameworks. We treat "pressed" as a key
// that transitioned from up->down since the last ResetKeyState() (implicitly
// each PollEvents style frame if user calls ResetKeyTransitions). "repeat"
// is any additional down event while already held. A simple FIFO queue holds
// distinct press keycodes; callers drain it via GetKeyPressed(). For now
// character queue mirrors key queue (no separate text input translation yet).
// -----------------------------------------------------------------------------

var (
	keyStateMu      sync.Mutex
	keyDown         = make(map[int]bool) // currently held
	keyPressedOnce  = make(map[int]bool) // down edge this frame
	keyReleasedOnce = make(map[int]bool) // up edge this frame
	keyRepeat       = make(map[int]bool) // repeat events (down while already held)
	keyPressQueue   []int                // ordered pressed keys
	charPressQueue  []int                // unicode codepoints
	currentMods     int                  // last observed modifiers mask
)

// Modifiers bitmask (matches native GetModifiersMask mapping)
const (
	ModLShift   = 1
	ModRShift   = 2
	ModLControl = 4
	ModRControl = 8
	ModLAlt     = 16
	ModRAlt     = 32
	ModLWin     = 64
	ModRWin     = 128

	ModShift   = ModLShift | ModRShift
	ModControl = ModLControl | ModRControl
	ModAlt     = ModLAlt | ModRAlt
	ModWin     = ModLWin | ModRWin
)

// Mouse buttons mapping (from native emitter)
const (
	MouseButtonLeft   = 1
	MouseButtonRight  = 2
	MouseButtonMiddle = 3
)

// Mouse state
var (
	mouseStateMu      sync.Mutex
	mouseDown         = make(map[int]bool)
	mousePressedOnce  = make(map[int]bool)
	mouseReleasedOnce = make(map[int]bool)
	mouseX, mouseY    int
)

// resetTransient clears per-frame key transition maps and queues.
func resetTransient() {
	for k := range keyPressedOnce {
		delete(keyPressedOnce, k)
	}
	for k := range keyReleasedOnce {
		delete(keyReleasedOnce, k)
	}
	for k := range keyRepeat {
		delete(keyRepeat, k)
	}
	keyPressQueue = keyPressQueue[:0]
	charPressQueue = charPressQueue[:0]
}

// public helpers -------------------------------------------------------------

// IsKeyDown returns true if key currently held.
func IsKeyDown(key int) bool { keyStateMu.Lock(); v := keyDown[key]; keyStateMu.Unlock(); return v }

// IsKeyUp returns true if key not currently held.
func IsKeyUp(key int) bool { keyStateMu.Lock(); v := !keyDown[key]; keyStateMu.Unlock(); return v }

// IsKeyPressed returns true if key transitioned up->down since last frame.
func IsKeyPressed(key int) bool {
	keyStateMu.Lock()
	v := keyPressedOnce[key]
	keyStateMu.Unlock()
	return v
}

// IsKeyPressedRepeat returns true if a repeat (additional down while held) occurred.
func IsKeyPressedRepeat(key int) bool {
	keyStateMu.Lock()
	v := keyRepeat[key]
	keyStateMu.Unlock()
	return v
}

// IsKeyReleased returns true if key transitioned down->up since last frame.
func IsKeyReleased(key int) bool {
	keyStateMu.Lock()
	v := keyReleasedOnce[key]
	keyStateMu.Unlock()
	return v
}

// GetKeyPressed dequeues next pressed keycode, or 0 if none.
func GetKeyPressed() int {
	keyStateMu.Lock()
	defer keyStateMu.Unlock()
	if len(keyPressQueue) == 0 {
		return 0
	}
	k := keyPressQueue[0]
	keyPressQueue = keyPressQueue[1:]
	return k
}

// GetCharPressed dequeues next character codepoint (currently same as key code) or 0.
func GetCharPressed() int {
	keyStateMu.Lock()
	defer keyStateMu.Unlock()
	if len(charPressQueue) == 0 {
		return 0
	}
	c := charPressQueue[0]
	charPressQueue = charPressQueue[1:]
	return c
}

// ResetKeyTransitions clears per-frame pressed/released/repeat/queues for both
// keyboard and mouse. Call once per frame.
func ResetKeyTransitions() {
	// Clear mouse transitions first (lock order consistent with callbacks: mouse then key)
	mouseStateMu.Lock()
	for k := range mousePressedOnce {
		delete(mousePressedOnce, k)
	}
	for k := range mouseReleasedOnce {
		delete(mouseReleasedOnce, k)
	}
	mouseStateMu.Unlock()

	// Clear key transitions and queues
	keyStateMu.Lock()
	resetTransient()
	keyStateMu.Unlock()
	atomic.StoreUint32(&windowResizedFlag, 0)
}

// helper: get or find the native HWND by window title or foreground window
func getHWND() uintptr {
	hwndMu.Lock()
	defer hwndMu.Unlock()
	if cachedHWND != 0 {
		return cachedHWND
	}
	// Try foreground window first
	if procGetForegroundWnd.Find() == nil {
		if h, _, _ := procGetForegroundWnd.Call(); h != 0 {
			cachedHWND = h
		}
	}
	if cachedHWND == 0 && lastWindowTitle != "" && procFindWindowW.Find() == nil {
		t16, _ := syscall.UTF16PtrFromString(lastWindowTitle)
		if h, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(t16))); h != 0 {
			cachedHWND = h
		}
	}
	return cachedHWND
}

// GetModifiers returns the last observed modifiers mask.
func GetModifiers() int { keyStateMu.Lock(); m := currentMods; keyStateMu.Unlock(); return m }

func IsShiftDown() bool   { return (GetModifiers() & ModShift) != 0 }
func IsControlDown() bool { return (GetModifiers() & ModControl) != 0 }
func IsAltDown() bool     { return (GetModifiers() & ModAlt) != 0 }

// Mouse helpers --------------------------------------------------------------
func IsMouseButtonDown(button int) bool {
	mouseStateMu.Lock()
	v := mouseDown[button]
	mouseStateMu.Unlock()
	return v
}
func IsMouseButtonUp(button int) bool {
	mouseStateMu.Lock()
	v := !mouseDown[button]
	mouseStateMu.Unlock()
	return v
}
func IsMouseButtonPressed(button int) bool {
	mouseStateMu.Lock()
	v := mousePressedOnce[button]
	mouseStateMu.Unlock()
	return v
}
func IsMouseButtonReleased(button int) bool {
	mouseStateMu.Lock()
	v := mouseReleasedOnce[button]
	mouseStateMu.Unlock()
	return v
}
func GetMouseX() int { mouseStateMu.Lock(); x := mouseX; mouseStateMu.Unlock(); return x }
func GetMouseY() int { mouseStateMu.Lock(); y := mouseY; mouseStateMu.Unlock(); return y }

func GetMousePosition() (int, int) {
	mouseStateMu.Lock()
	x, y := mouseX, mouseY
	mouseStateMu.Unlock()
	return x, y
}

// SetMousePosition sets the global cursor position (screen coordinates).
func SetMousePosition(x, y int) {
	// Best-effort; if unavailable, silently ignore.
	if procSetCursorPos.Find() == nil {
		procSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y)))
	}
}

var (
	dllOnce sync.Once
	dllErr  error
	mod     *windows.DLL

	// Proc pointers
	pInitUI, pShutdownUI                                               *windows.Proc
	pCreateWindow, pCreateTextInput                                    *windows.Proc
	pGetMainWindow, pWindowExists, pIsWindowReady, pWaitForWindowReady *windows.Proc
	pSetWindowTitle, pGetWindowSize                                    *windows.Proc
	pRegisterResizeCallback                                            *windows.Proc
	pRegisterInputCallback                                             *windows.Proc
	pSetWindowBackgroundColor                                          *windows.Proc
	pPollEvents                                                        *windows.Proc
	pRegisterCloseCallback                                             *windows.Proc
	pBeginShutdownAsync                                                *windows.Proc
	pGetRuntimeState                                                   *windows.Proc
	pSetWindowMinMax                                                   *windows.Proc

	resizeHandlerMu sync.RWMutex
	resizeHandler   ResizeHandler
	inputHandlerMu  sync.RWMutex
	inputHandler    InputHandler

	closeHandlerMu sync.RWMutex
	closeHandler   func()

	// Hold Go callbacks to prevent GC.
	resizeCallbackPtr uintptr
	inputCallbackPtr  uintptr
	closeCallbackPtr  uintptr

	// Track UI init
	uiInitialized uint32

	// Timing
	timeStartOnce sync.Once
	timeStart     time.Time
	targetFPS     int32 = 60
	lastFrameNS   int64 // nanoseconds for last completed frame
)

// window state tracking
var (
	hwndMu            sync.Mutex
	cachedHWND        uintptr
	lastWindowTitle   string
	windowResizedFlag uint32

	savedStyle   uintptr
	savedExStyle uintptr
	savedRect    rect
)

// user32 imports for text translation and cursor control
var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	procToUnicodeEx       = user32.NewProc("ToUnicodeEx")
	procGetKeyboardLayout = user32.NewProc("GetKeyboardLayout")
	procMapVirtualKeyExW  = user32.NewProc("MapVirtualKeyExW")
	procSetCursorPos      = user32.NewProc("SetCursorPos")
	procGetClientRect     = user32.NewProc("GetClientRect")
)

// additional user32 procs for window management
var (
	procFindWindowW       = user32.NewProc("FindWindowW")
	procGetForegroundWnd  = user32.NewProc("GetForegroundWindow")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procIsIconic          = user32.NewProc("IsIconic")
	procIsZoomed          = user32.NewProc("IsZoomed")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procShowWindow        = user32.NewProc("ShowWindow")
	procSetForegroundWnd  = user32.NewProc("SetForegroundWindow")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procGetDpiForWindow   = user32.NewProc("GetDpiForWindow")
	procGetWindowLongPtrW = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	procSetLayeredAttr    = user32.NewProc("SetLayeredWindowAttributes")
)

// RECT structure for GetWindowRect
type rect struct {
	Left, Top, Right, Bottom int32
}

// window constants
const (
	GWL_STYLE   = -16
	GWL_EXSTYLE = -20

	WS_OVERLAPPED  = 0x00000000
	WS_POPUP       = 0x80000000
	WS_CAPTION     = 0x00C00000
	WS_SYSMENU     = 0x00080000
	WS_THICKFRAME  = 0x00040000
	WS_MINIMIZEBOX = 0x00020000
	WS_MAXIMIZEBOX = 0x00010000
	WS_VISIBLE     = 0x10000000

	WS_OVERLAPPEDWINDOW = WS_OVERLAPPED | WS_CAPTION | WS_SYSMENU | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX

	WS_EX_LAYERED = 0x00080000

	SW_SHOW     = 5
	SW_HIDE     = 0
	SW_MINIMIZE = 6
	SW_RESTORE  = 9
	SW_MAXIMIZE = 3

	SWP_NOSIZE         = 0x0001
	SWP_NOMOVE         = 0x0002
	SWP_NOZORDER       = 0x0004
	SWP_NOOWNERZORDER  = 0x0200
	SWP_FRAMECHANGED   = 0x0020
	SWP_NOSENDCHANGING = 0x0400

	SM_CXSCREEN = 0
	SM_CYSCREEN = 1

	LWA_ALPHA = 0x00000002
)

// Virtual-key codes used for modifier key state population
const (
	vkSHIFT    = 0x10
	vkCONTROL  = 0x11
	vkMENU     = 0x12 // Alt
	vkLSHIFT   = 0xA0
	vkRSHIFT   = 0xA1
	vkLCONTROL = 0xA2
	vkRCONTROL = 0xA3
	vkLMENU    = 0xA4
	vkRMENU    = 0xA5
)

const (
	mapvkVK_TO_VSC = 0
)

// translateVKToRunes converts a virtual-key into Unicode runes using current layout and modifiers.
func translateVKToRunes(vk, mods int) []rune {
	// Ensure required procs are present
	if procToUnicodeEx.Find() != nil || procGetKeyboardLayout.Find() != nil {
		return nil
	}
	// Build a minimal keyboard state array
	var ks [256]byte
	if (mods & ModShift) != 0 {
		ks[vkSHIFT] = 0x80
		ks[vkLSHIFT] = 0x80
		ks[vkRSHIFT] = 0x80
	}
	if (mods & ModControl) != 0 {
		ks[vkCONTROL] = 0x80
		ks[vkLCONTROL] = 0x80
		ks[vkRCONTROL] = 0x80
	}
	if (mods & ModAlt) != 0 {
		ks[vkMENU] = 0x80
		ks[vkLMENU] = 0x80
		ks[vkRMENU] = 0x80
	}

	// Keyboard layout for current thread (0)
	hkl, _, _ := procGetKeyboardLayout.Call(0)
	// Map VK to scan code
	var sc uintptr
	if procMapVirtualKeyExW.Find() == nil {
		r, _, _ := procMapVirtualKeyExW.Call(uintptr(uint32(vk)), uintptr(mapvkVK_TO_VSC), hkl)
		sc = r
	}
	// Call ToUnicodeEx
	var out [8]uint16
	r1, _, _ := procToUnicodeEx.Call(uintptr(uint32(vk)), sc, uintptr(unsafe.Pointer(&ks[0])), uintptr(unsafe.Pointer(&out[0])), uintptr(int32(len(out))), 0, hkl)
	n := int(int32(r1))
	if n <= 0 {
		return nil
	}
	rs := utf16.Decode(out[:n])
	return rs
}

// Load loads the WinUI3Native DLL. If dllDir is non-empty it is temporarily added
// to the DLL search path (SetDllDirectory) for the duration of load.
func Load(dllDirs ...string) error {
	dllOnce.Do(func() {
		// Candidate directories: user-provided, exe dir, cwd, bin/x64/{Debug,Release}
		var cands []string
		for _, d := range dllDirs {
			if d != "" {
				cands = append(cands, d)
			}
		}
		if exe, err := os.Executable(); err == nil {
			cands = append(cands, filepath.Dir(exe))
		}
		if cwd, err := os.Getwd(); err == nil {
			cands = append(cands, cwd)
			cands = append(cands, filepath.Join(cwd, "bin", "x64", "Debug"))
			cands = append(cands, filepath.Join(cwd, "bin", "x64", "Release"))
		}

		var loaded bool
		var lastErr error
		for _, dir := range cands {
			_ = windows.SetDllDirectory(dir)
			if m, e := windows.LoadDLL("WinUI3Native.dll"); e == nil {
				mod = m
				loaded = true
				break
			} else {
				lastErr = e
			}
		}
		if !loaded {
			if m, e := windows.LoadDLL("WinUI3Native.dll"); e == nil {
				mod = m
			} else {
				dllErr = fmt.Errorf("load WinUI3Native.dll: %w", lastErr)
				return
			}
		}

		// Resolve all procedures; fail fast if any are missing.
		must := func(name string) *windows.Proc {
			p, err := mod.FindProc(name)
			if err != nil {
				dllErr = fmt.Errorf("missing export %s: %w", name, err)
			}
			return p
		}
		pInitUI = must("InitUI")
		pShutdownUI = must("ShutdownUI")
		pCreateWindow = must("create_window")
		pCreateTextInput = must("create_text_input")
		pGetMainWindow = must("get_main_window")
		pWindowExists = must("window_exists")
		pIsWindowReady = must("is_window_ready")
		pWaitForWindowReady = must("wait_for_window_ready")
		pSetWindowTitle = must("set_window_title")
		pGetWindowSize = must("get_window_size")
		pRegisterResizeCallback = must("register_resize_callback")
		pRegisterInputCallback = must("register_input_callback")
		pSetWindowBackgroundColor = must("set_window_background_color")
		pPollEvents = must("winui_poll_events")
		pRegisterCloseCallback = must("register_close_callback")
		pBeginShutdownAsync = must("begin_shutdown_async")
		pGetRuntimeState = must("get_runtime_state")
		pSetWindowMinMax = must("set_window_min_max")
	})
	if dllErr != nil {
		return dllErr
	}
	// Implicit Init (idempotent)
	return Init()
}

// Init initializes the WinUI runtime (bootstrap + UI thread).
func Init() error {
	if atomic.LoadUint32(&uiInitialized) == 1 {
		return nil
	}
	if pInitUI == nil {
		return errors.New("winui: DLL not loaded")
	}
	r1, _, _ := pInitUI.Call()
	if hr := HRESULT(r1); !hr.Succeeded() {
		return fmt.Errorf("InitUI failed: %s", hr)
	}
	atomic.StoreUint32(&uiInitialized, 1)
	// Establish timing start (once) for GetTime()
	timeStartOnce.Do(func() { timeStart = time.Now() })
	// Auto-register callbacks so input/resize work without extra setup
	ensureResizeCallbackRegistered()
	ensureInputCallbackRegistered()
	return nil
}

// Shutdown releases the runtime.
func Shutdown() {
	if pShutdownUI != nil {
		pShutdownUI.Call()
	}
}

// BeginShutdownAsync starts native shutdown on a detached thread (idempotent).
// Use this when you need to request shutdown without blocking caller.
func BeginShutdownAsync() {
	if pBeginShutdownAsync != nil {
		pBeginShutdownAsync.Call()
	}
}

// HRESULT is a helper for formatting.
type HRESULT uint32

func (h HRESULT) Succeeded() bool { return int32(h) >= 0 }
func (h HRESULT) Failed() bool    { return int32(h) < 0 }
func (h HRESULT) Error() string   { return fmt.Sprintf("HRESULT 0x%08X", uint32(h)) }

// CreateWindow creates (or returns) a window with title.
func CreateWindow(width, height int, title string) Handle {
	if pCreateWindow == nil {
		return 0
	}
	t16, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := pCreateWindow.Call(uintptr(width), uintptr(height), uintptr(unsafe.Pointer(t16)))
	hwndMu.Lock()
	lastWindowTitle = title
	cachedHWND = 0
	hwndMu.Unlock()
	return Handle(r)
}

// CreateTextInput creates a text input (TextBox) with initial text.
func CreateTextInput(parent Handle, text string) Handle {
	if pCreateTextInput == nil {
		return 0
	}
	t16, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := pCreateTextInput.Call(uintptr(parent), uintptr(unsafe.Pointer(t16)))
	return Handle(r)
}

// WindowExists returns true if native window exists.
func WindowExists() bool {
	if pWindowExists == nil {
		return false
	}
	r, _, _ := pWindowExists.Call()
	return r != 0
}

// GetMainWindow returns handle to main window.
func GetMainWindow() Handle {
	if pGetMainWindow == nil {
		return 0
	}
	r, _, _ := pGetMainWindow.Call()
	return Handle(r)
}

// IsWindowReady returns true if the window exists and has content.
func IsWindowReady() bool {
	if pIsWindowReady == nil {
		return false
	}
	r, _, _ := pIsWindowReady.Call()
	return r != 0
}

// WaitForWindowReady waits up to timeout for window readiness.
// Uses native wait_for_window_ready which polls on the UI side.
func WaitForWindowReady(timeout time.Duration) bool {
	if pWaitForWindowReady == nil {
		return false
	}
	ms := int(timeout / time.Millisecond)
	if ms <= 0 {
		ms = 5000
	}
	r, _, _ := pWaitForWindowReady.Call(uintptr(ms))
	return r != 0
}

// WaitUntilWindowReady blocks until the window is ready or the timeout elapses.
// Returns nil on success, or an error if the window was not ready in time.
func WaitUntilWindowReady(timeout time.Duration) error {
	if WaitForWindowReady(timeout) {
		return nil
	}
	return fmt.Errorf("window not ready after %v", timeout)
}

// CreateWindowAndWait creates the window and waits for readiness up to timeout.
// Returns the window handle or 0 with an error on timeout.
func CreateWindowAndWait(width, height int, title string, timeout time.Duration) (Handle, error) {
	h := CreateWindow(width, height, title)
	if err := WaitUntilWindowReady(timeout); err != nil {
		return 0, err
	}
	if h == 0 { // fetch handle post-initialization if native created asynchronously
		h = GetMainWindow()
		if h == 0 {
			return 0, fmt.Errorf("window ready but handle unavailable")
		}
	}
	return h, nil
}

// MustCreateWindow creates the window and waits for readiness.
// Returns the handle and a non-nil error if readiness wasn't achieved.
// Kept for ergonomics even though the name suggests a panic-style helper.
func MustCreateWindow(width, height int, title string, timeout time.Duration) (Handle, error) {
	return CreateWindowAndWait(width, height, title, timeout)
}

// InitWindow loads the DLL, initializes the runtime, creates a window and waits until it's ready.
// Uses a default 5s timeout for readiness.
func InitWindow(width, height int, title string) (Handle, error) {
	if err := Load(); err != nil {
		return 0, err
	}
	return CreateWindowAndWait(width, height, title, 5*time.Second)
}

// InitWindowWithTimeout allows specifying a readiness timeout.
func InitWindowWithTimeout(width, height int, title string, timeout time.Duration) (Handle, error) {
	if err := Load(); err != nil {
		return 0, err
	}
	return CreateWindowAndWait(width, height, title, timeout)
}

// WindowShouldClose returns true when the native runtime indicates shutdown was requested.
// This reflects the same condition that would emit EventKindClosed and is suitable for
// simple loops like: for !winui.WindowShouldClose() { ... }.
func WindowShouldClose() bool {
	rs := GetRuntimeState()
	return rs.ShutdownRequested
}

// RunEventLoop polls events on a fixed tick until either the window closes or the optional
// stop channel receives. Per tick, it polls up to maxBatch events and invokes onTick with
// the batch; if onTick returns false, the loop exits early. Pass nil for stop or onTick if unused.
func RunEventLoop(stop <-chan struct{}, tick time.Duration, maxBatch int, onTick func([]Event) bool) {
	if tick <= 0 {
		tick = 15 * time.Millisecond
	}
	if maxBatch <= 0 {
		maxBatch = 32
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// proceed
		default:
			// If no tick yet, still allow immediate stop
		}
		select {
		case <-stop:
			return
		default:
		}

		evs, _ := PollEvents(maxBatch)
		closed := false
		for _, ev := range evs {
			if ev.Kind == EventKindClosed {
				closed = true
				break
			}
		}
		if onTick != nil {
			if !onTick(evs) {
				return
			}
		}
		ResetKeyTransitions()
		if closed || WindowShouldClose() {
			return
		}
		// Wait for next tick or stop signal
		select {
		case <-stop:
			return
		case <-ticker.C:
			// next iteration
		}
	}
}

// RunEventLoopWithContext is a convenience wrapper that stops when ctx.Done() fires
// or when the window should close, mirroring RunEventLoop semantics.
func RunEventLoopWithContext(ctx context.Context, tick time.Duration, maxBatch int, onTick func([]Event) bool) {
	if ctx == nil {
		RunEventLoop(nil, tick, maxBatch, onTick)
		return
	}
	RunEventLoop(ctx.Done(), tick, maxBatch, onTick)
}

// -----------------------------------------------------------------------------
// Paced loop and frame timing helpers
// -----------------------------------------------------------------------------

// SetTargetFPS sets the desired maximum frames per second for RunPacedLoop.
// Values <=0 are clamped to 60.
func SetTargetFPS(fps int) {
	if fps <= 0 {
		fps = 60
	}
	if fps > 1000 {
		fps = 1000
	}
	atomic.StoreInt32(&targetFPS, int32(fps))
}

// GetFrameTime returns seconds elapsed for the last completed frame.
func GetFrameTime() float64 {
	ns := atomic.LoadInt64(&lastFrameNS)
	if ns <= 0 {
		// Derive from target FPS if no frame has completed yet
		fps := atomic.LoadInt32(&targetFPS)
		if fps <= 0 {
			fps = 60
		}
		return 1.0 / float64(fps)
	}
	return float64(ns) / 1e9
}

// GetTime returns seconds elapsed since Init() completed.
func GetTime() float64 {
	if (timeStart == time.Time{}) {
		return 0
	}
	return time.Since(timeStart).Seconds()
}

// GetFPS returns the instantaneous FPS computed from the last frame time
// (rounded). If not yet available, returns the target FPS.
func GetFPS() int {
	ns := atomic.LoadInt64(&lastFrameNS)
	if ns <= 0 {
		fps := atomic.LoadInt32(&targetFPS)
		if fps <= 0 {
			fps = 60
		}
		return int(fps)
	}
	dt := float64(ns) / 1e9
	if dt <= 0 {
		return int(atomic.LoadInt32(&targetFPS))
	}
	v := int(math.Round(1.0 / dt))
	if v < 1 {
		v = 1
	}
	if v > 100000 {
		v = 100000
	}
	return v
}

// RunPacedLoop runs a simple loop paced at the current target FPS (default 60).
// Each iteration polls events (with transitions reset) and invokes onTick.
// The loop exits when the window should close or when onTick returns false.
func RunPacedLoop(onTick func([]Event) bool) {
	// Ensure timing base exists
	timeStartOnce.Do(func() { timeStart = time.Now() })
	for !WindowShouldClose() {
		frameStart := time.Now()

		evs := PollEventsFrame(32)
		if onTick != nil {
			if !onTick(evs) {
				break
			}
		}
		if WindowShouldClose() {
			break
		}

		// Pace to target FPS
		fps := atomic.LoadInt32(&targetFPS)
		if fps <= 0 {
			fps = 60
		}
		desiredNS := int64(math.Round(1e9 / float64(fps)))
		workNS := time.Since(frameStart).Nanoseconds()
		sleepNS := desiredNS - workNS
		if sleepNS > 0 {
			time.Sleep(time.Duration(sleepNS))
		}
		// Record full frame duration (work + sleep)
		atomic.StoreInt64(&lastFrameNS, time.Since(frameStart).Nanoseconds())
	}
}

// WaitForMainWindow blocks until a main window exists or timeout expires.
// Returns handle (possibly 0 if timeout hit).
func WaitForMainWindow(timeout time.Duration) Handle {
	deadline := time.Now().Add(timeout)
	for {
		h := GetMainWindow()
		if h != 0 {
			return h
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// RuntimeState provides a diagnostic snapshot of native state.
type RuntimeState struct {
	WindowReady       bool
	ShutdownRequested bool
	ControlsCount     int
}

// GetRuntimeState fetches current native runtime state (best-effort).
func GetRuntimeState() RuntimeState {
	var rs RuntimeState
	if pGetRuntimeState == nil {
		return rs
	}
	var ready, shut, count int32
	pGetRuntimeState.Call(uintptr(unsafe.Pointer(&ready)), uintptr(unsafe.Pointer(&shut)), uintptr(unsafe.Pointer(&count)))
	rs.WindowReady = ready != 0
	rs.ShutdownRequested = shut != 0
	rs.ControlsCount = int(count)
	return rs
}

// SetWindowTitle sets window title.
func SetWindowTitle(title string) {
	if pSetWindowTitle != nil {
		t16, _ := syscall.UTF16PtrFromString(title)
		pSetWindowTitle.Call(uintptr(unsafe.Pointer(t16)))
	}
	// track title for hwnd discovery; invalidate cached hwnd
	hwndMu.Lock()
	lastWindowTitle = title
	cachedHWND = 0
	hwndMu.Unlock()
}

// GetWindowSize returns width/height.
func GetWindowSize() (w, h float64) {
	if pGetWindowSize == nil {
		return
	}
	var wf, hf float64
	pGetWindowSize.Call(uintptr(unsafe.Pointer(&wf)), uintptr(unsafe.Pointer(&hf)))
	return wf, hf
}

// Color represents a 32-bit ARGB color (0xAARRGGBB).
// Methods provided for extracting channels; creation helpers ease construction.
type Color uint32

// NewColor returns a Color from 8-bit channels (alpha, red, green, blue).
// NewColor constructs a Color from channel integers (0..255). Values outside
// the range are clamped. Accepting int makes call sites more ergonomic.
func NewColor(a, r, g, b int) Color {
	clamp := func(v int) uint32 {
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		return uint32(v)
	}
	return Color(clamp(a)<<24 | clamp(r)<<16 | clamp(g)<<8 | clamp(b))
}

// ARGB returns individual 8-bit channels.
func (c Color) ARGB() (a, r, g, b uint8) {
	v := uint32(c)
	return uint8(v >> 24), uint8(v >> 16), uint8(v >> 8), uint8(v)
}

// SetWindowBackgroundColor sets window background using a Color (0xAARRGGBB).
func SetWindowBackgroundColor(c Color) {
	if pSetWindowBackgroundColor == nil {
		return
	}
	a, r, g, b := c.ARGB()
	pSetWindowBackgroundColor.Call(uintptr(a), uintptr(r), uintptr(g), uintptr(b))
}

// RegisterResizeHandler installs a resize callback. If debounce>0, the handler
// is invoked only after no further resize events occur for that duration.
// If h is nil the handler is unregistered. Passing debounce<=0 registers an
// immediate (non-debounced) handler. Replaces any existing handler.
func RegisterResizeHandler(h ResizeHandler, debounce time.Duration) {
	if h == nil {
		resizeHandlerMu.Lock()
		resizeHandler = nil
		resizeHandlerMu.Unlock()
		return
	}

	// Base immediate handler target (may be wrapped for debounce).
	target := h
	if debounce > 0 {
		if debounce <= 0 {
			debounce = 150 * time.Millisecond // guard
		}
		var mu sync.Mutex
		var timer *time.Timer
		var lastW, lastH int
		immediate := h
		target = func(w, hgt int) {
			mu.Lock()
			lastW, lastH = w, hgt
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				mu.Lock()
				lw, lh := lastW, lastH
				mu.Unlock()
				immediate(lw, lh)
			})
			mu.Unlock()
		}
	}

	resizeHandlerMu.Lock()
	resizeHandler = target
	resizeHandlerMu.Unlock()

	if pRegisterResizeCallback == nil {
		return
	}
	if resizeCallbackPtr == 0 {
		// Native signature now: void cb(uint64 widthBits, uint64 heightBits)
		// NewCallback requires: func(...uintptr) uintptr
		resizeCallbackPtr = syscall.NewCallback(func(wBits, hBits uintptr) uintptr {
			wf := math.Float64frombits(uint64(wBits))
			hf := math.Float64frombits(uint64(hBits))
			wi := int(math.Round(wf))
			hi := int(math.Round(hf))
			atomic.StoreUint32(&windowResizedFlag, 1)
			resizeHandlerMu.RLock()
			rh := resizeHandler
			resizeHandlerMu.RUnlock()
			if rh != nil {
				rh(wi, hi)
			}
			return 0
		})
	}
	pRegisterResizeCallback.Call(resizeCallbackPtr)
}

// DefaultResizeDebounce defines the default debounce used by OnResize.
// Adjust if you want a snappier or lazier resize callback in simple apps.
var DefaultResizeDebounce = 200 * time.Millisecond

// OnResize registers a resize handler with a sensible default debounce.
// Prefer this for simple apps; use OnResizeImmediate for per-event callbacks
// or RegisterResizeHandler for full control over debounce interval.
func OnResize(h ResizeHandler) { RegisterResizeHandler(h, DefaultResizeDebounce) }

// OnResizeImmediate registers a resize handler that fires on every native
// resize event without debouncing.
func OnResizeImmediate(h ResizeHandler) { RegisterResizeHandler(h, 0) }

// ensureResizeCallbackRegistered makes sure the native resize callback is set up
// even if the user never calls RegisterResizeHandler. This keeps IsWindowResized()
// and size queries responsive without extra user code.
func ensureResizeCallbackRegistered() {
	if pRegisterResizeCallback == nil {
		return
	}
	if resizeCallbackPtr == 0 {
		resizeCallbackPtr = syscall.NewCallback(func(wBits, hBits uintptr) uintptr {
			wf := math.Float64frombits(uint64(wBits))
			hf := math.Float64frombits(uint64(hBits))
			atomic.StoreUint32(&windowResizedFlag, 1)
			// If a user handler is present, invoke it
			resizeHandlerMu.RLock()
			rh := resizeHandler
			resizeHandlerMu.RUnlock()
			if rh != nil {
				wi := int(math.Round(wf))
				hi := int(math.Round(hf))
				rh(wi, hi)
			}
			return 0
		})
	}
	pRegisterResizeCallback.Call(resizeCallbackPtr)
}

// RegisterInputHandler installs a low-level input callback.
func RegisterInputHandler(h InputHandler) {
	inputHandlerMu.Lock()
	defer inputHandlerMu.Unlock()
	inputHandler = h
	if pRegisterInputCallback == nil {
		return
	}
	if inputCallbackPtr == 0 {
		// New packed native signature: (int kind, int codeWithMods, int action, uint64 packedXY)
		// codeWithMods: low 16 bits = code (vk or mouse button), high 16 bits = mods.
		// packedXY: low 32 bits = x, high 32 bits = y (unsigned); key events have x=y=0.
		inputCallbackPtr = syscall.NewCallback(func(kind, codeWithMods, action, packedXY uintptr) uintptr {
			ik := int(kind)
			cwm := uint32(codeWithMods)
			code := int(cwm & 0xFFFF)
			mods := int((cwm >> 16) & 0xFFFF)
			ac := int(action)
			pxy := uint64(packedXY)
			x := int(uint32(pxy & 0xFFFFFFFF))
			y := int(uint32(pxy >> 32))

			switch ik {
			case EventKindKey:
				keyStateMu.Lock()
				switch ac {
				case ActionDown:
					if !keyDown[code] {
						keyPressedOnce[code] = true
						keyPressQueue = append(keyPressQueue, code)
						keyDown[code] = true
						for _, r := range translateVKToRunes(code, mods) {
							charPressQueue = append(charPressQueue, int(r))
						}
					} else {
						keyRepeat[code] = true
					}
				case ActionUp:
					if keyDown[code] {
						keyReleasedOnce[code] = true
						delete(keyDown, code)
					}
				}
				currentMods = mods
				keyStateMu.Unlock()
			case EventKindMouse:
				mouseStateMu.Lock()
				mouseX, mouseY = x, y
				switch ac {
				case ActionDown:
					if !mouseDown[code] {
						mousePressedOnce[code] = true
						mouseDown[code] = true
					}
				case ActionUp:
					if mouseDown[code] {
						mouseReleasedOnce[code] = true
						delete(mouseDown, code)
					}
				}
				mouseStateMu.Unlock()
				keyStateMu.Lock()
				currentMods = mods
				keyStateMu.Unlock()
			}
			inputHandlerMu.RLock()
			ih := inputHandler
			inputHandlerMu.RUnlock()
			if ih != nil {
				ih(ik, code, ac, mods, x, y)
			}
			return 0
		})
	}
	pRegisterInputCallback.Call(inputCallbackPtr)
}

// ensureInputCallbackRegistered ensures the native input callback is installed
// so keyboard/mouse helpers work out of the box. If the user later calls
// RegisterInputHandler, their handler will be invoked after internal state updates.
func ensureInputCallbackRegistered() {
	if pRegisterInputCallback == nil {
		return
	}
	if inputCallbackPtr == 0 {
		inputCallbackPtr = syscall.NewCallback(func(kind, codeWithMods, action, packedXY uintptr) uintptr {
			ik := int(kind)
			cwm := uint32(codeWithMods)
			code := int(cwm & 0xFFFF)
			mods := int((cwm >> 16) & 0xFFFF)
			ac := int(action)
			pxy := uint64(packedXY)
			x := int(uint32(pxy & 0xFFFFFFFF))
			y := int(uint32(pxy >> 32))

			switch ik {
			case EventKindKey:
				keyStateMu.Lock()
				switch ac {
				case ActionDown:
					if !keyDown[code] {
						keyPressedOnce[code] = true
						keyPressQueue = append(keyPressQueue, code)
						keyDown[code] = true
						for _, r := range translateVKToRunes(code, mods) {
							charPressQueue = append(charPressQueue, int(r))
						}
					} else {
						keyRepeat[code] = true
					}
				case ActionUp:
					if keyDown[code] {
						keyReleasedOnce[code] = true
						delete(keyDown, code)
					}
				}
				currentMods = mods
				keyStateMu.Unlock()
			case EventKindMouse:
				mouseStateMu.Lock()
				mouseX, mouseY = x, y
				switch ac {
				case ActionDown:
					if !mouseDown[code] {
						mousePressedOnce[code] = true
						mouseDown[code] = true
					}
				case ActionUp:
					if mouseDown[code] {
						mouseReleasedOnce[code] = true
						delete(mouseDown, code)
					}
				}
				mouseStateMu.Unlock()
				keyStateMu.Lock()
				currentMods = mods
				keyStateMu.Unlock()
			}
			inputHandlerMu.RLock()
			ih := inputHandler
			inputHandlerMu.RUnlock()
			if ih != nil {
				ih(ik, code, ac, mods, x, y)
			}
			return 0
		})
	}
	pRegisterInputCallback.Call(inputCallbackPtr)
}

// RegisterCloseHandler installs a callback invoked immediately when the native
// window Closed event fires (before Shutdown completes). Only one handler is stored.
func RegisterCloseHandler(fn func()) {
	closeHandlerMu.Lock()
	closeHandler = fn
	closeHandlerMu.Unlock()
	if pRegisterCloseCallback == nil {
		return
	}
	// Create callback once; native signature: void cb()
	if closeCallbackPtr == 0 {
		closeCallbackPtr = syscall.NewCallback(func() uintptr {
			closeHandlerMu.RLock()
			ch := closeHandler
			closeHandlerMu.RUnlock()
			if ch != nil {
				ch()
			}
			return 0
		})
	}
	pRegisterCloseCallback.Call(closeCallbackPtr)
}

// PollEvents retrieves pending events (batched). Returns slice len==n copied.
func PollEvents(max int) ([]Event, bool) {
	if pPollEvents == nil || max <= 0 {
		return nil, false
	}
	buf := make([]Event, max)
	var more int32
	n, _, _ := pPollEvents.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(int32(max)), uintptr(unsafe.Pointer(&more)))
	count := int(n)
	if count < 0 || count > max {
		count = 0
	}
	return buf[:count], more != 0
}

// PollEventsFrame polls up to max events then performs per-frame housekeeping
// by calling ResetKeyTransitions(). Prefer this in simple loops where you do
// not need to manually control the timing of transition resets.
func PollEventsFrame(max int) []Event {
	evs, _ := PollEvents(max)
	ResetKeyTransitions()
	return evs
}

// Screen/Monitor metrics ----------------------------------------------------

func GetScreenWidth() int {
	if procGetSystemMetrics.Find() != nil {
		return 0
	}
	w, _, _ := procGetSystemMetrics.Call(uintptr(SM_CXSCREEN))
	return int(int32(w))
}
func GetScreenHeight() int {
	if procGetSystemMetrics.Find() != nil {
		return 0
	}
	h, _, _ := procGetSystemMetrics.Call(uintptr(SM_CYSCREEN))
	return int(int32(h))
}

// Window rectangle and movement -------------------------------------------

// GetWindowPosition returns the top-left client window position in screen coords.
func GetWindowPosition() (x, y int) {
	h := getHWND()
	if h == 0 || procGetWindowRect.Find() != nil {
		return 0, 0
	}
	var rc rect
	procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	return int(rc.Left), int(rc.Top)
}

// SetWindowPosition moves the window to x,y.
func SetWindowPosition(x, y int) {
	h := getHWND()
	if h == 0 || procSetWindowPos.Find() != nil {
		return
	}
	procSetWindowPos.Call(h, 0, uintptr(int32(x)), uintptr(int32(y)), 0, 0, uintptr(SWP_NOSIZE|SWP_NOZORDER|SWP_NOOWNERZORDER|SWP_NOSENDCHANGING))
}

// SetWindowSize resizes the outer window to width/height.
func SetWindowSize(width, height int) {
	h := getHWND()
	if h == 0 || procSetWindowPos.Find() != nil {
		return
	}
	procSetWindowPos.Call(h, 0, 0, 0, uintptr(int32(width)), uintptr(int32(height)), uintptr(SWP_NOMOVE|SWP_NOZORDER|SWP_NOOWNERZORDER|SWP_NOSENDCHANGING|SWP_FRAMECHANGED))
}

// GetWindowOuterSize returns the full window rectangle size (including non-client frame).
func GetWindowOuterSize() (w, h int) {
	hWnd := getHWND()
	if hWnd == 0 || procGetWindowRect.Find() != nil {
		return 0, 0
	}
	var rc rect
	procGetWindowRect.Call(hWnd, uintptr(unsafe.Pointer(&rc)))
	return int(rc.Right - rc.Left), int(rc.Bottom - rc.Top)
}

// MaximizeWindow maximizes the window.
func MaximizeWindow() {
	h := getHWND()
	if h != 0 && procShowWindow.Find() == nil {
		procShowWindow.Call(h, uintptr(SW_MAXIMIZE))
	}
}

// MinimizeWindow minimizes the window.
func MinimizeWindow() {
	h := getHWND()
	if h != 0 && procShowWindow.Find() == nil {
		procShowWindow.Call(h, uintptr(SW_MINIMIZE))
	}
}

// RestoreWindow restores the window from minimized/maximized.
func RestoreWindow() {
	h := getHWND()
	if h != 0 && procShowWindow.Find() == nil {
		procShowWindow.Call(h, uintptr(SW_RESTORE))
	}
}

// SetWindowOpacity sets window alpha 0..1 for layered windows.
func SetWindowOpacity(alpha float64) {
	if alpha < 0 {
		alpha = 0
	} else if alpha > 1 {
		alpha = 1
	}
	h := getHWND()
	if h == 0 || procGetWindowLongPtrW.Find() != nil || procSetWindowLongPtrW.Find() != nil || procSetLayeredAttr.Find() != nil {
		return
	}
	// ensure layered style
	idxEx := int32(GWL_EXSTYLE)
	styleEx, _, _ := procGetWindowLongPtrW.Call(h, uintptr(idxEx))
	if (styleEx & WS_EX_LAYERED) == 0 {
		procSetWindowLongPtrW.Call(h, uintptr(idxEx), styleEx|WS_EX_LAYERED)
	}
	a := byte(int(math.Round(alpha * 255)))
	procSetLayeredAttr.Call(h, 0, uintptr(a), uintptr(LWA_ALPHA))
}

// DPI scale ----------------------------------------------------------------

// GetWindowScaleDPI returns scale factors relative to 96 DPI.
func GetWindowScaleDPI() (sx, sy float64) {
	h := getHWND()
	if h == 0 || procGetDpiForWindow.Find() != nil {
		return 1, 1
	}
	dpi, _, _ := procGetDpiForWindow.Call(h)
	d := float64(uint32(dpi))
	if d <= 0 {
		return 1, 1
	}
	s := d / 96.0
	return s, s
}

// Fullscreen handling -------------------------------------------------------

// ToggleFullscreen switches between borderless fullscreen and windowed.
func ToggleFullscreen() {
	h := getHWND()
	if h == 0 || procGetWindowLongPtrW.Find() != nil || procSetWindowLongPtrW.Find() != nil || procSetWindowPos.Find() != nil || procGetWindowRect.Find() != nil {
		return
	}
	// detect current style: if caption/frame flags missing and popup present, assume borderless
	idxStyle := int32(GWL_STYLE)
	idxEx := int32(GWL_EXSTYLE)
	style, _, _ := procGetWindowLongPtrW.Call(h, uintptr(idxStyle))
	isBorderless := (style&WS_POPUP) != 0 && (style&WS_CAPTION) == 0 && (style&WS_THICKFRAME) == 0
	if !isBorderless {
		// save current
		var rc rect
		procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
		hwndMu.Lock()
		savedRect = rc
		savedStyle = style
		ex, _, _ := procGetWindowLongPtrW.Call(h, uintptr(idxEx))
		savedExStyle = ex
		hwndMu.Unlock()
		// set popup borderless and resize to screen
		procSetWindowLongPtrW.Call(h, uintptr(idxStyle), uintptr(WS_POPUP|WS_VISIBLE))
		sw := GetScreenWidth()
		sh := GetScreenHeight()
		procSetWindowPos.Call(h, 0, 0, 0, uintptr(int32(sw)), uintptr(int32(sh)), uintptr(SWP_NOZORDER|SWP_NOOWNERZORDER|SWP_FRAMECHANGED))
	} else {
		// restore
		hwndMu.Lock()
		rc := savedRect
		st := savedStyle
		ex := savedExStyle
		hwndMu.Unlock()
		if st != 0 {
			procSetWindowLongPtrW.Call(h, uintptr(idxStyle), st)
			procSetWindowLongPtrW.Call(h, uintptr(idxEx), ex)
			procSetWindowPos.Call(h, 0, uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right-rc.Left), uintptr(rc.Bottom-rc.Top), uintptr(SWP_NOZORDER|SWP_NOOWNERZORDER|SWP_FRAMECHANGED))
		}
	}
}

// Convenience APIs ----------------------------------------------------------

// GetWindowHandle returns the HWND, or 0 if not found.
func GetWindowHandle() uintptr { return getHWND() }

// IsWindowFullscreen tries to detect borderless fullscreen state.
func IsWindowFullscreen() bool {
	h := getHWND()
	if h == 0 || procGetWindowLongPtrW.Find() != nil {
		return false
	}
	idxStyle := int32(GWL_STYLE)
	style, _, _ := procGetWindowLongPtrW.Call(h, uintptr(idxStyle))
	return (style&WS_POPUP) != 0 && (style&WS_CAPTION) == 0 && (style&WS_THICKFRAME) == 0
}

// ShowWindow shows the window if hidden; HideWindow hides it.
func ShowWindowIfHidden() {
	h := getHWND()
	if h != 0 && procShowWindow.Find() == nil {
		procShowWindow.Call(h, uintptr(SW_SHOW))
	}
}
func HideWindow() {
	h := getHWND()
	if h != 0 && procShowWindow.Find() == nil {
		procShowWindow.Call(h, uintptr(SW_HIDE))
	}
}

// CloseWindow requests shutdown.
func CloseWindow() { BeginShutdownAsync() }

// Min/Max size hints (stored only; not enforced without native hook)
var (
	minSizeMu  sync.Mutex
	minW, minH int
	maxW, maxH int
)

func SetWindowMinSize(w, h int) {
	minSizeMu.Lock()
	minW, minH = w, h
	minSizeMu.Unlock()
	ApplyMinMaxConstraints()
}
func SetWindowMaxSize(w, h int) {
	minSizeMu.Lock()
	maxW, maxH = w, h
	minSizeMu.Unlock()
	ApplyMinMaxConstraints()
}

// Apply currently stored min/max to native window constraints.
func ApplyMinMaxConstraints() {
	if pSetWindowMinMax == nil {
		return
	}
	minSizeMu.Lock()
	wmin, hmin, wmax, hmax := minW, minH, maxW, maxH
	minSizeMu.Unlock()
	pSetWindowMinMax.Call(uintptr(int32(wmin)), uintptr(int32(hmin)), uintptr(int32(wmax)), uintptr(int32(hmax)))
}
func GetWindowMinSize() (int, int) {
	minSizeMu.Lock()
	w, h := minW, minH
	minSizeMu.Unlock()
	return w, h
}
func GetWindowMaxSize() (int, int) {
	minSizeMu.Lock()
	w, h := maxW, maxH
	minSizeMu.Unlock()
	return w, h
}

// Monitor wrappers
func GetMonitorWidth() int  { return GetScreenWidth() }
func GetMonitorHeight() int { return GetScreenHeight() }

// GetWindowSizeInt returns rounded integer size.
func GetWindowSizeInt() (w, h int) {
	wf, hf := GetWindowSize()
	return int(math.Round(wf)), int(math.Round(hf))
}

// GetWindowClientSize returns the client-area width/height via GetClientRect.
func GetWindowClientSize() (w, h int) {
	hWnd := getHWND()
	if hWnd == 0 || procGetClientRect.Find() != nil {
		return 0, 0
	}
	var rc rect
	// reuse rect layout (Left/Top/Right/Bottom as int32)
	r1, _, _ := procGetClientRect.Call(hWnd, uintptr(unsafe.Pointer(&rc)))
	if r1 == 0 {
		return 0, 0
	}
	return int(rc.Right - rc.Left), int(rc.Bottom - rc.Top)
}

// Window state queries ------------------------------------------------------

// IsWindowHidden returns true if the window is not visible.
func IsWindowHidden() bool {
	h := getHWND()
	if h == 0 || procIsWindowVisible.Find() != nil {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(h)
	return r == 0
}

// IsWindowMinimized returns true if minimized (iconic).
func IsWindowMinimized() bool {
	h := getHWND()
	if h == 0 || procIsIconic.Find() != nil {
		return false
	}
	r, _, _ := procIsIconic.Call(h)
	return r != 0
}

// IsWindowMaximized returns true if maximized.
func IsWindowMaximized() bool {
	h := getHWND()
	if h == 0 || procIsZoomed.Find() != nil {
		return false
	}
	r, _, _ := procIsZoomed.Call(h)
	return r != 0
}

// IsWindowFocused returns true if the window is foreground.
func IsWindowFocused() bool {
	h := getHWND()
	if h == 0 || procGetForegroundWnd.Find() != nil {
		return false
	}
	f, _, _ := procGetForegroundWnd.Call()
	return f == h
}

// IsWindowResized returns true if a resize happened since last ResetKeyTransitions.
func IsWindowResized() bool { return atomic.LoadUint32(&windowResizedFlag) != 0 }

// SetWindowFocused brings the window to foreground, if possible.
func SetWindowFocused() {
	h := getHWND()
	if h != 0 && procSetForegroundWnd.Find() == nil {
		procSetForegroundWnd.Call(h)
	}
}

// Run provides a minimal, raylib-style loop: it paces to SetTargetFPS(),
// internally polls events and manages per-frame input transitions, and calls
// update() each frame. Return false from update() to exit early. The function
// also waits briefly for the native close-callback to fire before returning to
// avoid shutdown races.
func Run(update func() bool) {
	closed := make(chan struct{}, 1)
	// Tee the existing user close handler
	closeHandlerMu.Lock()
	prev := closeHandler
	closeHandler = func() {
		if prev != nil {
			prev()
		}
		select {
		case closed <- struct{}{}:
		default:
		}
	}
	closeHandlerMu.Unlock()
	if pRegisterCloseCallback != nil {
		if closeCallbackPtr == 0 {
			closeCallbackPtr = syscall.NewCallback(func() uintptr {
				closeHandlerMu.RLock()
				ch := closeHandler
				closeHandlerMu.RUnlock()
				if ch != nil {
					ch()
				}
				return 0
			})
		}
		pRegisterCloseCallback.Call(closeCallbackPtr)
	}

	timeStartOnce.Do(func() { timeStart = time.Now() })
	for !WindowShouldClose() {
		frameStart := time.Now()

		// Re-check just before any native calls to avoid race on teardown
		if WindowShouldClose() {
			break
		}

		// Poll events; low-level callbacks may also enqueue input asynchronously
		_, _ = PollEvents(64)
		if update != nil {
			if !update() {
				break
			}
		}
		if WindowShouldClose() {
			break
		}

		// Clear per-frame transitions immediately after update so
		// input that occurs during the sleep phase is preserved for
		// the next frame's update.
		ResetKeyTransitions()

		fps := atomic.LoadInt32(&targetFPS)
		if fps <= 0 {
			fps = 60
		}
		desiredNS := int64(math.Round(1e9 / float64(fps)))
		workNS := time.Since(frameStart).Nanoseconds()
		if sleepNS := desiredNS - workNS; sleepNS > 0 {
			time.Sleep(time.Duration(sleepNS))
		}
		atomic.StoreInt64(&lastFrameNS, time.Since(frameStart).Nanoseconds())
	}

	select {
	case <-closed:
	case <-time.After(1500 * time.Millisecond):
	}
}
