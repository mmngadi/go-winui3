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
	w.SetTitle("Hello from golang!")
	w.SetSize(1024, 768)

	w.OnCreate(func(win *winui.Window, _ *winui.WindowContext) {
		win.SetBackgroundColor(winui.NewColor(255, 255, 203, 0))
		fmt.Println("Hints: F11 toggle fullscreen; F6 print info; F7 min 800x600; F8 clear limits")
	})

	w.OnResize(func(_ *winui.Window, _ *winui.WindowContext, width, height int) {
		fmt.Printf("[resize] %dx%d\n", width, height)
	})

	w.OnUpdate(func(win *winui.Window, _ *winui.WindowContext) {
		// Keys + hotkeys
		for k := win.GetKeyPressed(); k != 0; k = win.GetKeyPressed() {
			switch k {
			case 0x7A: // F11
				win.ToggleFullscreen()
				fmt.Printf("[window] fullscreen=%v\n", win.IsFullscreen())
			case 0x75: // F6
				sx, sy := win.DPIScale()
				x, y := win.GetPosition()
				w, h := win.Size()
				cw, ch := win.ClientSize()
				ow, oh := win.OuterSize()
				fmt.Printf("[window] DPI=(%.2f,%.2f) pos=(%d,%d) size=%dx%d client=%dx%d outer=%dx%d\n",
					sx, sy, x, y, w, h, cw, ch, ow, oh)
			case 0x76: // F7
				win.SetMinSize(800, 600)
				fmt.Println("[window] min size set to 800x600")
			case 0x77: // F8
				win.SetMinSize(0, 0)
				win.SetMaxSize(0, 0)
				fmt.Println("[window] cleared min/max size limits")
			}
		}

		// Text input (Unicode)
		var runes []rune
		for ch := win.GetCharPressed(); ch != 0; ch = win.GetCharPressed() {
			runes = append(runes, rune(ch))
		}
		if len(runes) > 0 {
			fmt.Printf("[char] \"%s\"\n", string(runes))
		}

		// Mouse edges
		x, y := win.MouseGetPosition()
		if win.IsMouseButtonPressed(winui.MouseButtonLeft) {
			fmt.Printf("[mouse] left pressed at (%d,%d)\n", x, y)
		}
		if win.IsMouseButtonReleased(winui.MouseButtonLeft) {
			fmt.Printf("[mouse] left released at (%d,%d)\n", x, y)
		}
		if win.IsMouseButtonPressed(winui.MouseButtonRight) {
			fmt.Printf("[mouse] right pressed at (%d,%d)\n", x, y)
		}
		if win.IsMouseButtonReleased(winui.MouseButtonRight) {
			fmt.Printf("[mouse] right released at (%d,%d)\n", x, y)
		}
	})

	w.Run(context.Background())
}
