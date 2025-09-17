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
