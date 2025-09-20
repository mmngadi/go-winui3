package winui

import (
	"context"
	"sync"
	"time"
)

// WindowContext is a simple per-window key-value store.
type WindowContext struct {
	mu sync.RWMutex
	m  map[string]any
}

func NewWindowContext() *WindowContext { return &WindowContext{m: make(map[string]any)} }

func (wc *WindowContext) Set(key string, value any) {
	wc.mu.Lock()
	wc.m[key] = value
	wc.mu.Unlock()
}

func (wc *WindowContext) Get(key string) (any, bool) {
	wc.mu.RLock()
	v, ok := wc.m[key]
	wc.mu.RUnlock()
	return v, ok
}

// MustGet returns the value for key, panicking if missing or wrong type.
func MustGet[T any](wc *WindowContext, key string) T {
	wc.mu.RLock()
	v, ok := wc.m[key]
	wc.mu.RUnlock()
	if !ok {
		panic("winui: WindowContext key not found: " + key)
	}
	vv, ok := v.(T)
	if !ok {
		panic("winui: WindowContext wrong type for key: " + key)
	}
	return vv
}

// Window is a high-level wrapper around the single native window instance.
// Methods are safe to call before creation; properties are applied on create.
type Window struct {
	mu sync.RWMutex

	// queued config (applied on creation if set)
	title      *string
	sizeW      *int
	sizeH      *int
	minW, minH *int
	maxW, maxH *int
	bgColor    *Color

	// lifecycle + state
	created       bool
	contentCalled bool
	ctx           *WindowContext

	// callbacks
	onCreate  []func(*Window, *WindowContext)
	onStart   []func(*Window, *WindowContext)
	onUpdate  []func(*Window, *WindowContext)
	onResume  []func(*Window, *WindowContext)
	onPause   []func(*Window, *WindowContext)
	onStop    []func(*Window, *WindowContext)
	onDestroy []func(*Window, *WindowContext)
	onResize  []func(*Window, *WindowContext, int, int)

	// optional content initializer (runs exactly once)
	content func(*Window, *WindowContext)
}

// InitWindowHandler returns a new high-level Window wrapper.
func InitWindowHandler() *Window {
	_ = Load() // best-effort; Run will ensure Init
	return &Window{ctx: NewWindowContext()}
}

func (w *Window) Handle() Handle          { return GetMainWindow() }
func (w *Window) Context() *WindowContext { return w.ctx }

// Run creates the native window if needed, applies queued properties,
// and drives the lifecycle loop until closed or ctx canceled.
func (w *Window) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Ensure runtime initialized
	if err := Init(); err != nil {
		// best-effort: if init fails, return
		return
	}

	// Create window if missing
	if !WindowExists() {
		tw, th := 1024, 768
		w.mu.RLock()
		if w.sizeW != nil {
			tw = *w.sizeW
		}
		if w.sizeH != nil {
			th = *w.sizeH
		}
		t := ""
		if w.title != nil {
			t = *w.title
		}
		w.mu.RUnlock()
		_, _ = CreateWindowAndWait(tw, th, t, 5*time.Second)
	} else {
		// Wait until ready if already in-flight
		_ = WaitUntilWindowReady(5 * time.Second)
	}

	// Apply queued configuration and emit OnCreate once
	w.mu.Lock()
	if !w.created {
		if w.title != nil {
			SetWindowTitle(*w.title)
		}
		if w.sizeW != nil && w.sizeH != nil {
			w.applyClientSize(*w.sizeW, *w.sizeH)
		}
		if w.minW != nil && w.minH != nil {
			SetWindowMinSize(*w.minW, *w.minH)
		}
		if w.maxW != nil && w.maxH != nil {
			SetWindowMaxSize(*w.maxW, *w.maxH)
		}
		if w.bgColor != nil {
			SetWindowBackgroundColor(*w.bgColor)
		}
		w.created = true
		cbs := append([]func(*Window, *WindowContext){}, w.onCreate...)
		w.mu.Unlock()
		for _, fn := range cbs {
			w.safeCall(func() { fn(w, w.ctx) })
		}
		w.mu.Lock()
		if w.content != nil && !w.contentCalled {
			fn := w.content
			w.contentCalled = true
			w.mu.Unlock()
			w.safeCall(func() { fn(w, w.ctx) })
			w.mu.Lock()
		}
	}
	w.mu.Unlock()

	// Start
	w.emitSimple(w.onStart)

	// Loop
	prevFocused := IsWindowFocused()
	for {
		select {
		case <-ctx.Done():
			BeginShutdownAsync()
		default:
		}
		if WindowShouldClose() {
			break
		}

		// poll events and run update callbacks
		_, _ = PollEvents(64)

		// forward resize into lifecycle if it occurred
		if IsWindowResized() {
			cw, ch := GetWindowClientSize()
			w.emitResize(cw, ch)
		}

		// focus transitions
		curFocused := IsWindowFocused()
		if curFocused && !prevFocused {
			w.emitSimple(w.onResume)
		} else if !curFocused && prevFocused {
			w.emitSimple(w.onPause)
		}
		prevFocused = curFocused

		// OnUpdate
		w.emitSimple(w.onUpdate)

		// Clear per-frame transitions after update
		ResetKeyTransitions()

		// Pace similar to Run()
		fps := GetFPS()
		if fps <= 0 {
			fps = 60
		}
		time.Sleep(time.Duration(float64(time.Second) / float64(fps)))
	}

	// Stop + Destroy - using safeCall to prevent panics from callbacks
	// First clear all event handlers
	ResetInputCallbacks()
	ResetResizeCallback()

	// Execute lifecycle events
	w.emitSimple(w.onStop)
	w.emitSimple(w.onDestroy)

	// Ensure all callbacks are cleared before final shutdown
	w.mu.Lock()
	w.onCreate = nil
	w.onStart = nil
	w.onUpdate = nil
	w.onResume = nil
	w.onPause = nil
	w.onStop = nil
	w.onDestroy = nil
	w.onResize = nil
	w.content = nil
	w.ctx = nil
	w.mu.Unlock()

	// Wait a bit to ensure any pending callbacks have completed
	time.Sleep(50 * time.Millisecond)

	// If native shutdown was already requested (e.g., via window close or ESC),
	// do not call Shutdown() again. Instead, wait briefly for teardown to finish.
	// This avoids redundant native calls and potential races during process exit.
	if GetRuntimeState().ShutdownRequested {
		deadline := time.Now().Add(2500 * time.Millisecond)
		for WindowExists() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		return
	}

	// Otherwise, perform synchronous shutdown so native UI thread joins before returning.
	Shutdown()
}

// emitSimple invokes callbacks with panic recovery.
func (w *Window) emitSimple(fns []func(*Window, *WindowContext)) {
	w.mu.RLock()
	cbs := append([]func(*Window, *WindowContext){}, fns...)
	w.mu.RUnlock()
	for _, fn := range cbs {
		w.safeCall(func() { fn(w, w.ctx) })
	}
}

func (w *Window) emitResize(width, height int) {
	w.mu.RLock()
	cbs := append([]func(*Window, *WindowContext, int, int){}, w.onResize...)
	w.mu.RUnlock()
	for _, fn := range cbs {
		w.safeCall(func() { fn(w, w.ctx, width, height) })
	}
}

func (w *Window) safeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// SetContent registers content initializer to run once (pre or post creation).
func (w *Window) SetContent(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	if w.contentCalled {
		w.mu.Unlock()
		// Already ran; invoke immediately for ergonomics
		w.safeCall(func() { fn(w, w.ctx) })
		return
	}
	w.content = fn
	// If already created, run now
	if w.created && !w.contentCalled {
		w.contentCalled = true
		f := w.content
		w.mu.Unlock()
		w.safeCall(func() { f(w, w.ctx) })
		return
	}
	w.mu.Unlock()
}

// Callback registration -----------------------------------------------------
func (w *Window) OnCreate(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onCreate = append(w.onCreate, fn)
	w.mu.Unlock()
}
func (w *Window) OnStart(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onStart = append(w.onStart, fn)
	w.mu.Unlock()
}
func (w *Window) OnUpdate(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onUpdate = append(w.onUpdate, fn)
	w.mu.Unlock()
}
func (w *Window) OnResume(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onResume = append(w.onResume, fn)
	w.mu.Unlock()
}
func (w *Window) OnPause(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onPause = append(w.onPause, fn)
	w.mu.Unlock()
}
func (w *Window) OnStop(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onStop = append(w.onStop, fn)
	w.mu.Unlock()
}
func (w *Window) OnDestroy(fn func(*Window, *WindowContext)) {
	w.mu.Lock()
	w.onDestroy = append(w.onDestroy, fn)
	w.mu.Unlock()
}
func (w *Window) OnResize(fn func(*Window, *WindowContext, int, int)) {
	w.mu.Lock()
	w.onResize = append(w.onResize, fn)
	w.mu.Unlock()
}

// Config/properties ---------------------------------------------------------
func (w *Window) SetTitle(title string) {
	w.mu.Lock()
	w.title = &title
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowTitle(title)
	}
}

func (w *Window) SetBackgroundColor(c Color) {
	w.mu.Lock()
	w.bgColor = &c
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowBackgroundColor(c)
	}
}

// SetSize sets desired client size.
func (w *Window) SetSize(width, height int) {
	w.mu.Lock()
	w.sizeW, w.sizeH = &width, &height
	created := w.created
	w.mu.Unlock()
	if created {
		w.applyClientSize(width, height)
	}
}

func (w *Window) SetMinSize(width, height int) { w.SetMinWidth(width); w.SetMinHeight(height) }
func (w *Window) SetMaxSize(width, height int) { w.SetMaxWidth(width); w.SetMaxHeight(height) }

func (w *Window) SetMinWidth(width int) {
	w.mu.Lock()
	w.minW = &width
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowMinSize(width, w.currentOrZero(w.minH))
	}
}
func (w *Window) SetMinHeight(height int) {
	w.mu.Lock()
	w.minH = &height
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowMinSize(w.currentOrZero(w.minW), height)
	}
}
func (w *Window) SetMaxWidth(width int) {
	w.mu.Lock()
	w.maxW = &width
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowMaxSize(width, w.currentOrZero(w.maxH))
	}
}
func (w *Window) SetMaxHeight(height int) {
	w.mu.Lock()
	w.maxH = &height
	created := w.created
	w.mu.Unlock()
	if created {
		SetWindowMaxSize(w.currentOrZero(w.maxW), height)
	}
}

func (w *Window) currentOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// Size getters
func (w *Window) Size() (int, int)       { return GetWindowSizeInt() }
func (w *Window) ClientSize() (int, int) { return GetWindowClientSize() }
func (w *Window) OuterSize() (int, int)  { return GetWindowOuterSize() }

// Position, DPI, and state
func (w *Window) GetPosition() (int, int)      { return GetWindowPosition() }
func (w *Window) SetPosition(x, y int)         { SetWindowPosition(x, y) }
func (w *Window) DPIScale() (float64, float64) { return GetWindowScaleDPI() }
func (w *Window) IsFullscreen() bool           { return IsWindowFullscreen() }
func (w *Window) ToggleFullscreen()            { ToggleFullscreen() }
func (w *Window) ToggleBorderlessWindowed()    { ToggleFullscreen() }
func (w *Window) MaximizeWindow()              { MaximizeWindow() }
func (w *Window) MinimizeWindow()              { MinimizeWindow() }
func (w *Window) RestoreWindow()               { RestoreWindow() }

// Input wrappers (keyboard)
func (w *Window) GetKeyPressed() int              { return GetKeyPressed() }
func (w *Window) GetCharPressed() int             { return GetCharPressed() }
func (w *Window) IsKeyDown(key int) bool          { return IsKeyDown(key) }
func (w *Window) IsKeyPressed(key int) bool       { return IsKeyPressed(key) }
func (w *Window) IsKeyReleased(key int) bool      { return IsKeyReleased(key) }
func (w *Window) IsKeyPressedRepeat(key int) bool { return IsKeyPressedRepeat(key) }
func (w *Window) GetModifiers() int               { return GetModifiers() }
func (w *Window) IsShiftDown() bool               { return IsShiftDown() }
func (w *Window) IsControlDown() bool             { return IsControlDown() }
func (w *Window) IsAltDown() bool                 { return IsAltDown() }

// Input wrappers (mouse)
func (w *Window) IsMouseButtonDown(btn int) bool     { return IsMouseButtonDown(btn) }
func (w *Window) IsMouseButtonUp(btn int) bool       { return IsMouseButtonUp(btn) }
func (w *Window) IsMouseButtonPressed(btn int) bool  { return IsMouseButtonPressed(btn) }
func (w *Window) IsMouseButtonReleased(btn int) bool { return IsMouseButtonReleased(btn) }
func (w *Window) MouseGetPosition() (int, int)       { return GetMousePosition() }
func (w *Window) MouseGetX() int                     { x, _ := GetMousePosition(); return x }
func (w *Window) MouseGetY() int                     { _, y := GetMousePosition(); return y }

// helpers ------------------------------------------------------------------

// applyClientSize attempts to set the client size by accounting for the current
// non-client frame thickness.
func (w *Window) applyClientSize(cw, ch int) {
	ow, oh := GetWindowOuterSize()
	iw, ih := GetWindowClientSize()
	if iw <= 0 || ih <= 0 || ow <= 0 || oh <= 0 {
		// fallback to outer size
		SetWindowSize(cw, ch)
		return
	}
	dx := ow - iw
	dy := oh - ih
	if dx < 0 {
		dx = 0
	}
	if dy < 0 {
		dy = 0
	}
	SetWindowSize(cw+dx, ch+dy)
}
