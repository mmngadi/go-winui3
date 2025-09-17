package main

import (
	"fmt"
	"log"
	"runtime"
	"time"

	winui "github.com/mmngadi/go-winui3/internal/winui"
)

func main() {
	runtime.LockOSThread()
	if _, err := winui.InitWindow(1024, 768, "Hello from golang!"); err != nil {
		log.Fatalf("init window: %v", err)
	}

	// Simple debounced resize handler using default interval
	winui.OnResize(func(w, h int) { fmt.Printf("[resize] %dx%d\n", w, h) })
	winui.SetWindowBackgroundColor(winui.NewColor(255, 255, 203, 0))
	winui.SetTargetFPS(60)

	fmt.Println("UI initialized; event loop running (close window or Alt+F4)")
	fmt.Println("Hints: F11 toggles fullscreen; F6 prints DPI+pos+size; F7 set min 800x600; F8 clear limits")

	var lastStatus time.Time
	winui.Run(func() bool {
		if time.Since(lastStatus) >= time.Second {
			printStatus()
			lastStatus = time.Now()
		}

		drainKeysAndHotkeys()
		drainChars()
		logMouseEdges()
		return true
	})
}

func printStatus() {
	rs := winui.GetRuntimeState()
	fmt.Printf("[state] ready=%v shutdown=%v controls=%d focused=%v fps=%d dt=%.3f\n",
		rs.WindowReady, rs.ShutdownRequested, rs.ControlsCount, winui.IsWindowFocused(), winui.GetFPS(), winui.GetFrameTime())
}

func drainKeysAndHotkeys() {
	for {
		k := winui.GetKeyPressed()
		if k == 0 {
			break
		}
		fmt.Printf("[key] pressed vk=%d repeat=%v mods=0x%X\n", k, winui.IsKeyPressedRepeat(k), winui.GetModifiers())
		handleHotkey(k)
	}
}

func handleHotkey(k int) {
	switch k {
	case 0x7A: // VK_F11
		winui.ToggleFullscreen()
		fmt.Printf("[window] toggled fullscreen: now fullscreen=%v\n", winui.IsWindowFullscreen())
	case 0x75: // VK_F6
		printWindowInfo()
	case 0x76: // VK_F7
		winui.SetWindowMinSize(800, 600)
		mw, mh := winui.GetWindowMinSize()
		fmt.Printf("[window] set min size: %dx%d\n", mw, mh)
	case 0x77: // VK_F8
		winui.SetWindowMinSize(0, 0)
		winui.SetWindowMaxSize(0, 0)
		fmt.Println("[window] cleared min/max size limits")
	}
}

func printWindowInfo() {
	sx, sy := winui.GetWindowScaleDPI()
	x, y := winui.GetWindowPosition()
	w, h := winui.GetWindowSizeInt()
	cw, ch := winui.GetWindowClientSize()
	ow, oh := winui.GetWindowOuterSize()
	fmt.Printf("[window] DPI scale=(%.2f,%.2f) pos=(%d,%d) size=%dx%d client=%dx%d outer=%dx%d\n",
		sx, sy, x, y, w, h, cw, ch, ow, oh)
}

func drainChars() {
	var runes []rune
	for {
		ch := winui.GetCharPressed()
		if ch == 0 {
			break
		}
		runes = append(runes, rune(ch))
	}
	if len(runes) > 0 {
		fmt.Printf("[char] \"%s\"\n", string(runes))
	}
}

func logMouseEdges() {
	p := winui.GetMousePosition()
	if winui.IsMouseButtonPressed(winui.MouseButtonLeft) {
		fmt.Printf("[mouse] left pressed at (%d,%d)\n", p.X, p.Y)
	}
	if winui.IsMouseButtonReleased(winui.MouseButtonLeft) {
		fmt.Printf("[mouse] left released at (%d,%d)\n", p.X, p.Y)
	}
	if winui.IsMouseButtonPressed(winui.MouseButtonRight) {
		fmt.Printf("[mouse] right pressed at (%d,%d)\n", p.X, p.Y)
	}
	if winui.IsMouseButtonReleased(winui.MouseButtonRight) {
		fmt.Printf("[mouse] right released at (%d,%d)\n", p.X, p.Y)
	}
}
