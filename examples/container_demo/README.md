# Container Demo

This example demonstrates the usage of the new container control functions added to go-winui3:

- `create_stack_panel` - Creates a StackPanel container
- `create_grid` - Creates a Grid container  
- `add_child` - Adds child controls to parent containers

## Functions Added

### Native (WinUI3Native.cpp/.h)

1. **create_stack_panel()** - Creates a StackPanel container control
2. **create_grid()** - Creates a Grid container control
3. **add_child(parent, child)** - Adds a child control to a parent container

All functions run on the UI thread using the existing dispatcher queue pattern.

### Go Wrapper (internal/winui/winui.go)

1. **CreateStackPanel() Handle** - Go wrapper for create_stack_panel
2. **CreateGrid() Handle** - Go wrapper for create_grid  
3. **AddChild(parent, child Handle)** - Go wrapper for add_child

## Features

- **UI Thread Safety**: All operations are marshaled to the UI thread automatically
- **Container Support**: Works with StackPanel, Grid, ContentControl, Border, and other WinUI containers
- **Flexible Parent-Child Relationships**: The add_child function intelligently determines the container type and adds children appropriately

## Usage

```go
// Create containers
stackPanel := winui.CreateStackPanel()
grid := winui.CreateGrid()

// Create controls
textInput := winui.CreateTextInput(0, "Hello World")

// Add children to containers
winui.AddChild(stackPanel, textInput)
winui.AddChild(stackPanel, grid)  // Nested containers
```

## Building

To build and run this example:

1. First build the native DLL:
   ```bash
   cd native/WinUI3Native
   # Build using Visual Studio or MSBuild
   ```

2. Run the Go example:
   ```bash
   cd examples/container_demo
   go run main.go
   ```

## Notes

- The native functions use proper UI thread marshaling with DispatcherQueue.TryEnqueue()
- Container creation functions return synchronously after the UI thread operations complete
- The add_child function supports multiple container types and chooses the appropriate method for adding children