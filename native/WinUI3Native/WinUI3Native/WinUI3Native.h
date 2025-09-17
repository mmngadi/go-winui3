#pragma once

#include <MddBootstrap.h>
#pragma comment(lib, "Microsoft.WindowsAppRuntime.Bootstrap.lib")

#include <windows.h>
#include <winrt/Microsoft.UI.Xaml.Controls.h>
#include <winrt/Microsoft.UI.Xaml.Hosting.h>
#include <winrt/Microsoft.UI.Windowing.h>
#include <winrt/Microsoft.UI.Xaml.Media.h>

#ifdef WINUI3NATIVE_EXPORTS
#define WINUI3NATIVE_API __declspec(dllexport)
#else
#define WINUI3NATIVE_API __declspec(dllimport)
#endif

// A handle to a UI control.
typedef void* ControlHandle;

extern "C" {
    // Core Lifecycle Functions
    WINUI3NATIVE_API HRESULT __stdcall InitUI();
    WINUI3NATIVE_API void __stdcall ShutdownUI();

    // Window and Content Functions
    // Creates (or schedules) the main window with an initial client size (width,height)
    // and title. Width/height <=0 can fall back to defaults decided by the native layer.
    WINUI3NATIVE_API ControlHandle __stdcall create_window(int width, int height, const wchar_t* title);
    // (Removed: create_container / control creation / modifier / diagnostics exports per request)
    WINUI3NATIVE_API ControlHandle __stdcall get_main_window();
    WINUI3NATIVE_API int __stdcall window_exists();

    // Window metadata & control
    WINUI3NATIVE_API void __stdcall set_window_title(const wchar_t* title);
    WINUI3NATIVE_API void __stdcall get_window_size(double* width, double* height);
    // Resize callback now passes raw IEEE-754 bit patterns as uint64_t to avoid
    // calling convention issues with variadic / mixed float args across the
    // syscall.NewCallback boundary in Go. Caller reinterprets back to float64.
    typedef void(__stdcall* resize_callback_t)(uint64_t widthBits, uint64_t heightBits);
    WINUI3NATIVE_API void __stdcall register_resize_callback(resize_callback_t cb);
    // Set main window (root content) background color using ARGB 8-bit components.
    WINUI3NATIVE_API void __stdcall set_window_background_color(unsigned char a, unsigned char r, unsigned char g, unsigned char b);

    // Input event callback: kind:1=key 2=mouse. action:1=down 2=up 3=char.
    // For keys: code = virtual-key, mods = bitmask (1=Shift 2=Ctrl 4=Alt 8=Win).
    // For mouse: code = button (1=L 2=R 3=M 4=X1 5=X2), x,y in client coords.
    // input_event_callback_t packed parameters:
    // kind: 1=key 2=mouse
    // codeWithMods: low 16 bits = virtual key or mouse button id; high 16 bits = mods bitmask
    // action: 1=down/press 2=up/release (keys & mouse)
    // packedXY: lower 32 bits = x, upper 32 bits = y (client coordinates). For key events x=y=0.
    typedef void(__stdcall* input_event_callback_t)(int kind, int codeWithMods, int action, unsigned long long packedXY);
    WINUI3NATIVE_API void __stdcall register_input_callback(input_event_callback_t cb);

    // Close callback invoked immediately when the main window Closed event fires
    // (before the unified polled closed event is enqueued / before Application::Exit()).
    typedef void(__stdcall* close_callback_t)();
    WINUI3NATIVE_API void __stdcall register_close_callback(close_callback_t cb);

    // Set min/max client size hints (in client area pixels). Pass 0 to unset.
    // These are enforced via WM_GETMINMAXINFO by adjusting to outer window size.
    WINUI3NATIVE_API void __stdcall set_window_min_max(int minW, int minH, int maxW, int maxH);

    // Overlay / HUD utilities
    // Sets (or creates) a centered overlay TextBlock showing provided text.
    // Passing an empty string hides it.
    // (Removed: set_center_overlay_text per request)

    // Unified event system (polled from Go side)
    // kind:1=key 2=mouse 3=resize 4=window_closed 5=window_created
    // key: code=vk action:1=down 2=up mods=bitmask (side specific)
    // mouse: code=button(1..5) action:1=down 2=up x,y client coords mods=bitmask
    // resize: w,h populated (action/code unused)
    // window_closed/window_created: no extra fields
    typedef struct WinUIEvent {
        int   kind;
        int   code;
        int   action;
        int   mods;
        int   x;
        int   y;
        double w;
        double h;
    } WinUIEvent;

    // Poll up to max events into outEvents. Returns number copied.
    // If *more is set to 1 after return, additional events remain.
    WINUI3NATIVE_API int __stdcall winui_poll_events(WinUIEvent* outEvents, int max, int* more);
}