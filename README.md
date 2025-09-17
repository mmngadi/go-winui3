# go-winui3 (Go + WinUI 3 via C++/WinRT)

> Status: Experimental — heavy development in progress. APIs are unstable and may change without notice. Do not use in production. Commits can break builds/behavior; no guarantees or warranties are provided.

go-winui3 lets you build native Windows apps in Go using WinUI 3 without cgo. It ships a small C++/WinRT bridge DLL that hosts the WinUI 3 UI thread, and a pure Go wrapper that dynamically calls the DLL exports via `golang.org/x/sys/windows`.

The goal is fast iteration with a single script that builds the native DLL and any Go example you choose, producing a runnable app under `bin/<arch>/<config>`.

**Who it’s for**
- Go developers targeting Windows who want a native WinUI 3 UI without cgo.
- Teams that prefer shipping a small DLL + Go binary and keeping Go builds simple.

**How it works**
- The native project (`WinUI3Native.dll`) runs WinUI 3 on its own UI thread and exposes a compact API (window lifecycle, input, sizing, etc.).
- The Go wrapper (`internal/winui`) loads the DLL at runtime and calls its exports. Input/events are delivered via callbacks and a polled queue.
- Window constraints (min/max) are enforced in native via `WM_GETMINMAXINFO`; DPI and window operations use Win32 APIs.

**Scope & Platform**
- Windows-only: targets Windows 10/11.
- Primary focus is x64 builds; other architectures may be added later.
- Not cross-platform; there are no plans to support non-Windows UI backends.

**Motivation**
- This project started as a way to make creating Windows applications easier for personal use.
- It uses Go because the author is learning the language and wanted a fun way to build real apps.
- The idea is to quickly build utility apps without needing to learn C++ or WinRT details — the native DLL handles that complexity while Go stays simple.

## Direction & Roadmap

- Current focus: building out low-level wrappers for windowing, input, sizing, DPI, and other primitives. Expect frequent changes while APIs and ergonomics are refined.
- End goal: a Go-native declarative UI DSL inspired by Jetpack Compose and SwiftUI, enabling Go developers to build rich Windows desktop UIs entirely in Go.
	- Compose-like composable functions and state-driven rendering
	- SwiftUI-like declarative syntax for layout, styling, and interaction
	- Minimal exposure to C++/WinRT details — stay in Go
- The DSL will layer on top of these low-level wrappers and the native bridge, evolving towards higher-level components and layouts.

## Repository Layout

```
go-winui3/
	examples/yourgoapp/     # Example application (main.go)
	internal/winui/         # Go dynamic wrapper (syscall, no cgo)
	native/WinUI3Native/    # C++/WinRT project producing WinUI3Native.dll
	bin/<arch>/<config>/    # Unified output (DLLs + built example EXE)
	build.ps1               # Orchestrated build script
```

## Features (Current)

- Window: create; get/set position and size; outer vs client size; DPI scale; focus; minimize/maximize/restore; toggle borderless fullscreen.
- Sizing constraints: set min/max client size with correct non-client frame adjustments.
- Input: key/mouse helpers just work — no registration required (pressed/released/repeat, modifiers, Unicode chars).
- Resize: simple `OnResize` helper (default debounce) and `IsWindowResized()` flag for per-frame checks.
- Opacity: layered window alpha.
- Pure Go wrapper: no cgo; loads the DLL dynamically.

## Prerequisites

- Windows 10/11
- Go 1.21+ (recommended)
- Visual Studio Build Tools or MSBuild (for native build)
- PowerShell 5+ or PowerShell 7+

## Build & Run Examples

The build script now requires you to specify which example to build using `-ExamplePath`. This can be a directory containing `main.go` or a direct path to a `main.go` file.

- Full Build:
	`powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Debug -Platform x64 -Verbose -ExamplePath examples/yourgoapp`
- Run Build:
	`./bin/x64/Debug/debug.exe`

Other examples:
- Directory: `-ExamplePath examples/yourgoapp`
- Direct file: `-ExamplePath examples/yourgoapp/main.go`
- Release build: `-Configuration Release` (produces `./bin/x64/Release/release.exe`)

Notes:
- The Go executable loads `WinUI3Native.dll` from `bin/<arch>/<config>`. The script copies required dependency DLLs there.
- If you run your app outside `bin/`, call `winui.Load("path/to/bin/x64/Debug")` before `winui.Init()` or add that folder to `PATH`.

## Minimal Example (main.go)

Create a simple window with a background color of ARGB(255,245,245,245) and a title "Hello from Go-WinUI3".

```go
package main

import (
	"fmt"
	"log"
	"runtime"

	winui "github.com/mmngadi/go-winui3/internal/winui"
)

func main() {
	runtime.LockOSThread()

	// Initialize and create a window (loads DLL, waits until ready)
	if _, err := winui.InitWindow(1024, 768, "Hello from Go-WinUI3"); err != nil {
		log.Fatalf("init window: %v", err)
	}

	// Set background color ARGB(255,245,245,245)
	winui.SetWindowBackgroundColor(winui.NewColor(255, 245, 245, 245))

	// Simple debounced resize handler (default 200ms)
	winui.OnResize(func(w, h int) { fmt.Printf("[resize] %dx%d\n", w, h) })

	// Target 60 FPS pacing for the built-in loop
	winui.SetTargetFPS(60)

	// Raylib-style loop: winui.Run handles polling + pacing
	winui.Run(func() bool {
		// Keyboard
		for k := winui.GetKeyPressed(); k != 0; k = winui.GetKeyPressed() {
			fmt.Printf("[key] %d\n", k)
		}
		// Text input (Unicode)
		for ch := winui.GetCharPressed(); ch != 0; ch = winui.GetCharPressed() {
			fmt.Printf("[char] %q\n", rune(ch))
		}
		// Mouse
		if winui.IsMouseButtonPressed(winui.MouseButtonLeft) {
			p := winui.GetMousePosition()
			fmt.Printf("[mouse] left @ (%d,%d)\n", p.X, p.Y)
		}
		return true
	})
}
```

Build it inside this repo with the provided script by pointing `-ExamplePath` at your folder containing `main.go`:

```
powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Debug -Platform x64 -Verbose -ExamplePath path\to\your\example
./bin/x64/Debug/debug.exe
```

## Example App Controls

From `examples/yourgoapp` at runtime:
- F11: Toggle borderless fullscreen
- F6: Print DPI scale, window position, size, client size, and outer size
- F7: Set min client size to 800x600 (enforced)
- F8: Clear size limits

## High‑Level API (Go)

- Window lifecycle: `InitWindow(w, h, title)`, `Run(update)`, `WindowShouldClose()`
- Rendering/loop pacing: `SetTargetFPS(fps)`, `GetFrameTime()`, `GetFPS()`
- Resize: `OnResize(func(w, h int))`, `IsWindowResized()`, `GetWindowSizeInt()`, `GetWindowClientSize()`
- Window ops: `ToggleFullscreen()`, `IsWindowFullscreen()`, `GetWindowPosition()`, `SetWindowPosition(x,y)`, `GetWindowOuterSize()`
- Constraints: `SetWindowMinSize(w,h)`, `SetWindowMaxSize(w,h)`, `GetWindowMinSize()`, `GetWindowMaxSize()`
- DPI: `GetWindowScaleDPI()`
- Input (keyboard): `GetKeyPressed()`, `GetCharPressed()`, `IsKeyDown/Pressed/Released/Repeat(key)`
- Input (mouse): `IsMouseButtonPressed/Released(btn)`, `GetMousePosition()`
- Appearance: `SetWindowBackgroundColor(winui.Color)`

Notes
- Input helpers work out-of-the-box; no handler registration needed.
- `OnResize` uses a default debounce (`DefaultResizeDebounce = 200ms`). Use `OnResizeImmediate` for per-event callbacks.
- Advanced/low-level callbacks and full event polling exist but are intentionally omitted here to keep the surface simple. See source if needed.
<!-- Advanced APIs intentionally omitted here to keep the surface simple. Consult the source if you need lower-level control (callbacks, polling, visibility/show/hide, opacity, etc.). -->
Types and constants
- `type Handle uintptr`, `type Event struct{ ... }`, `type Vector2 struct{ X, Y int }`, `type Color uint32`.
- Event kinds: `EventKindKey`, `EventKindMouse`, `EventKindResize`, `EventKindClosed`, `EventKindCreated`.
- Actions: `ActionDown`, `ActionUp`.
- Modifiers: side-specific bits (L/R Shift/Ctrl/Alt/Win) with `Mod*` constants.
- Mouse buttons: `MouseButtonLeft`, `MouseButtonRight`, `MouseButtonMiddle`.

