//go:build linux

// Package linux implements the client-side input injection layer for Linux
// using the XTEST extension (XSendEvent alternative that actually works).
//
// # Requirements
//
//   - X11 with XTEST extension (virtually universal on desktop Linux)
//   - libxtst-dev package:  sudo apt install libxtst-dev
//
// # Compilation
//
//	CGO_ENABLED=1 go build -tags linux ./...
package linux

/*
#cgo LDFLAGS: -lX11 -lXtst

#include <X11/Xlib.h>
#include <X11/extensions/XTest.h>
#include <X11/keysym.h>
#include <stdlib.h>
#include <string.h>

static Display *gDisplay = NULL;

static int openDisplay(const char *name) {
    gDisplay = XOpenDisplay(name);
    return gDisplay != NULL ? 1 : 0;
}

static void closeDisplay(void) {
    if (gDisplay) { XCloseDisplay(gDisplay); gDisplay = NULL; }
}

static void moveCursorAbs(int x, int y) {
    XTestFakeMotionEvent(gDisplay, -1, x, y, CurrentTime);
    XFlush(gDisplay);
}

static void moveCursorRel(int dx, int dy) {
    XTestFakeRelativeMotionEvent(gDisplay, dx, dy, CurrentTime);
    XFlush(gDisplay);
}

static void mouseButton(int button, int press) {
    XTestFakeButtonEvent(gDisplay, button, press, CurrentTime);
    XFlush(gDisplay);
}

static void keyEvent(KeySym ks, int press) {
    KeyCode kc = XKeysymToKeycode(gDisplay, ks);
    if (kc == 0) return;
    XTestFakeKeyEvent(gDisplay, kc, press, CurrentTime);
    XFlush(gDisplay);
}

static void getScreenSize(int *w, int *h) {
    Screen *s = DefaultScreenOfDisplay(gDisplay);
    *w = WidthOfScreen(s);
    *h = HeightOfScreen(s);
}

static void setCursorVisible(int visible) {
    // Cursor hiding on X11 requires creating a blank cursor — omitted for
    // brevity; a real implementation uses XFixesHideCursor.
    (void)visible;
}

static void warpPointer(int x, int y) {
    XWarpPointer(gDisplay, None, DefaultRootWindow(gDisplay), 0,0,0,0, x, y);
    XFlush(gDisplay);
}
*/
import "C"

import (
	"fmt"
	"log/slog"
)

// Injector injects input events into the local X11 display.
type Injector struct {
	display string
}

// New creates a new Injector.  display is the X display string, e.g. ":0".
// Pass "" to use the DISPLAY environment variable.
func New(display string) (*Injector, error) {
	d := C.CString(display)
	defer C.free(unsafe.Pointer(d))
	if C.openDisplay(d) == 0 {
		return nil, fmt.Errorf("XOpenDisplay(%q) failed", display)
	}
	return &Injector{display: display}, nil
}

// Close releases X11 resources.
func (inj *Injector) Close() { C.closeDisplay() }

// ScreenSize returns the primary screen dimensions.
func (inj *Injector) ScreenSize() (w, h int) {
	var cw, ch C.int
	C.getScreenSize(&cw, &ch)
	return int(cw), int(ch)
}

// MoveCursorAbs moves the cursor to an absolute position.
func (inj *Injector) MoveCursorAbs(x, y int16) {
	C.moveCursorAbs(C.int(x), C.int(y))
}

// MoveCursorRel moves the cursor by a relative delta.
func (inj *Injector) MoveCursorRel(dx, dy int16) {
	C.moveCursorRel(C.int(dx), C.int(dy))
}

// MouseDown presses a mouse button (1=left, 2=middle, 3=right).
func (inj *Injector) MouseDown(btn uint8) {
	C.mouseButton(C.int(btn), 1)
}

// MouseUp releases a mouse button.
func (inj *Injector) MouseUp(btn uint8) {
	C.mouseButton(C.int(btn), 0)
}

// MouseWheel sends a scroll event.  XTest maps button 4/5 to vertical
// scroll and 6/7 to horizontal scroll.
func (inj *Injector) MouseWheel(xDelta, yDelta int16) {
	if yDelta > 0 {
		for i := int16(0); i < yDelta/120; i++ {
			C.mouseButton(4, 1)
			C.mouseButton(4, 0)
		}
	} else if yDelta < 0 {
		for i := int16(0); i < -yDelta/120; i++ {
			C.mouseButton(5, 1)
			C.mouseButton(5, 0)
		}
	}
	if xDelta > 0 {
		for i := int16(0); i < xDelta/120; i++ {
			C.mouseButton(6, 1)
			C.mouseButton(6, 0)
		}
	} else if xDelta < 0 {
		for i := int16(0); i < -xDelta/120; i++ {
			C.mouseButton(7, 1)
			C.mouseButton(7, 0)
		}
	}
}

// KeyDown injects a key press.  keyID is a Barrier KeySym (= X11 KeySym).
func (inj *Injector) KeyDown(keyID uint16) {
	C.keyEvent(C.KeySym(keyID), 1)
}

// KeyUp injects a key release.
func (inj *Injector) KeyUp(keyID uint16) {
	C.keyEvent(C.KeySym(keyID), 0)
}

// WarpCursor moves the cursor without generating a motion event.
func (inj *Injector) WarpCursor(x, y int16) {
	C.warpPointer(C.int(x), C.int(y))
}

// Log helper.
var _ = slog.Info

// Needed for CGO unsafe.Pointer usage.
import "unsafe"
