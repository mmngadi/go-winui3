Add the below
- High-level, Android-style lifecycle API centered around a Window type.
- Ergonomic per-window wrappers for window management and DPI utilities.
- Ergonomic per-window input wrappers for keyboard and mouse.
- Low-level API modernization: Vector2 removed, GetMousePosition now returns (int, int).
- Updated examples and README; low-level API remains available for advanced use.

High-level lifecycle API (Android-style)
- Window lifecycle events:
  - OnCreate: once when the native window is created (applies queued title/size).
  - OnStart: loop begins.
  - OnUpdate: per-frame callback after PollEventsFrame, where input state is guaranteed current.
  - OnResume / OnPause: focus transitions (based on IsWindowFocused()).
  - OnResize: window size changed.
  - OnStop: loop ends.
  - OnDestroy: native close or context cancellation.
- Run(ctx): creates the native window if missing, applies queued SetTitle/SetSize, drives the lifecycle, and handles focus-based Resume/Pause.
- WindowContext: per-window key-value store with:
  - Set(key, value)
  - Get(key) (value, ok)
  - MustGet[T](ctx, key) T for type-safe retrieval.
- SetContent semantics:
  - Can be registered before creation or inside OnCreate.
  - Runs exactly once; if the window is already created, runs immediately (guarded by contentCalled).
- Safety and ergonomics:
  - Thread-safe callback registration and emission with panic recovery.
  - Resize events are forwarded into the lifecycle.

Ergonomic per-window wrappers (window and DPI)
- Setup and configuration:
  - SetTitle(title)
  - SetSize(w, h)
  - SetBackgroundColor(color)
  - SetMinSize(w, h), SetMaxSize(w, h)
  - SetMinWidth(w), SetMinHeight(h), SetMaxWidth(w), SetMaxHeight(h)
  - Size getters: Size(), ClientSize(), OuterSize()
- Window state and runtime utilities:
  - IsFullscreen()
  - GetPosition() (x, y), SetPosition(x, y)
  - DPIScale() (sx, sy)
  - ToggleFullscreen(), ToggleBorderlessWindowed() (alias)
  - MaximizeWindow(), MinimizeWindow(), RestoreWindow()

Ergonomic per-window input wrappers
- Keyboard:
  - GetKeyPressed(), GetCharPressed()
  - IsKeyDown(key), IsKeyPressed(key), IsKeyReleased(key), IsKeyPressedRepeat(key)
  - GetModifiers(), IsShiftDown(), IsControlDown(), IsAltDown()
- Mouse:
  - IsMouseButtonDown(btn), IsMouseButtonUp(btn)
  - IsMouseButtonPressed(btn), IsMouseButtonReleased(btn)
  - MouseGetPosition() (px, py), MouseGetX(), MouseGetY()
- Implementation detail: these are thin delegates to the existing root-level helpers, scoped for ergonomics.

Low-level API modernization
- Removed: Vector2.
- Changed: GetMousePosition() now returns (int, int).
- All call sites and examples updated to use px, py := winui.GetMousePosition().

Examples and documentation
- examples/minimal:
  - Converted to the lifecycle API and demonstrates:
    - Pre-create SetTitle/SetSize/constraints.
    - OnCreate/OnStart/OnResume/OnPause/OnResize/OnUpdate/OnStop/OnDestroy.
    - Using per-window input wrappers and window manipulation in OnUpdate.
- examples/yourgoapp:
  - Updated to use px, py := winui.GetMousePosition().
- README:
  - Leads with the lifecycle API as the recommended approach.
  - Documents the per-window wrappers and new input methods.
  - Notes Vector2 removal and new mouse position signature.
  - Keeps the low-level API documented for advanced use.

Compatibility and migration
- Backwards compatibility: all existing low-level helpers remain.
- Migration:
  - Replace any p := winui.GetMousePosition(); p.X, p.Y with px, py := winui.GetMousePosition().
  - Prefer lifecycle API (InitWindowHandler + Run(ctx)) for new code; keep low-level API for specialized scenarios.

Key benefits
- Ergonomics: cleaner, object-oriented surface around window and input.
- Correctness: OnUpdate ensures input reflects current frame state.
- Idiomatic Go: (int, int) return values instead of Vector2 for coordinates.
- Safety: guarded, thread-safe callbacks with panic recovery.
- Extensibility: unified, consistent API poised for future features.

README.MD
```
# go-winui3 — High-Level Lifecycle and Input API Specification

This spec documents the new Android‑style lifecycle API centered around a high-level `Window` type, ergonomic per-window wrappers for input and window management, and the low-level mouse API modernization.

The API is designed to:
- Provide a predictable lifecycle with per-frame updates (input state is current within `OnUpdate`).
- Offer ergonomic per-window methods for window, DPI, and input handling.
- Preserve low-level functions for advanced scenarios.

---

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "runtime"

    winui "github.com/mmngadi/go-winui3/internal/winui"
)

func main() {
    runtime.LockOSThread()

    hwnd := winui.InitWindowHandler()

    // Pre-create configuration (applied on window creation)
    hwnd.SetTitle("My App")
    hwnd.SetSize(1024, 768)
    hwnd.SetMinWidth(640)
    hwnd.SetMinHeight(480)

    // Lifecycle: create, then per-frame updates with current input state
    hwnd.OnCreate(func(w *winui.Window, ctx *winui.WindowContext) {
        w.SetBackgroundColor(winui.NewColor(255, 245, 245, 245))
        sx, sy := w.DPIScale()
        fmt.Printf("DPI scale: %.2fx, %.2fy\n", sx, sy)
    })

    hwnd.OnUpdate(func(w *winui.Window, ctx *winui.WindowContext) {
        // Drain key presses this frame
        for k := w.GetKeyPressed(); k != 0; k = w.GetKeyPressed() {
            fmt.Printf("Key pressed: 0x%X (repeat=%v)\n", k, w.IsKeyPressedRepeat(k))
            if k == 0x7A { // F11
                w.ToggleFullscreen()
            }
        }
        // Mouse example
        if w.IsMouseButtonPressed(winui.MouseButtonLeft) {
            x, y := w.MouseGetPosition()
            fmt.Printf("Left click at (%d, %d)\n", x, y)
        }
    })

    hwnd.OnResize(func(w *winui.Window, ctx *winui.WindowContext, width, height int) {
        fmt.Printf("Resized to: %dx%d\n", width, height)
    })

    // Run lifecycle loop (blocks until closed or context cancelled)
    hwnd.Run(context.Background())
}
```

---

## Package Overview

- High-level API entrypoint:
  - `InitWindowHandler() *Window`
- High-level types:
  - `type Window`
  - `type WindowContext`
- Low-level modernization:
  - `GetMousePosition() (int, int)` — returns `(x, y)`
  - Removed `Vector2` type

All high-level `Window` methods delegate to existing package-level helpers where applicable to maintain consistency.

---

## Types

### WindowContext

A per-window key/value store that persists across lifecycle events.

- Methods:
  - `func NewWindowContext() *WindowContext`
  - `func (wc *WindowContext) Set(key string, value interface{})`
  - `func (wc *WindowContext) Get(key string) (interface{}, bool)`

- Helper:
  - `func MustGet[T any](wc *WindowContext, key string) T`
    - Panics if key missing or wrong type.

Example:
```go
ctx.Set("frameCount", 42)
count := winui.MustGet[int](ctx, "frameCount")
```

---

### Window

Represents the main application window with lifecycle management, window utilities, and input wrappers.

Core:
- `func InitWindowHandler() *Window`
- `func (w *Window) Run(ctx context.Context)`
- `func (w *Window) Handle() Handle`
- `func (w *Window) Context() *WindowContext`

Configuration (pre/post creation):
- `func (w *Window) SetTitle(title string)`
- `func (w *Window) SetBackgroundColor(c Color)`
- `func (w *Window) SetSize(width, height int)`             // client area
- `func (w *Window) SetMinSize(width, height int)`
- `func (w *Window) SetMaxSize(width, height int)`
- `func (w *Window) SetMinWidth(width int)`
- `func (w *Window) SetMinHeight(height int)`
- `func (w *Window) SetMaxWidth(width int)`
- `func (w *Window) SetMaxHeight(height int)`

Size getters:
- `func (w *Window) Size() (int, int)`
- `func (w *Window) ClientSize() (int, int)`
- `func (w *Window) OuterSize() (int, int)`

Position, DPI, and window state:
- `func (w *Window) GetPosition() (int, int)`
- `func (w *Window) SetPosition(x, y int)`
- `func (w *Window) DPIScale() (float64, float64)`
- `func (w *Window) IsFullscreen() bool`
- `func (w *Window) ToggleFullscreen()`
- `func (w *Window) ToggleBorderlessWindowed()` // alias to ToggleFullscreen
- `func (w *Window) MaximizeWindow()`
- `func (w *Window) MinimizeWindow()`
- `func (w *Window) RestoreWindow()`

Lifecycle callbacks:
- `func (w *Window) OnCreate(fn func(*Window, *WindowContext))`
- `func (w *Window) OnStart(fn func(*Window, *WindowContext))`
- `func (w *Window) OnResume(fn func(*Window, *WindowContext))`
- `func (w *Window) OnPause(fn func(*Window, *WindowContext))`
- `func (w *Window) OnStop(fn func(*Window, *WindowContext))`
- `func (w *Window) OnDestroy(fn func(*Window, *WindowContext))`
- `func (w *Window) OnResize(fn func(*Window, *WindowContext, int, int))`
- `func (w *Window) OnUpdate(fn func(*Window, *WindowContext))`
  - Called once per loop tick after event polling; input state is current here.

Input wrappers — Keyboard:
- `func (w *Window) GetKeyPressed() int`
- `func (w *Window) GetCharPressed() int`
- `func (w *Window) IsKeyDown(key int) bool`
- `func (w *Window) IsKeyPressed(key int) bool`
- `func (w *Window) IsKeyReleased(key int) bool`
- `func (w *Window) IsKeyPressedRepeat(key int) bool`
- `func (w *Window) GetModifiers() int`
- `func (w *Window) IsShiftDown() bool`
- `func (w *Window) IsControlDown() bool`
- `func (w *Window) IsAltDown() bool`

Input wrappers — Mouse:
- `func (w *Window) IsMouseButtonDown(btn int) bool`
- `func (w *Window) IsMouseButtonUp(btn int) bool`
- `func (w *Window) IsMouseButtonPressed(btn int) bool`
- `func (w *Window) IsMouseButtonReleased(btn int) bool`
- `func (w *Window) MouseGetPosition() (int, int)`  // (x, y)
- `func (w *Window) MouseGetX() int`
- `func (w *Window) MouseGetY() int`

Notes:
- Input wrappers are thin delegates to the package-level input helpers; the wrappers exist for ergonomics and discoverability.
- Use constants such as `winui.MouseButtonLeft` for mouse buttons. See package constants for the full set.

---

## Lifecycle Semantics

Order of events driven by `Run(ctx)`:
1. Create (emitted once on native window creation)
2. Start (loop begins)
3. Loop:
   - Poll events and input
   - OnUpdate (per-frame; input state is current here)
   - Resume/Pause on focus transitions
4. Stop (loop ends)
5. Destroy (native close or context cancellation)

Focus handling:
- `OnResume` is fired when the window gains focus.
- `OnPause` is fired when the window loses focus.

Per-frame timing:
- `OnUpdate` runs after input/events are polled via the internal loop, ensuring `GetKeyPressed`, `IsKeyDown`, `IsMouseButtonPressed`, etc., reflect the current frame.

---

## Input Usage Patterns

Drain “key pressed” in the current frame:
```go
hwnd.OnUpdate(func(w *winui.Window, ctx *winui.WindowContext) {
    for k := w.GetKeyPressed(); k != 0; k = w.GetKeyPressed() {
        // Handle key press k (int virtual-key code)
    }
})
```

Detect modifier combinations (e.g., Shift+A):
```go
hwnd.OnUpdate(func(w *winui.Window, ctx *winui.WindowContext) {
    if w.IsShiftDown() && w.IsKeyPressed(0x41) { // 'A'
        // Handle Shift+A
    }
})
```

Mouse click with position:
```go
hwnd.OnUpdate(func(w *winui.Window, ctx *winui.WindowContext) {
    if w.IsMouseButtonPressed(winui.MouseButtonLeft) {
        x, y := w.MouseGetPosition()
        // Use x, y
    }
})
```

---

## Window Manipulation at Runtime

Move or toggle fullscreen during `OnUpdate`:
```go
hwnd.OnUpdate(func(w *winui.Window, ctx *winui.WindowContext) {
    // Nudge window when a character is pressed
    if c := w.GetCharPressed(); c != 0 {
        x, y := w.GetPosition()
        w.SetPosition(x+10, y)
    }

    // Toggle fullscreen on F11
    if w.IsKeyPressed(0x7A) {
        w.ToggleFullscreen()
    }
})
```

---

## Low-Level Mouse API Modernization

- Removed:
  - `type Vector2 struct{ X, Y int }`
- Changed:
  - `func GetMousePosition() (int, int)` now returns `(x, y)`

Migration:
```go
// Before:
p := winui.GetMousePosition()
fmt.Printf("Mouse at (%d,%d)\n", p.X, p.Y)

// After:
x, y := winui.GetMousePosition()
fmt.Printf("Mouse at (%d,%d)\n", x, y)
```

All examples and docs should use `px, py := winui.GetMousePosition()` going forward.

---

## Best Practices

- Always call `runtime.LockOSThread()` before any GUI work in `main()`.
- Prefer the lifecycle API (`InitWindowHandler()` + callbacks + `Run(ctx)`) for most apps.
- Use `OnUpdate` for per-frame logic; input state is guaranteed current there.
- Use `WindowContext` for sharing state between callbacks; prefer `MustGet[T]` when appropriate.

---

## Compatibility

- The lifecycle API is additive; low-level helpers remain available for advanced use.
- Existing code using low-level APIs continues to work, aside from the `GetMousePosition` signature change.
- Use the ergonomic `Window` methods for better readability and discoverability.

---
```