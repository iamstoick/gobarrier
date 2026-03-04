//go:build darwin

// Package darwin implements the platform-specific input capture and injection
// layer for macOS using CGEventTap (CoreGraphics) via CGO.
//
// # Why CGEventTap instead of IOKit HID?
//
// CGEventTap intercepts events *before* they reach any application, gives us
// the raw key codes and modifier flags, and lets us suppress events so the
// local cursor doesn't move when we're driving a remote screen.
//
// # Compilation
//
//	CGO_ENABLED=1 go build -tags darwin ./...
//
// The binary must be code-signed with the Accessibility entitlement or the
// user must grant "Input Monitoring" in System Preferences → Privacy.
package darwin

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation -framework AppKit

#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

// Forward declaration of the Go callback (implemented in cgo_bridge.go).
extern CGEventRef goEventCallback(CGEventTapProxy proxy, CGEventType type,
                                   CGEventRef event, void *userInfo);

static CFMachPortRef createEventTap(void) {
    CGEventMask mask =
        CGEventMaskBit(kCGEventMouseMoved)      |
        CGEventMaskBit(kCGEventLeftMouseDown)   |
        CGEventMaskBit(kCGEventLeftMouseUp)     |
        CGEventMaskBit(kCGEventRightMouseDown)  |
        CGEventMaskBit(kCGEventRightMouseUp)    |
        CGEventMaskBit(kCGEventOtherMouseDown)  |
        CGEventMaskBit(kCGEventOtherMouseUp)    |
        CGEventMaskBit(kCGEventScrollWheel)     |
        CGEventMaskBit(kCGEventKeyDown)         |
        CGEventMaskBit(kCGEventKeyUp)           |
        CGEventMaskBit(kCGEventFlagsChanged);

    return CGEventTapCreate(
        kCGSessionEventTap,
        kCGHeadInsertEventTap,
        kCGEventTapOptionDefault,
        mask,
        goEventCallback,
        NULL
    );
}

static void startRunLoop(CFMachPortRef tap) {
    CFRunLoopSourceRef src = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, tap, 0);
    CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopCommonModes);
    CGEventTapEnable(tap, true);
    CFRunLoopRun();
}

// Warp the cursor to (x, y) in screen coordinates.
static void warpCursor(double x, double y) {
    CGWarpMouseCursorPosition(CGPointMake(x, y));
}

// Hide / show the cursor.
static void hideCursor(int hide) {
    if (hide) CGDisplayHideCursor(kCGNullDirectDisplay);
    else      CGDisplayShowCursor(kCGNullDirectDisplay);
}

// Returns current screen size for display 0.
static void primaryScreenSize(int *w, int *h) {
    CGRect r = CGDisplayBounds(CGMainDisplayID());
    *w = (int)r.size.width;
    *h = (int)r.size.height;
}
*/
import "C"

import (
	"log/slog"
	"sync/atomic"
	"unsafe"

	"github.com/yourusername/gobarrier/internal/server"
)

// inputCapture is the singleton capture instance.  Set by Start().
var capture *InputCapture

// InputCapture hooks CGEventTap to capture all HID events.
type InputCapture struct {
	srv         *server.Server
	capturing   atomic.Bool // true when cursor is forwarded to a remote screen
	tap         C.CFMachPortRef
}

// New creates an InputCapture connected to srv.
func New(srv *server.Server) *InputCapture {
	return &InputCapture{srv: srv}
}

// Start installs the event tap and blocks on the CoreFoundation run loop.
// Call in a dedicated goroutine.
func (ic *InputCapture) Start() error {
	capture = ic

	// Query primary screen size.
	var w, h C.int
	C.primaryScreenSize(&w, &h)
	ic.srv.SetPrimarySize(int16(w), int16(h))
	slog.Info("primary screen size", "w", int(w), "h", int(h))

	tap := C.createEventTap()
	if tap == 0 {
		return &Error{"CGEventTapCreate failed — check Accessibility permissions in System Preferences"}
	}
	ic.tap = tap
	C.startRunLoop(tap) // blocks
	return nil
}

// SetCapturing switches the event tap into "forwarding" mode: mouse events
// are suppressed locally and sent to the active secondary screen.
func (ic *InputCapture) SetCapturing(on bool) {
	ic.capturing.Store(on)
	if on {
		C.hideCursor(1)
	} else {
		C.hideCursor(0)
	}
}

// WarpCursor moves the OS cursor without firing a move event.
func (ic *InputCapture) WarpCursor(x, y int16) {
	C.warpCursor(C.double(x), C.double(y))
}

// --------------------------------------------------------------------------
// CGO callback — called on the CoreFoundation thread for every raw event.
// Keep this function lean; dispatch heavy work to Go goroutines.
// --------------------------------------------------------------------------

//export goEventCallback
func goEventCallback(proxy C.CGEventTapProxy, eventType C.CGEventType,
	event C.CGEventRef, _ unsafe.Pointer) C.CGEventRef {

	ic := capture
	if ic == nil {
		return event
	}

	switch eventType {
	case C.kCGEventMouseMoved:
		pt := C.CGEventGetLocation(event)
		ic.srv.RouteMouseMove(int16(pt.x), int16(pt.y))
		if ic.capturing.Load() {
			return nil // suppress local cursor movement
		}

	case C.kCGEventLeftMouseDown:
		ic.srv.RouteMouseDown(1)
		if ic.capturing.Load() {
			return nil
		}
	case C.kCGEventLeftMouseUp:
		ic.srv.RouteMouseUp(1)
		if ic.capturing.Load() {
			return nil
		}
	case C.kCGEventRightMouseDown:
		ic.srv.RouteMouseDown(2)
		if ic.capturing.Load() {
			return nil
		}
	case C.kCGEventRightMouseUp:
		ic.srv.RouteMouseUp(2)
		if ic.capturing.Load() {
			return nil
		}
	case C.kCGEventOtherMouseDown:
		btn := uint8(C.CGEventGetIntegerValueField(event, C.kCGMouseEventButtonNumber)) + 1
		ic.srv.RouteMouseDown(btn)
		if ic.capturing.Load() {
			return nil
		}
	case C.kCGEventOtherMouseUp:
		btn := uint8(C.CGEventGetIntegerValueField(event, C.kCGMouseEventButtonNumber)) + 1
		ic.srv.RouteMouseUp(btn)
		if ic.capturing.Load() {
			return nil
		}

	case C.kCGEventScrollWheel:
		xd := int16(C.CGEventGetIntegerValueField(event, C.kCGScrollWheelEventPointDeltaAxis2))
		yd := int16(C.CGEventGetIntegerValueField(event, C.kCGScrollWheelEventPointDeltaAxis1))
		// Barrier uses 120-unit ticks; CGEvent uses 1-unit deltas.
		ic.srv.RouteMouseWheel(xd*120, yd*120)
		if ic.capturing.Load() {
			return nil
		}

	case C.kCGEventKeyDown:
		keyCode := uint16(C.CGEventGetIntegerValueField(event, C.kCGKeyboardEventKeycode))
		flags := uint16(C.CGEventGetFlags(event))
		ic.srv.RouteKeyDown(cgKeyToBarrier(keyCode), cgFlagsToBarrier(flags), keyCode)
		if ic.capturing.Load() {
			return nil
		}

	case C.kCGEventKeyUp:
		keyCode := uint16(C.CGEventGetIntegerValueField(event, C.kCGKeyboardEventKeycode))
		flags := uint16(C.CGEventGetFlags(event))
		ic.srv.RouteKeyUp(cgKeyToBarrier(keyCode), cgFlagsToBarrier(flags), keyCode)
		if ic.capturing.Load() {
			return nil
		}

	case C.kCGEventFlagsChanged:
		// Modifier-only change — treat as key event.
		keyCode := uint16(C.CGEventGetIntegerValueField(event, C.kCGKeyboardEventKeycode))
		flags := uint16(C.CGEventGetFlags(event))
		ic.srv.RouteKeyDown(cgKeyToBarrier(keyCode), cgFlagsToBarrier(flags), keyCode)
		if ic.capturing.Load() {
			return nil
		}
	}

	return event
}

// --------------------------------------------------------------------------
// Key/modifier translation: macOS virtual key codes → Barrier KeyIDs
// --------------------------------------------------------------------------
// Barrier uses X11 KeySyms as its universal key IDs.  The table below covers
// the most common keys; extend as needed.

func cgKeyToBarrier(vk uint16) uint16 {
	if mapped, ok := cgToBarrierKeyMap[vk]; ok {
		return mapped
	}
	// For printable ASCII keys, Barrier KeyID == Unicode code point.
	// CGEvent key codes are hardware-level; we'd normally use UCKeyTranslate to
	// get the Unicode character.  For now return the raw vk as a placeholder.
	return vk
}

func cgFlagsToBarrier(flags uint16) uint16 {
	// Barrier modifier mask bits (from key_types.h):
	// Shift=0x0001, CapsLock=0x0002, Ctrl=0x0004, Alt=0x0008, Meta=0x0010
	const (
		cgShift   = 0x0002
		cgCtrl    = 0x0004
		cgAlt     = 0x0008 // Option
		cgCommand = 0x0010
		cgCaps    = 0x0001
	)
	var out uint16
	if flags&cgShift != 0   { out |= 0x0001 }
	if flags&cgCaps != 0    { out |= 0x0002 }
	if flags&cgCtrl != 0    { out |= 0x0004 }
	if flags&cgAlt != 0     { out |= 0x0008 }
	if flags&cgCommand != 0 { out |= 0x0010 }
	return out
}

// Partial map: macOS virtual key code → Barrier KeySym (X11).
// Source: <HIToolbox/Events.h> and X11 keysymdef.h
var cgToBarrierKeyMap = map[uint16]uint16{
	// Special / function keys
	0x24: 0xFF0D, // Return      → XK_Return
	0x30: 0xFF09, // Tab         → XK_Tab
	0x33: 0xFF08, // Delete/BS   → XK_BackSpace
	0x35: 0xFF1B, // Escape      → XK_Escape
	0x39: 0xFFE5, // CapsLock    → XK_Caps_Lock
	0x37: 0xFFE9, // Command(L)  → XK_Meta_L
	0x36: 0xFFEA, // Command(R)  → XK_Meta_R
	0x38: 0xFFE1, // Shift(L)    → XK_Shift_L
	0x3C: 0xFFE2, // Shift(R)    → XK_Shift_R
	0x3B: 0xFFE3, // Ctrl(L)     → XK_Control_L
	0x3E: 0xFFE4, // Ctrl(R)     → XK_Control_R
	0x3A: 0xFFE7, // Option(L)   → XK_Alt_L
	0x3D: 0xFFE8, // Option(R)   → XK_Alt_R
	0x31: 0x0020, // Space       → XK_space
	0x7B: 0xFF51, // Left arrow  → XK_Left
	0x7C: 0xFF53, // Right arrow → XK_Right
	0x7D: 0xFF54, // Down arrow  → XK_Down
	0x7E: 0xFF52, // Up arrow    → XK_Up
	0x75: 0xFFFF, // Fwd Delete  → XK_Delete
	0x73: 0xFF50, // Home        → XK_Home
	0x77: 0xFF57, // End         → XK_End
	0x74: 0xFF55, // Page Up     → XK_Prior
	0x79: 0xFF56, // Page Down   → XK_Next
	// Function keys F1–F12
	0x7A: 0xFFBE, 0x78: 0xFFBF, 0x63: 0xFFC0, 0x76: 0xFFC1,
	0x60: 0xFFC2, 0x61: 0xFFC3, 0x62: 0xFFC4, 0x64: 0xFFC5,
	0x65: 0xFFC6, 0x6D: 0xFFC7, 0x67: 0xFFC8, 0x6F: 0xFFC9,
}

// Error is a typed error from the platform layer.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }
