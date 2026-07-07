/*
 * pbd_darwin.m - NSPasteboard bridge for pbd, the 9vz pasteboard daemon.
 *
 * Implements the three C functions declared in pbd_darwin.h.  Each manages
 * its own @autoreleasepool so callers need not wrap calls themselves.
 * Compiled as Objective-C by cgo (the .m extension selects the ObjC
 * compiler; the -fno-objc-arc flag from pbd.go's cgo CFLAGS applies).
 */

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#include "pbd_darwin.h"
#include <stdlib.h>
#include <string.h>

int64_t pbdGetChangeCount(void)
{
    @autoreleasepool {
        NSPasteboard *pb = [NSPasteboard generalPasteboard];
        return (int64_t)[pb changeCount];
    }
}

char *pbdGetString(void)
{
    @autoreleasepool {
        NSPasteboard *pb = [NSPasteboard generalPasteboard];
        NSString *str = [pb stringForType:NSPasteboardTypeString];
        if (str == nil) {
            return NULL;
        }
        const char *utf8 = [str UTF8String];
        if (utf8 == NULL) {
            return NULL;
        }
        /* dup so the caller owns the memory outside the pool */
        return strdup(utf8);
    }
}

int64_t pbdSetString(const char *utf8, int len)
{
    @autoreleasepool {
        NSPasteboard *pb = [NSPasteboard generalPasteboard];
        NSString *str;
        if (len == 0 || utf8 == NULL) {
            str = @"";
        } else {
            str = [[NSString alloc] initWithBytes:utf8
                                          length:(NSUInteger)len
                                        encoding:NSUTF8StringEncoding];
            if (str == nil) {
                return -1;
            }
        }
        [pb clearContents];
        BOOL ok = [pb setString:str forType:NSPasteboardTypeString];
        if (len > 0 && utf8 != NULL) {
            [str release];
        }
        if (!ok) {
            return -1;
        }
        return (int64_t)[pb changeCount];
    }
}
