// Simplified, structured version of original implementation.
// Public exported API remains unchanged.

#include "pch.h"
#include "WinUI3Native.h"
#include <map>
#include <thread>
#include <mutex>
#include <condition_variable>
#include <future>
#include <atomic>
#include <winrt/Microsoft.UI.Xaml.h>
#include <winrt/Microsoft.UI.Xaml.Controls.h>
#include <winrt/Microsoft.UI.Windowing.h>
#include <winrt/Microsoft.UI.Xaml.Input.h>
#include <winrt/Microsoft.UI.Input.h>
#include <winrt/Microsoft.UI.Dispatching.h>
#include <winrt/Windows.UI.Text.h>
#include <MddBootstrap.h>
#include <Windows.h>
#include <psapi.h>
#pragma comment(lib, "Psapi.lib")
#include <dbghelp.h>
#pragma comment(lib, "Dbghelp.lib")

// Needed for IWindowNative to extract HWND from Microsoft::UI::Xaml::Window
#include <microsoft.ui.xaml.window.h>

// Forward declare bootstrap shutdown deferral
static void DeferredBootstrapShutdown();
static bool g_bootstrapShutdownRegistered = false; // now only active if explicitly enabled via env var

// State / diagnostics -------------------------------------------------------
static std::wstring g_lastErrorMessage;
static HRESULT g_lastHRESULT = S_OK;
static uint32_t g_bootstrapVersion = 0;
static std::atomic<int> g_windowCreateAttempts{ 0 };
static constexpr int kMaxWindowCreateAttempts = 8;
static std::atomic<int> g_shutdownSeq{0};
static PVOID g_vectoredHandler = nullptr;
// Pending window title captured from create_window calls before the window actually exists.
static std::mutex g_windowTitleMutex;
static std::wstring g_pendingWindowTitle; // empty => use default
// Pending background color to apply once window exists if set earlier.
static std::atomic<bool> g_pendingBGSet{ false };
static std::atomic<unsigned int> g_pendingBGARGB{ 0 }; // 0xAARRGGBB
// Pending initial size (client area) requested before the window actually exists.
// 0/negative means ignore. Applied once at creation; subsequent create_window calls
// with an existing window will resize immediately.
static std::atomic<int> g_pendingInitialWidth{ 0 };
static std::atomic<int> g_pendingInitialHeight{ 0 };

static void LogSeq(const wchar_t* msg) {
    int n = ++g_shutdownSeq;
    wchar_t buf[256];
    _snwprintf_s(buf, _TRUNCATE, L"[ShutdownSeq %d] %ls\n", n, msg ? msg : L"" );
    OutputDebugStringW(buf);
}

// Vectored exception handler to log access violation site (module+offset) for post-mortem triage.
static LONG CALLBACK CrashDiagVectoredHandler(EXCEPTION_POINTERS* info) {
    if (!info || !info->ExceptionRecord) return EXCEPTION_CONTINUE_SEARCH;
    auto rec = info->ExceptionRecord;
    if (rec->ExceptionCode == EXCEPTION_ACCESS_VIOLATION) {
        static std::atomic<bool> g_symInit{ false };
        // Lazy initialize symbols (only once, ignore failures).
        if (!g_symInit.load(std::memory_order_acquire)) {
            wchar_t disableBuf[8];
            if (GetEnvironmentVariableW(L"WINUI_DISABLE_SYMBOLS", disableBuf, _countof(disableBuf)) == 0) {
                HANDLE proc = GetCurrentProcess();
                SymSetOptions(SYMOPT_DEFERRED_LOADS | SYMOPT_UNDNAME | SYMOPT_LOAD_LINES);
                if (SymInitialize(proc, nullptr, TRUE)) {
                    OutputDebugStringW(L"[CrashDiag] Symbols initialized\n");
                } else {
                    OutputDebugStringW(L"[CrashDiag] SymInitialize failed\n");
                }
            } else {
                OutputDebugStringW(L"[CrashDiag] Symbol loading disabled by env\n");
            }
            g_symInit.store(true, std::memory_order_release);
        }

        void* pc = rec->ExceptionAddress;
        HMODULE hm = nullptr;
        char modPath[MAX_PATH] = { 0 };
        if (GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
            (LPCSTR)pc, &hm)) {
            GetModuleFileNameA(hm, modPath, MAX_PATH);
        } else {
            strcpy_s(modPath, "<unknown>");
        }
        uintptr_t base = reinterpret_cast<uintptr_t>(hm);
        uintptr_t addr = reinterpret_cast<uintptr_t>(pc);
        size_t offset = base ? (addr - base) : 0;
        const char* mode = (rec->NumberParameters >= 1) ? (rec->ExceptionInformation[0] ? "WRITE" : "READ") : "?";
        void* fault = (rec->NumberParameters >= 2) ? reinterpret_cast<void*>(rec->ExceptionInformation[1]) : nullptr;
        char buf[512];
        _snprintf_s(buf, _TRUNCATE, "[CrashDiag] AV pc=%p %s+0x%zx %s addr=%p\n", pc, modPath, offset, mode, fault);
        OutputDebugStringA(buf);

    // Stack capture (shallow – sufficient for post-mortem correlation).
        PVOID frames[24];
        USHORT captured = RtlCaptureStackBackTrace(0, 24, frames, nullptr);
        HANDLE proc = GetCurrentProcess();
        for (USHORT i = 0; i < captured; ++i) {
            void* f = frames[i];
            HMODULE mh = nullptr;
            char fmod[MAX_PATH] = { 0 };
            if (GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, (LPCSTR)f, &mh)) {
                GetModuleFileNameA(mh, fmod, MAX_PATH);
            } else {
                strcpy_s(fmod, "<unknown>");
            }
            uintptr_t fbase = reinterpret_cast<uintptr_t>(mh);
            uintptr_t faddr = reinterpret_cast<uintptr_t>(f);
            size_t fOffset = fbase ? (faddr - fbase) : 0;

            // Attempt symbol + line resolution.
            char symLine[1024];
            symLine[0] = '\0';
            bool haveSym = false;
            if (g_symInit.load(std::memory_order_acquire)) {
                unsigned char symBuffer[sizeof(SYMBOL_INFO) + 512];
                PSYMBOL_INFO pSym = reinterpret_cast<PSYMBOL_INFO>(symBuffer);
                pSym->SizeOfStruct = sizeof(SYMBOL_INFO);
                pSym->MaxNameLen = 512 - 1;
                DWORD64 displacement = 0;
                if (SymFromAddr(proc, (DWORD64)faddr, &displacement, pSym)) {
                    // Line info
                    IMAGEHLP_LINEW64 lineInfo; memset(&lineInfo, 0, sizeof(lineInfo)); lineInfo.SizeOfStruct = sizeof(lineInfo);
                    DWORD lineDisp = 0;
                    if (SymGetLineFromAddrW64(proc, (DWORD64)faddr, &lineDisp, &lineInfo) && lineInfo.FileName) {
                        // Convert wide filename to UTF-8 (best effort)
                        char fileUtf8[260]; fileUtf8[0] = '\0';
                        int conv = WideCharToMultiByte(CP_UTF8, 0, lineInfo.FileName, -1, fileUtf8, (int)sizeof(fileUtf8), nullptr, nullptr);
                        if (conv == 0) strcpy_s(fileUtf8, "<conv-fail>");
                        _snprintf_s(symLine, _TRUNCATE, " %s+0x%llx (%s:%lu)", pSym->Name, (unsigned long long)displacement, fileUtf8, lineInfo.LineNumber);
                    } else {
                        _snprintf_s(symLine, _TRUNCATE, " %s+0x%llx", pSym->Name, (unsigned long long)displacement);
                    }
                    haveSym = true;
                }
            }

            char line[1200];
            if (haveSym) {
                _snprintf_s(line, _TRUNCATE, "[CrashDiag]  frame[%u] %p %s+0x%zx%s\n", (unsigned)i, f, fmod, fOffset, symLine);
            } else {
                _snprintf_s(line, _TRUNCATE, "[CrashDiag]  frame[%u] %p %s+0x%zx\n", (unsigned)i, f, fmod, fOffset);
            }
            OutputDebugStringA(line);
        }
    }
    return EXCEPTION_CONTINUE_SEARCH; // allow normal handling
}

static void SetLastErrorInfo(HRESULT hr, const wchar_t* msg) {
    g_lastHRESULT = hr;
    try { g_lastErrorMessage = msg ? msg : L""; }
    catch (...) { g_lastErrorMessage = L"<error storing message>"; }
}

static std::wstring ArchString() {
#if defined(_M_X64)
    return L"x64";
#elif defined(_M_ARM64)
    return L"arm64";
#elif defined(_M_IX86)
    return L"x86";
#else
    return L"unknown-arch";
#endif
}

static void LogHRESULT(const wchar_t* prefix, HRESULT hr) {
    wchar_t buf[256];
    _snwprintf_s(buf, _TRUNCATE, L"%ls hr=0x%08X", prefix, static_cast<unsigned>(hr));
    SetLastErrorInfo(hr, buf);
}

static const uint32_t kBootstrapCandidates[] = {
    (1u<<16)|8u,(1u<<16)|7u,(1u<<16)|6u,(1u<<16)|5u
};

static void LogBootstrapAttempt(uint32_t v, HRESULT hr) {
    wchar_t buf[256];
    _snwprintf_s(buf, _TRUNCATE,
        L"[Bootstrap] try %u.%u arch=%s hr=0x%08X\n",
        v >> 16, v & 0xFFFF, ArchString().c_str(),
        static_cast<unsigned>(hr));
    OutputDebugStringW(buf);
    if (SUCCEEDED(hr)) {
        wchar_t ok[128];
        _snwprintf_s(ok, _TRUNCATE, L"Bootstrap success %u.%u (%s)",
            v >> 16, v & 0xFFFF, ArchString().c_str());
        SetLastErrorInfo(S_OK, ok);
    }
}

static HRESULT TryBootstrapMulti() {
    HRESULT lastHr = E_FAIL;
    PACKAGE_VERSION minVersion{}; // zero-initialized minimum version
    for (auto v : kBootstrapCandidates) {
        HRESULT hr = MddBootstrapInitialize(v, nullptr, minVersion);
        LogBootstrapAttempt(v, hr);
        if (SUCCEEDED(hr)) {
            g_bootstrapVersion = v;
            return S_OK;
        }
        lastHr = hr;
    }
    return lastHr;
}

using namespace winrt;
using namespace Microsoft::UI::Xaml;
using namespace Microsoft::UI::Xaml::Controls;
using namespace Microsoft::UI::Windowing;

// UI objects / callbacks ----------------------------------------------------
std::map<ControlHandle, FrameworkElement> g_controls;
Window g_window{ nullptr };
// Helper to retrieve HWND for the current WinUI Window (after g_window defined)
static HWND GetWindowHandle() {
    try {
        if (!g_window) return nullptr;
        auto native = g_window.as<IWindowNative>();
        HWND hwnd{};
        if (native) native->get_WindowHandle(&hwnd);
        return hwnd;
    } catch (...) { return nullptr; }
}
static Microsoft::UI::Xaml::Controls::Grid g_overlayRoot{ nullptr };
static Microsoft::UI::Xaml::Controls::TextBlock g_overlayText{ nullptr };
static Microsoft::UI::Xaml::FrameworkElement g_originalRootFE{ nullptr };
static resize_callback_t g_resizeCallback = nullptr;
static input_event_callback_t g_inputCallback = nullptr;
static close_callback_t g_closeCallback = nullptr;
static int g_lastPointerButton = 0;
// Aggregate modifier bits (legacy): 1=Shift 2=Ctrl 4=Alt 8=Win
// Side-specific modifier bit mask (v2):
// 1=LShift 2=RShift 4=LCtrl 8=RCtrl 16=LAlt 32=RAlt 64=LWin 128=RWin
static int ComputeMods(){
    int m=0;
    if ((GetKeyState(VK_LSHIFT)   & 0x8000)!=0) m|=1;
    if ((GetKeyState(VK_RSHIFT)   & 0x8000)!=0) m|=2;
    if ((GetKeyState(VK_LCONTROL) & 0x8000)!=0) m|=4;
    if ((GetKeyState(VK_RCONTROL) & 0x8000)!=0) m|=8;
    if ((GetKeyState(VK_LMENU)    & 0x8000)!=0) m|=16;
    if ((GetKeyState(VK_RMENU)    & 0x8000)!=0) m|=32;
    if ((GetKeyState(VK_LWIN)     & 0x8000)!=0) m|=64;
    if ((GetKeyState(VK_RWIN)     & 0x8000)!=0) m|=128;
    return m;
}

// Unified event queue (single-producer (UI thread) / multi-consumer safe read) ------
struct WinUIEventInternal {
    int kind;  // 1=key 2=mouse 3=resize 4=window_closed 5=window_created
    int code;  // key: vk, mouse: button, resize/window: 0
    int action; // key:1=down 2=up mouse:1=down 2=up
    int mods;  // side-specific bitmask
    int x;
    int y;
    double w;  // resize width
    double h;  // resize height
};
static constexpr int kEventRingSize = 256;
static WinUIEventInternal g_eventRing[kEventRingSize];
static std::atomic<int> g_eventHead{0}; // next write
static std::atomic<int> g_eventTail{0}; // next read
static std::atomic<int> g_eventOverflow{0};
static void EnqueueEvent(const WinUIEventInternal& ev){
    int head = g_eventHead.load(std::memory_order_relaxed);
    int tail = g_eventTail.load(std::memory_order_acquire);
    int next = (head + 1) % kEventRingSize;
    if (next == tail) { // full -> drop oldest
        g_eventOverflow.fetch_add(1, std::memory_order_relaxed);
        g_eventTail.store((tail + 1) % kEventRingSize, std::memory_order_release);
    }
    g_eventRing[head] = ev;
    g_eventHead.store(next, std::memory_order_release);
}


// Threading / lifecycle
// Threading / lifecycle -----------------------------------------------------
static std::thread g_uiThread;
static bool g_appThreadStarted=false;
static bool g_appReady=false;
static DWORD g_uiThreadId=0;
static Microsoft::UI::Dispatching::DispatcherQueue g_dispatcherQueue{nullptr};
static std::mutex g_initMutex;
static std::condition_variable g_initCv;
static bool g_shutdownRequested=false;
static std::atomic<bool> g_windowCreationScheduled{false};
static bool g_windowReady=false; // window created + content attached
static std::condition_variable g_windowReadyCv;
static std::mutex g_windowReadyMutex;
static std::wstring g_unhandledExceptionMessage; // last unhandled exception message
// Min/Max client size hints (0 means unset). Enforced via WM_GETMINMAXINFO.
static std::atomic<int> g_minClientW{0};
static std::atomic<int> g_minClientH{0};
static std::atomic<int> g_maxClientW{0};
static std::atomic<int> g_maxClientH{0};
// Original window proc for subclassing
static WNDPROC g_originalWndProc = nullptr;

// Forward declarations
static void ScheduleWindowCreation(int attempt);

struct NativeApp : ApplicationT<NativeApp>
{
    void OnLaunched(LaunchActivatedEventArgs const&) {
        g_dispatcherQueue = Microsoft::UI::Dispatching::DispatcherQueue::GetForCurrentThread();
        g_uiThreadId = ::GetCurrentThreadId();

        // Attach unhandled exception handler (best-effort)
        try {
            if (auto app = Application::Current()) {
                app.UnhandledException([](auto&&, Microsoft::UI::Xaml::UnhandledExceptionEventArgs const& e){
                    try {
                        g_unhandledExceptionMessage = e.Message().c_str();
                        OutputDebugStringW((L"[UnhandledException] " + g_unhandledExceptionMessage + L"\n").c_str());
                        e.Handled(true); // prevent process crash; we still may decide to shutdown later
                    } catch(...) {}
                });
            }
        } catch(...) {}

        if (!winrt::impl::is_sta_thread()) {
            SetLastErrorInfo(E_FAIL, L"OnLaunched: not STA");
        }

        // Lightweight probe: ensure some basic control can activate
        try {
            Button probe;
            probe.Content(winrt::box_value(L"probe"));
        }
        catch (const winrt::hresult_error& e) {
            std::wstring msg = L"Probe Button failed hr=0x";
            wchar_t hex[9]; swprintf_s(hex, L"%08X", (unsigned)e.code().value);
            msg += hex;
            msg += L" ";
            msg += e.message();
            SetLastErrorInfo(e.code(), msg.c_str());
        }

        {
            std::lock_guard<std::mutex> lock(g_initMutex);
            g_appReady = true; // App model initialized (window may follow)
        }
        g_initCv.notify_all();

        // Defer window creation (avoid early E_NOINTERFACE timing)
        if (!g_windowCreationScheduled.exchange(true)) {
            ScheduleWindowCreation(0);
        }
    }
};

static void LogModulePresence(const wchar_t* mod){HMODULE h=GetModuleHandleW(mod);wchar_t buf[256];_snwprintf_s(buf,_TRUNCATE,L"[ModuleCheck] %s %s\n",mod,h?L"loaded":L"NOT loaded");OutputDebugStringW(buf);} 

static void AttemptCreateMainWindow(int attempt) {
    if (g_window) return;

    try {
        g_window = Window();
    auto root = Microsoft::UI::Xaml::Controls::Grid();
    root.HorizontalAlignment(Microsoft::UI::Xaml::HorizontalAlignment::Stretch);
    root.VerticalAlignment(Microsoft::UI::Xaml::VerticalAlignment::Stretch);
    // Ensure the root can receive pointer and keyboard focus
    try { root.Background(Microsoft::UI::Xaml::Media::SolidColorBrush{ Windows::UI::Colors::Transparent() }); } catch(...) {}
    try { root.IsTabStop(true); } catch(...) {}
        g_window.Content(root);
        try { g_originalRootFE = root.as<Microsoft::UI::Xaml::FrameworkElement>(); } catch (...) {}
        g_overlayRoot = root; // treat root grid as overlay root directly
        {
            std::wstring title;
            {
                std::lock_guard<std::mutex> lock(g_windowTitleMutex);
                title = g_pendingWindowTitle; // copy under lock
            }
            if (title.empty()) title = L"Go WinUI Host";
            try { g_window.Title(title); } catch (...) {}
        }
        g_window.Activate();
        // Focus the root when the window becomes active so it receives KeyDown/Up
        try {
            g_window.Activated([root](auto&&, auto&&){
                try { root.Focus(Microsoft::UI::Xaml::FocusState::Programmatic); } catch(...) {}
            });
        } catch(...) {}
        // Subclass HWND to enforce min/max via WM_GETMINMAXINFO.
        try {
            if (auto hwnd = GetWindowHandle()) {
                LONG_PTR prev = GetWindowLongPtr(hwnd, GWLP_WNDPROC);
                g_originalWndProc = reinterpret_cast<WNDPROC>(prev);
                SetWindowLongPtr(hwnd, GWLP_WNDPROC, reinterpret_cast<LONG_PTR>(+[](HWND h, UINT msg, WPARAM w, LPARAM l) -> LRESULT {
                    if (msg == WM_GETMINMAXINFO) {
                        auto mmi = reinterpret_cast<MINMAXINFO*>(l);
                        // Convert client hints to window (outer) size considering styles.
                        RECT rc = {0,0,0,0};
                        DWORD style = (DWORD)GetWindowLongPtr(h, GWL_STYLE);
                        DWORD ex = (DWORD)GetWindowLongPtr(h, GWL_EXSTYLE);
                        auto toOuter = [&](int cw, int ch) {
                            RECT d{0,0,cw,ch};
                            if (AdjustWindowRectEx(&d, style, FALSE, ex)) {
                                return SIZE{ d.right - d.left, d.bottom - d.top };
                            }
                            return SIZE{ cw, ch };
                        };
                        int minW = g_minClientW.load(std::memory_order_relaxed);
                        int minH = g_minClientH.load(std::memory_order_relaxed);
                        int maxW = g_maxClientW.load(std::memory_order_relaxed);
                        int maxH = g_maxClientH.load(std::memory_order_relaxed);
                        if (minW > 0 || minH > 0) {
                            SIZE s = toOuter(minW > 0 ? minW : 0, minH > 0 ? minH : 0);
                            if (minW > 0) mmi->ptMinTrackSize.x = s.cx;
                            if (minH > 0) mmi->ptMinTrackSize.y = s.cy;
                        }
                        if (maxW > 0 || maxH > 0) {
                            SIZE s = toOuter(maxW > 0 ? maxW : 0, maxH > 0 ? maxH : 0);
                            if (maxW > 0) mmi->ptMaxTrackSize.x = s.cx;
                            if (maxH > 0) mmi->ptMaxTrackSize.y = s.cy;
                        }
                        // Forward to original proc so default behavior respects our adjustments.
                        if (g_originalWndProc) return CallWindowProc(g_originalWndProc, h, msg, w, l);
                        return DefWindowProc(h, msg, w, l);
                    }
                    if (g_originalWndProc) return CallWindowProc(g_originalWndProc, h, msg, w, l);
                    return DefWindowProc(h, msg, w, l);
                }));
            }
        } catch(...) {}
        // Apply pending initial size if specified before creation.
        try {
            int reqW = g_pendingInitialWidth.load(std::memory_order_relaxed);
            int reqH = g_pendingInitialHeight.load(std::memory_order_relaxed);
            if (reqW > 0 && reqH > 0) {
                if (auto hwnd = GetWindowHandle()) {
                    RECT desired{ 0,0,reqW,reqH };
                    DWORD dwStyle = (DWORD)GetWindowLongPtr(hwnd, GWL_STYLE);
                    DWORD dwExStyle = (DWORD)GetWindowLongPtr(hwnd, GWL_EXSTYLE);
                    if (AdjustWindowRectEx(&desired, dwStyle, FALSE, dwExStyle)) {
                        int outW = desired.right - desired.left;
                        int outH = desired.bottom - desired.top;
                        SetWindowPos(hwnd, nullptr, 0, 0, outW, outH, SWP_NOMOVE | SWP_NOZORDER | SWP_NOACTIVATE);
                    }
                }
            }
        } catch(...) {}
        try { EnqueueEvent({5,0,0,0,0,0,0,0}); } catch(...) {}
        {
            std::lock_guard<std::mutex> lk(g_windowReadyMutex);
            g_windowReady = true;
        }
        g_windowReadyCv.notify_all();
        // Apply any pending background color (if previously requested before window existed)
        try {
            if (g_pendingBGSet.load(std::memory_order_acquire)) {
                unsigned int argb = g_pendingBGARGB.load(std::memory_order_relaxed);
                unsigned char a = (argb >> 24) & 0xFF;
                unsigned char r = (argb >> 16) & 0xFF;
                unsigned char g = (argb >> 8) & 0xFF;
                unsigned char b = (argb) & 0xFF;
                set_window_background_color(a, r, g, b);
            }
        } catch (...) {}
        // Input events attached to root (UIElement) rather than Window
        root.KeyDown([](auto&&, Microsoft::UI::Xaml::Input::KeyRoutedEventArgs const& args) {
            int vk = static_cast<int>(args.OriginalKey());
            int mods = ComputeMods();
            // Keyboard events intentionally report x,y=0 (no pointer context).
            unsigned long long packedXY = 0; // x=y=0 for key events
            int codeWithMods = (mods << 16) | (vk & 0xFFFF);
            if (g_inputCallback) g_inputCallback(1, codeWithMods, 1, packedXY);
            try { EnqueueEvent({1,vk,1,mods,0,0,0,0}); } catch(...) {}
        });
        root.KeyUp([](auto&&, Microsoft::UI::Xaml::Input::KeyRoutedEventArgs const& args) {
            int vk = static_cast<int>(args.OriginalKey());
            int mods = ComputeMods();
            // Keyboard events intentionally report x,y=0 (no pointer context).
            unsigned long long packedXY = 0;
            int codeWithMods = (mods << 16) | (vk & 0xFFFF);
            if (g_inputCallback) g_inputCallback(1, codeWithMods, 2, packedXY);
            try { EnqueueEvent({1,vk,2,mods,0,0,0,0}); } catch(...) {}
        });
        root.PointerPressed([](auto&&, Microsoft::UI::Xaml::Input::PointerRoutedEventArgs const& args) {
            auto src = args.OriginalSource().try_as<Microsoft::UI::Xaml::UIElement>();
            auto point = args.GetCurrentPoint(src);
            int button = 0;
            auto props = point.Properties();
            if (props.IsLeftButtonPressed()) button = 1;
            else if (props.IsRightButtonPressed()) button = 2;
            else if (props.IsMiddleButtonPressed()) button = 3;
            else if (props.IsXButton1Pressed()) button = 4;
            else if (props.IsXButton2Pressed()) button = 5;
            g_lastPointerButton = button;
            int mods = ComputeMods();
            // Position() returns element-relative (root) pixels; treat as client coordinates.
            int x = static_cast<int>(point.Position().X);
            int y = static_cast<int>(point.Position().Y);
            unsigned long long packedXY = (static_cast<unsigned long long>(static_cast<unsigned int>(y)) << 32) | (static_cast<unsigned long long>(static_cast<unsigned int>(x)));
            int codeWithMods = (mods << 16) | (button & 0xFFFF);
            if (g_inputCallback) g_inputCallback(2, codeWithMods, 1, packedXY);
            try { EnqueueEvent({2,button,1,mods,x,y,0,0}); } catch(...) {}
        });
        root.PointerReleased([](auto&&, Microsoft::UI::Xaml::Input::PointerRoutedEventArgs const& args) {
            auto src = args.OriginalSource().try_as<Microsoft::UI::Xaml::UIElement>();
            auto point = args.GetCurrentPoint(src);
            int mods = ComputeMods();
            // Position() returns element-relative (root) pixels; treat as client coordinates.
            int x = static_cast<int>(point.Position().X);
            int y = static_cast<int>(point.Position().Y);
            int button = g_lastPointerButton;
            unsigned long long packedXY = (static_cast<unsigned long long>(static_cast<unsigned int>(y)) << 32) | (static_cast<unsigned long long>(static_cast<unsigned int>(x)));
            int codeWithMods = (mods << 16) | (button & 0xFFFF);
            if (g_inputCallback) g_inputCallback(2, codeWithMods, 2, packedXY);
            g_lastPointerButton = 0;
            try { EnqueueEvent({2,button,2,mods,x,y,0,0}); } catch(...) {}
        });
        // Closed handler: enqueue closed event then start shutdown asynchronously (callback now fired at end of ShutdownUI only).
        g_window.Closed([](auto&&, auto&&) {
            try { EnqueueEvent({4,0,0,0,0,0,0,0}); } catch(...) {}
            static std::atomic<bool> launched{ false };
            if (!launched.exchange(true)) {
                std::thread([](){
                    try { ShutdownUI(); } catch(...) {}
                }).detach();
            }
        });
        // SizeChanged handler to propagate dimensions to Go layer (if callback registered later we still hook here).
        g_window.SizeChanged([](auto&&, Microsoft::UI::Xaml::WindowSizeChangedEventArgs const& args) {
            if (g_resizeCallback) {
                double w = args.Size().Width;
                double h = args.Size().Height;
                uint64_t wb = *reinterpret_cast<uint64_t*>(&w);
                uint64_t hb = *reinterpret_cast<uint64_t*>(&h);
                g_resizeCallback(wb, hb);
            }
            try { EnqueueEvent({3,0,0,0,0,0,args.Size().Width,args.Size().Height}); } catch(...) {}
        });
        ControlHandle h = reinterpret_cast<ControlHandle>(winrt::get_abi(g_window));
        g_controls.insert({ h, g_window.as<FrameworkElement>() });

    LogModulePresence(L"Microsoft.UI.Xaml.dll");
    LogModulePresence(L"Microsoft.WindowsAppRuntime.Bootstrap.dll");
    LogModulePresence(L"mrt100_app.dll");

        SetLastErrorInfo(S_OK, L"Main window created");
    }
    catch (const winrt::hresult_error& e) {
        HRESULT hr = e.code();
        if (hr == 0x80004002 /*E_NOINTERFACE*/ && attempt + 1 < kMaxWindowCreateAttempts) {
            wchar_t msg[256];
            _snwprintf_s(msg, _TRUNCATE,
                L"Window create E_NOINTERFACE attempt=%d hr=0x%08X retrying",
                attempt, (unsigned)hr);
            SetLastErrorInfo(hr, msg);
            ScheduleWindowCreation(attempt + 1);
        }
        else {
            std::wstring m = L"Window creation failed hr=0x";
            wchar_t hex[9]; swprintf_s(hex, L"%08X", (unsigned)hr);
            m += hex;
            m += L" ";
            m += e.message();
            if (attempt + 1 >= kMaxWindowCreateAttempts) {
                m += L" (giving up)";
            }
            SetLastErrorInfo(hr, m.c_str());
        }
    }
    catch (...) {
        SetLastErrorInfo(E_FAIL, L"Window creation unknown exception");
    }
}

static void ScheduleWindowCreation(int attempt){std::thread([attempt]{int delayMs=50*(attempt+1);std::this_thread::sleep_for(std::chrono::milliseconds(delayMs));if(g_dispatcherQueue){g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler([attempt]{AttemptCreateMainWindow(attempt);}));}}).detach();}

static HRESULT StartAppThread() {
    std::lock_guard<std::mutex> lock(g_initMutex);
    if (g_appThreadStarted) return S_OK;
    g_appThreadStarted = true;

    try {
        g_uiThread = std::thread([] {
            HRESULT hrBootstrap = TryBootstrapMulti();
            if (FAILED(hrBootstrap)) {
                LogHRESULT(L"All bootstrap attempts failed", hrBootstrap);
                {
                    std::lock_guard<std::mutex> lk(g_initMutex);
                    g_appReady = true;
                }
                g_initCv.notify_all();
                return;
            }

            SetLastErrorInfo(S_OK, L"Bootstrap succeeded; initializing apartment");
            // Register deferred bootstrap shutdown only if explicitly enabled.
            if (!g_bootstrapShutdownRegistered) {
                wchar_t enableBuf[8];
                if (GetEnvironmentVariableW(L"WINUI_ENABLE_BOOTSTRAP_SHUTDOWN", enableBuf, _countof(enableBuf)) > 0) {
                    g_bootstrapShutdownRegistered = true;
                    ::atexit(DeferredBootstrapShutdown);
                    try { OutputDebugStringW(L"[Bootstrap] Registered DeferredBootstrapShutdown via atexit (opt-in)\n"); } catch(...) {}
                } else {
                    try { OutputDebugStringW(L"[Bootstrap] Skipping bootstrap shutdown registration (default)\n"); } catch(...) {}
                }
            }
            winrt::init_apartment(apartment_type::single_threaded);
            if (!g_vectoredHandler) {
                g_vectoredHandler = AddVectoredExceptionHandler(1, CrashDiagVectoredHandler);
                OutputDebugStringW(L"[CrashDiag] Vectored exception handler registered\n");
                // Log key module bases for later offset correlation
                auto logMod = [](const wchar_t* mod) {
                    HMODULE h = GetModuleHandleW(mod);
                    if (h) {
                        wchar_t b[256]; _snwprintf_s(b, _TRUNCATE, L"[CrashDiag] ModuleBase %s=%p\n", mod, h);
                        OutputDebugStringW(b);
                    }
                };
                logMod(L"WinUI3Native.dll");
                logMod(L"Microsoft.UI.Xaml.dll");
                logMod(L"WindowsApp.dll");
                logMod(L"mrt100_app.dll");
            }

            Application::Start([](auto&&) {
                winrt::make<NativeApp>();
            });

            try { LogSeq(L"UI thread Application::Start returned; uninitializing apartment"); } catch(...) {}
            wchar_t skipBuf[8];
            if (GetEnvironmentVariableW(L"WINUI_SKIP_UNINIT", skipBuf, _countof(skipBuf)) > 0) {
                LogSeq(L"WINUI_SKIP_UNINIT set; skipping winrt::uninit_apartment");
            } else {
                try { winrt::uninit_apartment(); LogSeq(L"winrt::uninit_apartment completed on UI thread"); } catch(...) {}
            }
            try { LogSeq(L"UI thread exiting"); } catch(...) {}
            });
    }
    catch (...) {
        SetLastErrorInfo(E_FAIL, L"Failed to start UI thread");
        return E_FAIL;
    }
    return S_OK;
}

static HRESULT WaitForAppReady() {
    std::unique_lock<std::mutex> lk(g_initMutex);
    g_initCv.wait(lk, [] { return g_appReady; });
    return g_lastHRESULT;
}

static bool IsOnUIThread() {
    return g_uiThreadId != 0 && g_uiThreadId == ::GetCurrentThreadId();
}

extern "C" {

    ControlHandle __stdcall get_main_window() {
        if (g_window) {
            return reinterpret_cast<ControlHandle>(winrt::get_abi(g_window));
        }
        return nullptr;
    }

    HRESULT __stdcall winui_last_hresult() {
        return g_lastHRESULT;
    }

    const wchar_t* __stdcall winui_last_error_message() {
        return g_lastErrorMessage.c_str();
    }

    const wchar_t* __stdcall winui_last_unhandled_exception_message() {
        return g_unhandledExceptionMessage.empty() ? L"" : g_unhandledExceptionMessage.c_str();
    }

    HRESULT __stdcall InitUI() {
        HRESULT hr = StartAppThread();
        if (FAILED(hr)) return hr;
        hr = WaitForAppReady();
        if (FAILED(g_lastHRESULT)) return g_lastHRESULT;

        if (!g_dispatcherQueue) {
            SetLastErrorInfo(E_FAIL, L"DispatcherQueue not available after app start");
            return E_FAIL;
        }

        // If window still pending, the retry cycle is in progress; do not treat as failure.
        if (!g_window) {
            SetLastErrorInfo(S_OK, L"InitUI: app ready (window pending)");
        }
        else {
            SetLastErrorInfo(S_OK, L"InitUI: app + window ready");
        }
        return S_OK;
    }

    void __stdcall ShutdownUI() {
        static std::atomic<bool> finished{ false };
        bool firstCall = false;
        {
            std::lock_guard<std::mutex> lk(g_initMutex);
            if (!g_shutdownRequested) { g_shutdownRequested = true; firstCall = true; }
        }
        if (!firstCall) {
            // Fast idempotent path: if already fully finished just return.
            if (finished.load(std::memory_order_acquire)) {
                SetLastErrorInfo(S_OK, L"Shutdown complete (idempotent fast-path)");
                return;
            }
        }

        LogSeq(L"ShutdownUI invoked (begin)");
        if (firstCall) LogSeq(L"ShutdownUI first-call path"); else LogSeq(L"ShutdownUI repeat-call path");

        // For first call, marshal release of WinRT/XAML objects ON the UI thread, then exit app.
        if (firstCall && g_dispatcherQueue) {
            LogSeq(L"Enqueue UI-thread cleanup + app.Exit");
            auto cleanupAndExit = []() {
                try {
                    LogSeq(L"[UI] Cleanup lambda start");
                    // Release WinRT objects on UI thread to avoid cross-thread final release after dispatcher shutdown.
                    g_resizeCallback = nullptr;
                    g_inputCallback = nullptr;
                    g_originalRootFE = nullptr;
                    g_overlayText = nullptr;
                    g_overlayRoot = nullptr;
                    g_controls.clear();
                    // Capture then clear window last so any dependent objects already released.
                    g_window = nullptr;
                    LogSeq(L"[UI] Objects released; calling app.Exit");
                    if (auto app = Application::Current()) {
                        app.Exit();
                    }
                    LogSeq(L"[UI] Cleanup lambda end");
                } catch(...) {}
            };
            g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler(cleanupAndExit));
        }

        // Watchdog: ensure we always have a safety timeout even if ShutdownUI
        // is invoked only after the Closed handler set g_shutdownRequested (non-first call path).
        {
            static std::atomic<bool> watchdogStarted{ false };
            if (!watchdogStarted.exchange(true)) {
                std::thread([]() {
                    constexpr int maxWaitMs = 2000;
                    for (int waited = 0; waited < maxWaitMs; waited += 100) {
                        if (g_uiThread.joinable() == false) return; // already joined
                        if (waited == 0) LogSeq(L"Watchdog started (2s timeout)");
                        Sleep(100);
                    }
                    LogSeq(L"Watchdog timeout; forcing _exit(0)");
                    fflush(nullptr);
                    _exit(0);
                }).detach();
            }
        }

        // If we are not on the first-call path (window Closed invoked earlier), we may have
        // missed scheduling cleanup / app.Exit from the first-call branch. Attempt an extra
        // app.Exit dispatch just in case dispatcher still alive.
        if (!firstCall && g_dispatcherQueue) {
            try {
                g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler([]() {
                    try {
                        if (auto app = Application::Current()) {
                            app.Exit();
                        }
                    } catch(...) {}
                }));
            } catch(...) {}
        }

        if (g_uiThread.joinable()) {
            LogSeq(L"Joining UI thread");
            try { g_uiThread.join(); } catch(...) {}
            LogSeq(L"UI thread joined");
        } else {
            LogSeq(L"UI thread not joinable (already joined?)");
        }

        LogSeq(L"Skipping MddBootstrapShutdown (default)");

        // Dispatcher has exited at this point (UI thread joined). Only clear non-WinRT state here.
        g_dispatcherQueue = nullptr;
        g_uiThreadId = 0;
        {
            std::lock_guard<std::mutex> lk(g_initMutex);
            g_appReady = false;
            g_appThreadStarted = false;
            g_windowCreationScheduled = false;
            g_windowCreateAttempts.store(0);
        }
        finished.store(true, std::memory_order_release);
        try { OutputDebugStringW(L"[Shutdown] Native teardown complete\n"); } catch(...) {}
        LogSeq(L"ShutdownUI complete");
        // Fire close callback exactly once here (teardown finished).
        static std::atomic<bool> closeCbFired{ false };
        if (!closeCbFired.exchange(true)) {
            if (g_closeCallback) { try { g_closeCallback(); } catch(...) {} }
        }
        {
            std::lock_guard<std::mutex> lk(g_windowReadyMutex);
            g_windowReady = false;
        }
        SetLastErrorInfo(S_OK, firstCall ? L"Shutdown complete" : L"Shutdown complete (idempotent late)");
    }

    int __stdcall window_exists() {
        return g_window ? 1 : 0;
    }

    // Returns 1 if a window object exists AND has Content set (activated root), else 0.
    int __stdcall is_window_ready() {
        std::lock_guard<std::mutex> lk(g_windowReadyMutex);
        return (g_windowReady && g_window) ? 1 : 0;
    }

    int __stdcall wait_for_window_ready(int timeoutMs) {
        if (timeoutMs <= 0) timeoutMs = 5000;
        std::unique_lock<std::mutex> lk(g_windowReadyMutex);
        if (g_windowReady && g_window) return 1;
        auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(timeoutMs);
        while (!g_windowReady || !g_window) {
            if (g_windowReadyCv.wait_until(lk, deadline) == std::cv_status::timeout) break;
        }
        return (g_windowReady && g_window) ? 1 : 0;
    }

    // Diagnostic: returns snapshot of runtime state.
    void __stdcall get_runtime_state(int* ready, int* shutdown, int* controlsCount) {
        if (ready) *ready = 0; if (shutdown) *shutdown = 0; if (controlsCount) *controlsCount = 0;
        try {
            std::lock_guard<std::mutex> lk(g_windowReadyMutex);
            if (ready) *ready = (g_windowReady && g_window) ? 1 : 0;
        } catch(...) {}
        if (shutdown) { try { *shutdown = g_shutdownRequested ? 1 : 0; } catch(...) {} }
        if (controlsCount) { try { *controlsCount = (int)g_controls.size(); } catch(...) {} }
    }

    void __stdcall set_window_title(const wchar_t* title) {
        if (!title) return;
        if (!g_window) return;
        // If we're already on UI thread just set directly.
        if (IsOnUIThread()) {
            try { g_window.Title(title); } catch (...) {}
            return;
        }
        // Marshal to dispatcher thread.
        if (g_dispatcherQueue) {
            std::wstring copy;
            try { copy = title; } catch (...) { return; }
            g_dispatcherQueue.TryEnqueue(
                Microsoft::UI::Dispatching::DispatcherQueueHandler([copy]() {
                    try { if (g_window) g_window.Title(copy.c_str()); } catch (...) {}
                })
            );
        }
    }

    void __stdcall get_window_size(double* width, double* height) {
        if (width) *width = 0.0; if (height) *height = 0.0;
        if (!g_window) return;
        try {
            auto appWindow = g_window.AppWindow();
            if (appWindow) {
                auto sz = appWindow.Size();
                if (width) *width = static_cast<double>(sz.Width);
                if (height) *height = static_cast<double>(sz.Height);
            }
        } catch (...) {}
    }

    void __stdcall register_resize_callback(resize_callback_t cb) {
        g_resizeCallback = cb;
        // Optionally push a current size immediately
        if (g_resizeCallback && g_window) {
            try {
                auto appWindow = g_window.AppWindow();
                if (appWindow) {
                    auto sz = appWindow.Size();
                    double w = static_cast<double>(sz.Width);
                    double h = static_cast<double>(sz.Height);
                    uint64_t wb = *reinterpret_cast<uint64_t*>(&w);
                    uint64_t hb = *reinterpret_cast<uint64_t*>(&h);
                    g_resizeCallback(wb, hb);
                }
            } catch (...) {}
        }
    }

    void __stdcall register_input_callback(input_event_callback_t cb) {
        g_inputCallback = cb;
    }

    void __stdcall register_close_callback(close_callback_t cb) {
        g_closeCallback = cb;
    }

    // begin_shutdown_async: non-blocking request to start ShutdownUI if not already in progress.
    void __stdcall begin_shutdown_async() {
        static std::atomic<bool> started{ false };
        if (started.exchange(true)) return;
        std::thread([](){
            try { ShutdownUI(); } catch(...) {}
        }).detach();
    }

    void __stdcall set_window_background_color(unsigned char a, unsigned char r, unsigned char g, unsigned char b) {
        if (g_shutdownRequested) return; // early out if shutting down
        if (!g_window) {
            // Persist request and return; will be applied after creation.
            unsigned int argb = ((unsigned int)a << 24) | ((unsigned int)r << 16) | ((unsigned int)g << 8) | (unsigned int)b;
            g_pendingBGARGB.store(argb, std::memory_order_relaxed);
            g_pendingBGSet.store(true, std::memory_order_release);
            return;
        }
        static std::atomic<int> g_bgApplyCount{0};
        auto apply = [a, r, g, b]() {
            if (g_shutdownRequested || !g_window) return;
            try {
                auto currentRoot = g_window.Content();
                if (!currentRoot) return;
                // If we already have a Grid root (preferred path) reuse it.
                if (!g_overlayRoot) {
                    if (auto gridTry = currentRoot.try_as<Microsoft::UI::Xaml::Controls::Grid>()) {
                        g_overlayRoot = gridTry;
                        try { OutputDebugStringW(L"[bg] overlay root bound to existing root grid\n"); } catch (...) {}
                    } else {
                        // Fallback: wrap existing content with a new Grid (legacy path)
                        auto grid = Microsoft::UI::Xaml::Controls::Grid();
                        grid.HorizontalAlignment(Microsoft::UI::Xaml::HorizontalAlignment::Stretch);
                        grid.VerticalAlignment(Microsoft::UI::Xaml::VerticalAlignment::Stretch);
                        grid.Children().Append(currentRoot);
                        g_window.Content(grid);
                        g_overlayRoot = grid;
                        try { OutputDebugStringW(L"[bg] overlay root created (wrap)\n"); } catch (...) {}
                    }
                }
                Microsoft::UI::Xaml::Media::SolidColorBrush brush{ Windows::UI::Color{ a, r, g, b } };
                // Always paint overlay root (grid) so color is guaranteed visible.
                try { if (g_overlayRoot) g_overlayRoot.Background(brush); } catch (...) {}
                // Optionally also paint underlying child (index 0) if it supports Background (Panel/ContentControl)
                try {
                    if (g_overlayRoot && g_overlayRoot.Children().Size() > 0) {
                        auto child = g_overlayRoot.Children().GetAt(0);
                        if (auto fe = child.try_as<Microsoft::UI::Xaml::FrameworkElement>()) {
                            if (auto panel = fe.try_as<Microsoft::UI::Xaml::Controls::Panel>()) {
                                try { panel.Background(brush); } catch (...) {}
                            } else if (auto cc = fe.try_as<Microsoft::UI::Xaml::Controls::ContentControl>()) {
                                try { cc.Background(brush); } catch (...) {}
                            }
                        }
                    }
                } catch (...) {}
                try {
                    OutputDebugStringW(L"[bg] overlay root background set\n");
                    int c = ++g_bgApplyCount;
                    wchar_t buf2[128];
                    _snwprintf_s(buf2, _TRUNCATE, L"[bg] apply-count=%d\n", c);
                    OutputDebugStringW(buf2);
                } catch (...) {}
                try {
                    wchar_t buf[160];
                    _snwprintf_s(buf, _TRUNCATE, L"[bg] ARGB=(%u,%u,%u,%u)\n", a, r, g, b);
                    OutputDebugStringW(buf);
                } catch (...) {}
            } catch (...) {}
        };
        if (IsOnUIThread()) {
            apply();
        } else if (g_dispatcherQueue) {
            g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler(apply));
        }
    }

    void __stdcall set_center_overlay_text(const wchar_t* text, double fontSize) {
        if (g_shutdownRequested || !g_window) return;
        auto apply = [textStr = std::wstring(text ? text : L""), fontSize]() {
            if (g_shutdownRequested || !g_window) return;
            try {
                auto existingRoot = g_window.Content();
                if (!existingRoot) return;
                if (!g_overlayRoot) {
                    auto grid = Microsoft::UI::Xaml::Controls::Grid();
                    grid.HorizontalAlignment(Microsoft::UI::Xaml::HorizontalAlignment::Stretch);
                    grid.VerticalAlignment(Microsoft::UI::Xaml::VerticalAlignment::Stretch);
                    grid.Children().Append(existingRoot);
                    g_window.Content(grid);
                    g_overlayRoot = grid;
                }
                if (textStr.empty()) {
                    if (g_overlayText) {
                        g_overlayText.Text(L"");
                        g_overlayText.Visibility(Microsoft::UI::Xaml::Visibility::Collapsed);
                    }
                    return;
                }
                if (!g_overlayText) {
                    g_overlayText = Microsoft::UI::Xaml::Controls::TextBlock();
                    g_overlayText.HorizontalAlignment(Microsoft::UI::Xaml::HorizontalAlignment::Center);
                    g_overlayText.VerticalAlignment(Microsoft::UI::Xaml::VerticalAlignment::Center);
                    g_overlayText.TextAlignment(Microsoft::UI::Xaml::TextAlignment::Center);
                    g_overlayText.Foreground(Microsoft::UI::Xaml::Media::SolidColorBrush{ Windows::UI::Colors::White() });
                    g_overlayText.IsHitTestVisible(false);
                    g_overlayText.FontWeight(Windows::UI::Text::FontWeights::Bold());
                    g_overlayRoot.Children().Append(g_overlayText);
                }
                g_overlayText.Text(textStr);
                if (fontSize > 0) g_overlayText.FontSize(fontSize);
                g_overlayText.Visibility(Microsoft::UI::Xaml::Visibility::Visible);
            } catch (...) {}
        };
        if (IsOnUIThread()) apply(); else if (g_dispatcherQueue) g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler(apply));
    }

    // Set min/max client size hints. 0 clears the respective bound.
    void __stdcall set_window_min_max(int minW, int minH, int maxW, int maxH) {
        g_minClientW.store(minW, std::memory_order_relaxed);
        g_minClientH.store(minH, std::memory_order_relaxed);
        g_maxClientW.store(maxW, std::memory_order_relaxed);
        g_maxClientH.store(maxH, std::memory_order_relaxed);
        try {
            if (auto hwnd = GetWindowHandle()) {
                // Nudge window to re-evaluate constraints
                RECT rc{}; if (GetWindowRect(hwnd, &rc)) {
                    SetWindowPos(hwnd, nullptr, rc.left, rc.top, rc.right-rc.left, rc.bottom-rc.top,
                        SWP_NOZORDER | SWP_NOACTIVATE | SWP_NOMOVE | SWP_NOSENDCHANGING | SWP_FRAMECHANGED);
                }
            }
        } catch(...) {}
    }

    ControlHandle __stdcall create_window(int width, int height, const wchar_t* title) {
        // If window already exists, optionally retitle and return. If size provided and different, attempt resize.
        if (g_window) {
            try {
                if (title && *title) {
                    g_window.Title(title);
                }
                if (width > 0 && height > 0) {
                    if (auto hwnd = GetWindowHandle()) {
                        RECT rc{};
                        if (GetClientRect(hwnd, &rc)) {
                            int curW = rc.right - rc.left;
                            int curH = rc.bottom - rc.top;
                            if (curW != width || curH != height) {
                                // Adjust window size so client area matches requested width/height.
                                RECT desired{ 0,0,width,height };
                                DWORD dwStyle = (DWORD)GetWindowLongPtr(hwnd, GWL_STYLE);
                                DWORD dwExStyle = (DWORD)GetWindowLongPtr(hwnd, GWL_EXSTYLE);
                                if (AdjustWindowRectEx(&desired, dwStyle, FALSE, dwExStyle)) {
                                    int outW = desired.right - desired.left;
                                    int outH = desired.bottom - desired.top;
                                    SetWindowPos(hwnd, nullptr, 0, 0, outW, outH, SWP_NOMOVE | SWP_NOZORDER | SWP_NOACTIVATE);
                                }
                            }
                        }
                    }
                }
            } catch (...) {}
            ControlHandle h = reinterpret_cast<ControlHandle>(winrt::get_abi(g_window));
            SetLastErrorInfo(S_OK, L"create_window returned existing window");
            return h;
        }

        // Store pending title & size for creation thread.
        if (title && *title) {
            try {
                std::lock_guard<std::mutex> lock(g_windowTitleMutex);
                g_pendingWindowTitle = title; // last call wins before creation
            } catch (...) {}
        }
        if (width > 0) g_pendingInitialWidth.store(width, std::memory_order_relaxed);
        if (height > 0) g_pendingInitialHeight.store(height, std::memory_order_relaxed);

        if (!g_windowCreationScheduled.exchange(true)) {
            ScheduleWindowCreation(0);
        }

        SetLastErrorInfo(S_OK, L"create_window: window not ready (scheduled)");
        return nullptr;
    }




    // create_text_input: creates a TextBox control with optional initial text.
    ControlHandle __stdcall create_text_input(ControlHandle parent_handle, const wchar_t* content) {
        if (!parent_handle) {
            SetLastErrorInfo(E_INVALIDARG, L"create_text_input: parent null");
            return nullptr;
        }
        if (!g_dispatcherQueue) {
            SetLastErrorInfo(E_FAIL, L"create_text_input: dispatcher unavailable");
            return nullptr;
        }

        std::promise<ControlHandle> promise;
        auto fut = promise.get_future();
        auto promisePtr = std::make_shared<std::promise<ControlHandle>>(std::move(promise));

        auto op = [promisePtr, parent_handle, content]() {
            try {
                auto it = g_controls.find(parent_handle);
                if (it == g_controls.end()) {
                    SetLastErrorInfo(E_INVALIDARG, L"create_text_input: parent not found");
                    promisePtr->set_value(nullptr);
                    return;
                }

                auto parentFE = it->second;
                Microsoft::UI::Xaml::Controls::TextBox tb;
                if (content && *content) {
                    tb.Text(content);
                }

                bool attached = false;
                if (auto parentPanel = parentFE.try_as<Panel>()) {
                    parentPanel.Children().Append(tb);
                    attached = true;
                } else if (auto parentContent = parentFE.try_as<ContentControl>()) {
                    parentContent.Content(tb);
                    attached = true;
                }

                if (!attached) {
                    SetLastErrorInfo(E_FAIL, L"create_text_input: unsupported parent type");
                    promisePtr->set_value(nullptr);
                    return;
                }

                ControlHandle handle = reinterpret_cast<ControlHandle>(winrt::get_abi(tb));
                g_controls.insert({ handle, tb.as<FrameworkElement>() });
                SetLastErrorInfo(S_OK, L"create_text_input succeeded");
                promisePtr->set_value(handle);
            } catch (const winrt::hresult_error& e) {
                std::wstring msg = L"create_text_input failed: ";
                msg += e.message();
                SetLastErrorInfo(e.code(), msg.c_str());
                promisePtr->set_value(nullptr);
            } catch (...) {
                SetLastErrorInfo(E_FAIL, L"create_text_input failed: unknown");
                promisePtr->set_value(nullptr);
            }
        };

        if (IsOnUIThread()) {
            op();
        } else {
            if (!g_dispatcherQueue.TryEnqueue(Microsoft::UI::Dispatching::DispatcherQueueHandler(op))) {
                SetLastErrorInfo(E_FAIL, L"create_text_input: enqueue failed");
                return nullptr;
            }
        }
        return fut.get();
    }


    int __stdcall winui_poll_events(WinUIEvent* outEvents, int max, int* more) {
        if (!outEvents || max <= 0) { if (more) *more = 0; return 0; }
        int count = 0;
        int tail = g_eventTail.load(std::memory_order_acquire);
        int head = g_eventHead.load(std::memory_order_acquire);
        while (tail != head && count < max) {
            auto &src = g_eventRing[tail];
            outEvents[count].kind = src.kind;
            outEvents[count].code = src.code;
            outEvents[count].action = src.action;
            outEvents[count].mods = src.mods;
            outEvents[count].x = src.x;
            outEvents[count].y = src.y;
            outEvents[count].w = src.w;
            outEvents[count].h = src.h;
            ++count;
            tail = (tail + 1) % kEventRingSize;
        }
        g_eventTail.store(tail, std::memory_order_release);
        int newHead = g_eventHead.load(std::memory_order_acquire);
        if (more) *more = (tail != newHead) ? 1 : 0;
        return count;
    }

} // extern "C"

static void DeferredBootstrapShutdown(){wchar_t enableBuf[8];if(GetEnvironmentVariableW(L"WINUI_ENABLE_BOOTSTRAP_SHUTDOWN",enableBuf,_countof(enableBuf))==0)return; if(g_bootstrapVersion){try{OutputDebugStringW(L"[Bootstrap] DeferredBootstrapShutdown begin (opt-in)\n");}catch(...){} try{MddBootstrapShutdown();OutputDebugStringW(L"[Bootstrap] DeferredBootstrapShutdown complete (opt-in)\n");}catch(...){try{OutputDebugStringW(L"[Bootstrap] DeferredBootstrapShutdown exception (opt-in)\n");}catch(...){} }} }

extern "C" __declspec(dllexport) void __stdcall winui_force_bootstrap_shutdown(){DeferredBootstrapShutdown();}

extern "C" __declspec(dllexport) void __stdcall winui_request_close(){
    try {
        if(!g_window) return;
        auto disp = g_window.DispatcherQueue();
        if(disp){
            disp.TryEnqueue([](){
                try{ if(g_window) g_window.Close(); }catch(...){}
            });
        }else{
            try{ g_window.Close(); }catch(...){}
        }
    }catch(...){}
}