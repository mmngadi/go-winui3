package main

import (
	"context"
	"fmt"
	"log"
	"runtime"

	winui "github.com/mmngadi/go-winui3/internal/winui"
)

func main() {
	runtime.LockOSThread()

	w := winui.InitWindowHandler()
	w.SetTitle("Container Demo")
	w.SetSize(600, 400)
	w.SetMinWidth(400)
	w.SetMinHeight(300)

	w.OnCreate(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo window created!")
		win.SetBackgroundColor(winui.NewColor(255, 248, 248, 255))
		sx, sy := win.DPIScale()
		fmt.Printf("DPI scale: %.2fx, %.2fy\n", sx, sy)
	})

	w.SetContent(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Setting up container layout...")

		// Create a StackPanel container
		stackPanel := winui.CreateStackPanel()
		if stackPanel == 0 {
			log.Fatal("Failed to create StackPanel")
		}

		// Create a Grid container
		grid := winui.CreateGrid()
		if grid == 0 {
			log.Fatal("Failed to create Grid")
		}

		// Create text inputs with proper parent containers
		textInput1 := winui.CreateTextInput(stackPanel, "Hello from Text Input 1")
		textInput2 := winui.CreateTextInput(stackPanel, "Hello from Text Input 2")
		textInput3 := winui.CreateTextInput(grid, "Hello from Text Input 3")

		// Add both containers to the StackPanel (nested layout)
		winui.AddChild(stackPanel, grid)

		// Store container handles in context for later use
		ctx.Set("stackPanel", stackPanel)
		ctx.Set("grid", grid)
		ctx.Set("textInputs", []winui.Handle{textInput1, textInput2, textInput3})

		log.Println("Container demo layout created successfully!")
		log.Println("StackPanel handle:", stackPanel)
		log.Println("Grid handle:", grid)
		log.Println("TextInput handles:", textInput1, textInput2, textInput3)
	})

	w.OnStart(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo started")
	})

	w.OnUpdate(func(win *winui.Window, ctx *winui.WindowContext) {
		// Handle input events
		for k := win.GetKeyPressed(); k != 0; k = win.GetKeyPressed() {
			if k == 0x1B { // ESC key
				log.Println("ESC pressed, closing window...")
				winui.BeginShutdownAsync()
			}
			if k == 0x7A { // F11
				win.ToggleFullscreen()
			}
		}

		// Handle mouse clicks
		if win.IsMouseButtonPressed(winui.MouseButtonLeft) {
			x, y := win.MouseGetPosition()
			fmt.Printf("Left click at (%d, %d)\n", x, y)
		}
	})

	w.OnResize(func(win *winui.Window, ctx *winui.WindowContext, width, height int) {
		fmt.Printf("Container demo window resized to: %dx%d\n", width, height)
	})

	w.OnResume(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo resumed (gained focus)")
	})

	w.OnPause(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo paused (lost focus)")
	})

	w.OnStop(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo stopping...")
	})

	w.OnDestroy(func(win *winui.Window, ctx *winui.WindowContext) {
		log.Println("Container demo destroyed")
		// Note: We intentionally skip ReleaseControl here.
		// Native ShutdownUI clears window content and g_controls safely on the UI thread,
		// so explicit release at this late stage can trigger debug breakpoints in dependencies.
		// For this demo, rely on native teardown to release the XAML tree.
	})

	log.Println("Starting container demo with lifecycle functions...")
	log.Println("Press ESC to exit, F11 to toggle fullscreen")
	w.Run(context.Background())
}
