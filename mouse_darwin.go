//go:build darwin

// Modifier-click mouse button remapping for the -gui window.
//
// Apple's VZVirtualMachineView turns host AppKit mouse events into USB
// HID pointer reports for the guest.  A bare laptop trackpad only gives
// us a single (left) button, which the guest sees as Plan 9 button 1.
// There is no host-side gesture for buttons 2 and 3, so rio's chords are
// unreachable -- exactly the problem drawterm's Cocoa backend solves by
// treating Option-click as button 2 and Command-click as button 3.
//
// We reproduce that here without touching the vendored vz module.  An
// NSEvent local monitor sits in front of the view: when a left-button
// event (down/dragged/up) arrives carrying Command or Option, we build a
// replacement NSEvent of the matching kind and hand THAT to AppKit
// instead.  VZVirtualMachineView then emits the right HID button:
//
//	Command + left  -> synthesized right  mouse -> HID right  -> Plan 9 button 3
//	Option  + left  -> synthesized middle mouse -> HID middle -> Plan 9 button 2
//
// Command wins if both are held (matches drawterm).  The whole down/
// drag/up sequence is steered by latching which synthetic button a press
// began with, so a drag that started as button 3 stays button 3 even if
// the modifier is released mid-drag -- otherwise the guest would see a
// button-up on a button it never saw go down.
//
// Why CGEvent and not just -[NSEvent mouseEventWithType:...]: that
// convenience constructor cannot set an arbitrary buttonNumber, so a
// synthesized NSEventTypeOtherMouseDown always reports button 0, never
// the middle (button 2) that the guest needs.  We therefore build the
// replacement at the CoreGraphics layer -- where the button index is
// explicit -- and wrap it back into an NSEvent for AppKit to dispatch.
package main

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Cocoa -framework CoreGraphics
#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CoreGraphics.h>
#include <stdlib.h>

// Which synthetic button a left-press latched onto, so the matching
// drag/up stays consistent even if the modifier changes mid-gesture.
// 0 = none (pass through), 2 = middle (button 2), 3 = right (button 3).
static int latchedButton = 0;

// Escape hatch: if the host/guest button convention turns out swapped
// on your setup (Command landing on 2 and Option on 3), set the
// environment variable 9VZ_SWAPMODBUTTONS=1 to flip them, no rebuild
// of the logic required.  Resolved once at install time.
static int swapModButtons = 0;

// Build a replacement NSEvent for the given target button and phase,
// preserving the original's screen location, modifiers and timing.
// phase: 0 = down, 1 = dragged, 2 = up.  target: 2 = middle, 3 = right.
static NSEvent *
remap(NSEvent *e, int target, int phase)
{
    CGMouseButton cgButton;
    CGEventType cgType;

    if (target == 3) {
        cgButton = kCGMouseButtonRight;
        cgType = phase == 0 ? kCGEventRightMouseDown
               : phase == 2 ? kCGEventRightMouseUp
                            : kCGEventRightMouseDragged;
    } else {
        // Middle button (index 2) is "other" at the CG layer.
        cgButton = kCGMouseButtonCenter;
        cgType = phase == 0 ? kCGEventOtherMouseDown
               : phase == 2 ? kCGEventOtherMouseUp
                            : kCGEventOtherMouseDragged;
    }

    // CGEvent wants screen coordinates with a top-left origin.  The
    // original NSEvent carries window-local coordinates with a
    // bottom-left origin, so convert through the event's window.
    NSWindow *win = [e window];
    NSPoint pWin = [e locationInWindow];
    NSPoint pScreen = win ? [win convertPointToScreen:pWin] : pWin;
    CGFloat screenH = NSMaxY([[NSScreen mainScreen] frame]);
    CGPoint cgPoint = CGPointMake(pScreen.x, screenH - pScreen.y);

    CGEventRef cg = CGEventCreateMouseEvent(NULL, cgType, cgPoint, cgButton);
    if (cg == NULL)
        return e;
    // Carry the original modifier flags so the guest still sees e.g.
    // Command held, and so click-counting stays sane.
    CGEventSetFlags(cg, (CGEventFlags)[e modifierFlags]);
    NSEvent *out = [NSEvent eventWithCGEvent:cg];
    CFRelease(cg);
    return out ? out : e;
}

// Install the local monitor.  Called once, on the AppKit main thread,
// just before [NSApp run] takes over.
static void
installMouseRemap(void)
{
    const char *sw = getenv("9VZ_SWAPMODBUTTONS");
    swapModButtons = (sw != NULL && sw[0] != '\0' && sw[0] != '0');

    NSEventMask mask = NSEventMaskLeftMouseDown
        | NSEventMaskLeftMouseUp
        | NSEventMaskLeftMouseDragged;

    [NSEvent addLocalMonitorForEventsMatchingMask:mask
                                          handler:^NSEvent *(NSEvent *e) {
        NSEventType t = [e type];
        NSEventModifierFlags m = [e modifierFlags];
        int target, phase;

        if (t == NSEventTypeLeftMouseDown) {
            int cmdBtn = swapModButtons ? 2 : 3;
            int optBtn = swapModButtons ? 3 : 2;
            if (m & NSEventModifierFlagCommand)
                target = cmdBtn;
            else if (m & NSEventModifierFlagOption)
                target = optBtn;
            else
                target = 0;
            latchedButton = target;
            phase = 0;
        } else if (t == NSEventTypeLeftMouseUp) {
            target = latchedButton;
            latchedButton = 0;
            phase = 2;
        } else { // dragged
            target = latchedButton;
            phase = 1;
        }

        if (target == 0)
            return e; // ordinary left button: pass through untouched
        return remap(e, target, phase);
    }];
}
*/
import "C"

// installMouseRemap wires up the Command/Option-click remapper.  It must
// run on the OS main thread before StartGraphicApplication enters the
// AppKit run loop (the caller is already pinned there by init()).
func installMouseRemap() {
	C.installMouseRemap()
}
