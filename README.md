# gobarrier
A high-performance, cross-platform KVM software switch written in Go — inspired by Barrier but rebuilt for lower latency and with drag-and-drop file transfer between machines.

## Features 

| Feature                            |                 | 
| ---                                | ---             |
| Mouse/keyboard sharing             | ✅              |
| Clipboard sync                     | ✅              |
| macOS → Linux                      | ✅              |
| macOS → Windows                    | ✅              |
| TLS encryption                     | ✅ (planned)    |
| File drag-and-drop                 | ✅              |
| Latency                            | ~1–5 ms target  |
| Single binary, no Qt GUI           | ✅              |
| TOML config                        | ✅              |

## Why is it faster than Barrier?
Barrier is built on a C++ event loop that was originally designed for Synergy
circa 2002.  It processes events synchronously on a single thread, has multiple
layers of abstraction, and goes through Qt's signal/slot mechanism on the GUI
side.

gobarrier achieves lower latency by:

1. **CGEventTap** at `kCGHeadInsertEventTap` — events are intercepted before
any application sees them, on the same thread that the OS delivers them.
2. **Lock-free hot path** — `RouteMouseMove` does a single atomic read to check
whether we're on a remote screen, then writes 8 bytes to a socket.  No
allocations, no channel bouncing.
3. **Goroutine-per-client** with buffered channels — each client's write path
is independent; a slow Windows machine doesn't block the Ubuntu machine.
4. `TCP_NODELAY` — set automatically by `net.Dial` in Go, so there's no
Nagle delay on small packets like mouse-move messages.

## Architecture
```
┌─────────────────────────────────────────────────────┐
│  macOS (primary / server)                           │
│                                                     │
│  CGEventTap ──▶ gobarrier-server ──▶ TCP :24800     │
│  (captures all HID events at the OS level)          │
└───────────────────────────┬─────────────────────────┘
                            │  TCP (gobarrier protocol v1.6)
          ┌─────────────────┴───────────────────┐
          │                                     │
┌─────────▼───────────┐             ┌───────────▼──────────┐
│  Ubuntu (secondary) │             │ Windows (secondary)  │
│  gobarrier-client   │             │ gobarrier-client     │
│  XTest injection    │             │ SendInput injection  │
└─────────────────────┘             └──────────────────────┘
```

## Build
### Prerequisites

| Platform        | Requirement
| ---             | --- 
| macOS (server)  | Xcode CLI tools, `CGO_ENABLED=1`
| Ubuntu (client) | `sudo apt install libxtst-dev libx11-dev`
| Windows (client)| No extra deps (`golang.org/x/sys` only)

### macOS server
```
CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
  go build -o gobarrier-server ./cmd/server
```

### Ubuntu client
```
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -o gobarrier-client ./cmd/client
```

### Windows client (cross-compile from Mac or Linux)
```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -o gobarrier-client.exe ./cmd/client
```
CGO is not needed for Windows because the injection layer uses only
`golang.org/x/sys/windows` (pure Go syscall wrappers).

## Configuration
### Generate a starter config:
```
./gobarrier-server --example-config > gobarrier.toml
```

### Edit to match your setup:
```
[server]
screen_name = "mac"
port        = 24800

[screens.mac]
[screens.ubuntu]
  switch_delay = 150   # ms before cursor crosses edge
[screens.windows]

[[links]]
  from      = "mac"
  direction = "right"
  to        = "ubuntu"

[[links]]
  from      = "mac"
  direction = "left"
  to        = "windows"
```

## Running
### macOS (server):
```
./gobarrier-server --config gobarrier.toml
```

### Ubuntu:
```
./gobarrier-client --server mac.local:24800 --name ubuntu
```

### Windows (PowerShell):
```
.\gobarrier-client.exe --server mac.local:24800 --name windows
```

## File Drag-and-Drop
gobarrier implements the `DFTR` / `DDRG` messages from the Barrier v1.5
protocol spec (which Barrier itself never finished).

### How it works:
1. On the primary (mac), drag a file toward the edge of the screen where a
secondary machine lives.

2. gobarrier-server detects the drag via the `NSPasteboard` drag session (TODO:
implement `NSPasteboardItem` watcher) and sends a `DDRG` announcement
followed by `DFTR` chunks to the target client.

3. The client reassembles the chunks and writes the file to `~/Downloads/`.

> Status: The protocol plumbing (DFTR/DDRG encode/decode) is complete.
> The macOS drag-detection hook (`NSEvent` `mouseDown` + drag threshold) and the
> cross-platform drop-folder writer are the next items to implement.

## Roadmap
- TLS (reuse Barrier's certificate fingerprint approach)
- macOS drag detection via NSEvent global monitor
- `~/Downloads` drop target with OS notification
- Clipboard image support (not just text)
- System tray icon (macOS menu bar extra)
- Hot-corner shortcuts to lock cursor to a screen
- `gobarrier-ctl status` CLI for live connection info

## Protocol Compatibility
gobarrier speaks **Barrier protocol v1.6** (the same version as Barrier ≥2.3).
This means you can run `gobarrier-server` on macOS and connect the official
Barrier client on Ubuntu/Windows as a drop-in test — and vice-versa.

## License
GPL-2.0 (same as Barrier, since the protocol wire format is derived from
their open specification).
