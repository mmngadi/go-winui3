# YourGoApp (Example)

This example demonstrates using the `internal/winui` Go wrapper with the native `WinUI3Native.dll` to create a basic WinUI 3 window, handle input, print diagnostics, and enforce min/max client sizes.

## Build

Use the repo build script and point it at this example directory with `-ExamplePath`.

Debug build (console visible, logs enabled):

```
powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Debug -Platform x64 -Verbose -ExamplePath examples/yourgoapp
```

Release build (GUI subsystem, no console):

```
powershell -ExecutionPolicy Bypass -File build.ps1 -Configuration Release -Platform x64 -ExamplePath examples/yourgoapp
```

Artifacts are placed under `bin/<arch>/<config>`.

## Run

Debug:

```
./bin/x64/Debug/YourGoApp.exe
```

Release:

```
./bin/x64/Release/YourGoApp.exe
```

Note: The app loads `WinUI3Native.dll` from the corresponding `bin` directory. If running the EXE from elsewhere, either add that folder to `PATH` or edit the example to call `winui.Load("path/to/bin/x64/<Config>")` before `winui.Init()`.

## Keyboard Shortcuts

- F11: Toggle borderless fullscreen
- F6: Print diagnostics — DPI scale, window position, rounded size, client size, and outer size
- F7: Set min client size to 800x600 (prevents sizing smaller)
- F8: Clear min/max size limits (restore free resizing)

## What You’ll See

- Debounced resize logs like `[resize] 1280x720` after resizing settles.
- Input logs for keys/mouse and Unicode characters pressed.
- F6 diagnostic lines, e.g.: `DPI scale=(1.00,1.00) pos=(X,Y) size=WxH client=CW×CH outer=OW×OH`.
- F7/F8 affect actual resize behavior via native `WM_GETMINMAXINFO` enforcement.

## Troubleshooting

- Missing MSBuild/VS: You can use `-SkipNative` if the native DLLs already exist in `bin/`.
- DLL not found: Ensure you built at least once and are running from the `bin/<arch>/<config>` directory, or update `winui.Load()` path in `main.go`.
- DPI/Multimonitor: Reported sizes/positions are in physical pixels; F6 helps confirm client vs outer sizes.
