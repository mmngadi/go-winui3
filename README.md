# go-winui3 (Go + WinUI 3 via C++/WinRT)

Status: Experimental — heavy development in progress. APIs are unstable and may change without notice.

go-winui3 lets you build native Windows apps in Go using WinUI 3 without cgo. A small C++/WinRT bridge DLL hosts the WinUI 3 UI thread, and a pure Go wrapper dynamically calls the DLL exports via `golang.org/x/sys/windows`.

The goal is fast iteration with a single script that builds the native DLL and your Go example, producing a runnable app under `bin/<arch>/<config>`.

## Repository Layout

```
go-winui3/
  examples/minimal/       # Minimal lifecycle example
  examples/yourgoapp/     # Diagnostic/control example
  internal/winui/         # Go dynamic wrapper (syscall, no cgo)
  native/WinUI3Native/    # C++/WinRT project producing WinUI3Native.dll
  bin/<arch>/<config>/    # Unified output (DLLs + built example EXE)
  build.ps1               # Orchestrated build script
```

## Prerequisites

- Windows 10/11
- Go 1.21+
- Visual Studio Build Tools or MSBuild
- PowerShell 5+ or PowerShell 7+

## Build & Run

The build script requires `-ExamplePath` pointing to a directory with `main.go` or directly to a `main.go`.

- Build (Debug x64):
  `powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Debug -Platform x64 -Verbose -ExamplePath examples/minimal`
- Run:
  `./bin/x64/Debug/debug.exe`

Notes:
- The executable loads `WinUI3Native.dll` from `bin/<arch>/<config>`. The script copies required DLLs there.
- If running outside `bin/`, call `winui.Load("path/to/bin/x64/Debug")` before initialization or add that folder to `PATH`.

## Quick Start (Lifecycle API)

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

    w := winui.InitWindowHandler()
    w.SetTitle("My App")
    w.SetSize(1024, 768)
    w.SetMinWidth(640)
    w.SetMinHeight(480)

    w.OnCreate(func(win *winui.Window, ctx *winui.WindowContext) {
        win.SetBackgroundColor(winui.NewColor(255, 245, 245, 245))
        sx, sy := win.DPIScale()
        fmt.Printf("DPI scale: %.2fx, %.2fy\n", sx, sy)
    })

    w.OnUpdate(func(win *winui.Window, ctx *winui.WindowContext) {
        for k := win.GetKeyPressed(); k != 0; k = win.GetKeyPressed() {
            if k == 0x7A { // F11
                win.ToggleFullscreen()
            }
        }
        if win.IsMouseButtonPressed(winui.MouseButtonLeft) {
            x, y := win.MouseGetPosition()
            fmt.Printf("Left click at (%d, %d)\n", x, y)
        }
    })

    w.OnResize(func(win *winui.Window, ctx *winui.WindowContext, width, height int) {
        fmt.Printf("Resized to: %dx%d\n", width, height)
    })

    w.Run(context.Background())
}
```

## High-Level Concepts

- Lifecycle callbacks: `OnCreate`, `OnStart`, `OnUpdate`, `OnResume`, `OnPause`, `OnResize`, `OnStop`, `OnDestroy`.
- Per-window ergonomics: title, size, min/max constraints, position, DPI, fullscreen/maximize/minimize/restore, background color.
- Input wrappers: keyboard (`GetKeyPressed`, `IsKeyDown/Pressed/Released/Repeat`, modifiers) and mouse (`IsMouseButton*`, `MouseGetPosition`).
- Context store: `WindowContext` provides `Set`, `Get`, and `MustGet[T]` helpers.

## Low-Level API Modernization

- Removed `Vector2`.
- `GetMousePosition()` now returns `(int, int)`.

Migration example:
```go
// Before:
p := winui.GetMousePosition()
fmt.Printf("Mouse at (%d,%d)\n", p.X, p.Y)

// After:
x, y := winui.GetMousePosition()
fmt.Printf("Mouse at (%d,%d)\n", x, y)
```

## Reference: Per-Window Methods

- Core: `InitWindowHandler()`, `(*Window).Run(ctx)`, `(*Window).Handle()`, `(*Window).Context()`
- Config: `SetTitle`, `SetBackgroundColor`, `SetSize`, `SetMinSize`, `SetMaxSize`, `SetMinWidth`, `SetMinHeight`, `SetMaxWidth`, `SetMaxHeight`
- Size: `Size()`, `ClientSize()`, `OuterSize()`
- Position/DPI/state: `GetPosition()`, `SetPosition()`, `DPIScale()`, `IsFullscreen()`, `ToggleFullscreen()`, `MaximizeWindow()`, `MinimizeWindow()`, `RestoreWindow()`
- Input (keyboard): `GetKeyPressed()`, `GetCharPressed()`, `IsKeyDown()`, `IsKeyPressed()`, `IsKeyReleased()`, `IsKeyPressedRepeat()`, `GetModifiers()`, `IsShiftDown()`, `IsControlDown()`, `IsAltDown()`
- Input (mouse): `IsMouseButtonDown()`, `IsMouseButtonUp()`, `IsMouseButtonPressed()`, `IsMouseButtonReleased()`, `MouseGetPosition()`, `MouseGetX()`, `MouseGetY()`

## Notes

- Input helpers work out-of-the-box; no registration needed.
- `OnUpdate` runs after event/input polling; input reflects the current frame.
- Low-level helpers remain available alongside the high-level `Window` API.

