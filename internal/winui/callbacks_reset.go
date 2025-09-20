package winui

import (
	"sync/atomic"
)

// ResetInputCallbacks clears all registered input callback handlers.
// This is used internally during shutdown to prevent callbacks after the native
// layer is being torn down.
func ResetInputCallbacks() {
	if pRegisterInputCallback != nil && inputCallbackPtr != 0 {
		// Set a null callback to prevent further callbacks
		pRegisterInputCallback.Call(0)
	}

	inputHandlerMu.Lock()
	inputHandler = nil
	inputHandlerMu.Unlock()

	// Also clear the stored input states
	keyStateMu.Lock()
	keyDown = make(map[int]bool)
	keyRepeat = make(map[int]bool)
	keyPressedOnce = make(map[int]bool)
	keyReleasedOnce = make(map[int]bool)
	keyPressQueue = keyPressQueue[:0]
	charPressQueue = charPressQueue[:0]
	currentMods = 0
	keyStateMu.Unlock()

	mouseStateMu.Lock()
	mouseDown = make(map[int]bool)
	mousePressedOnce = make(map[int]bool)
	mouseReleasedOnce = make(map[int]bool)
	mouseX, mouseY = 0, 0
	mouseStateMu.Unlock()
}

// ResetResizeCallback clears the registered resize callback handler.
// This is used internally during shutdown to prevent callbacks after the native
// layer is being torn down.
func ResetResizeCallback() {
	if pRegisterResizeCallback != nil && resizeCallbackPtr != 0 {
		// Set a null callback to prevent further callbacks
		pRegisterResizeCallback.Call(0)
	}

	resizeHandlerMu.Lock()
	resizeHandler = nil
	resizeHandlerMu.Unlock()

	// Reset the window resized flag
	atomic.StoreUint32(&windowResizedFlag, 0)
}
