/*
 * pbd_darwin.h - NSPasteboard bridge for 9vz pasteboard daemon (pbd).
 *
 * Exposes a minimal C API over NSPasteboard.general so pbd.go can call
 * into AppKit without Objective-C syntax in the Go file.  Each function
 * manages its own @autoreleasepool internally and may be called from any
 * thread; NSPasteboard does not require the main thread.
 *
 * pbd.go serialises all calls onto a single OS-thread-locked goroutine
 * (the "Cocoa goroutine") for consistent memory-management semantics under
 * -fno-objc-arc and to provide the atomicity guarantees described in
 * devsock.md §5.3.
 */

#pragma once

#include <stdint.h>

/*
 * pbdGetChangeCount returns NSPasteboard.general.changeCount.
 */
int64_t pbdGetChangeCount(void);

/*
 * pbdGetString returns the UTF-8 string value of the general pasteboard,
 * or NULL if the pasteboard does not contain a plain-text item.
 * The returned pointer is a malloc'd C string; the caller must free() it.
 */
char *pbdGetString(void);

/*
 * pbdSetString writes a UTF-8 string to the general pasteboard and returns
 * the resulting changeCount (read immediately after the write, before any
 * other caller can observe the new count).  Returns -1 on failure.
 * Pass len==0 / utf8==NULL to write an empty string.
 */
int64_t pbdSetString(const char *utf8, int len);
