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
	w.SetTitle("Hello from Go-WinUI3")
	w.SetSize(1024, 768)
	w.SetMinWidth(640)
	w.SetMinHeight(480)

	w.OnCreate(func(_ *winui.Window, _ *winui.WindowContext) {
		w.SetBackgroundColor(winui.NewColor(255, 245, 245, 245))
		sx, sy := w.DPIScale()
		fmt.Printf("DPI scale: %.2fx, %.2fy\n", sx, sy)
	})

	w.OnResize(func(_ *winui.Window, _ *winui.WindowContext, width, height int) {
		fmt.Printf("[resize] %dx%d\n", width, height)
	})

	w.OnUpdate(func(_ *winui.Window, _ *winui.WindowContext) {
		for k := w.GetKeyPressed(); k != 0; k = w.GetKeyPressed() {
			if k == 0x7A { // F11
				w.ToggleFullscreen()
			}
		}
		if w.IsMouseButtonPressed(winui.MouseButtonLeft) {
			x, y := w.MouseGetPosition()
			fmt.Printf("Left click at (%d, %d)\n", x, y)
		}
	})

	w.Run(context.Background())
}
