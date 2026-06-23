#!/bin/sh
# patches/apply.sh - apply 9vz's local edits to the vendored Code-Hex/vz code.
#
# Run automatically by `make vendor` after `go mod vendor` (which REWRITES the
# vendor tree, so these edits must be re-applied every time).  Standalone:
#
#     sh patches/apply.sh
#
# We use targeted awk edits rather than a unified diff: a .patch is fussy about
# exact line counts and blank-line representation (a stray empty line makes
# `git apply` report "corrupt patch"), whereas matching on unique source text
# is robust across cosmetic upstream churn.  awk (not sed) is used so the edits
# behave identically on the BSD awk/sed shipped with macOS and on GNU.  Each
# edit is idempotent and fails loudly if the text it expects is missing (so a
# vz version bump can't silently no-op).
#
# Edits to vendor/.../virtualization_view.m for a readable -gui console on a
# Retina Mac, plus an attempt at real native fullscreen:
#
#   1. automaticallyReconfiguresDisplay = YES -> NO.
#      With YES (upstream default on macOS 14+) the view resizes the GUEST
#      display to the view's backing-pixel size, overriding the scanout 9vz
#      configures -- which defeated -scale (guest forced back to native Retina
#      density => tiny font).  NO makes our fixed scanout stick and be upscaled.
#
#   2. Start the window fullscreen.  The windowed view interacts poorly (title
#      bar, resize, partial-screen pointer mapping).  This takes TWO edits:
#        2a. Make the window FullScreenPrimary-eligible in
#            createMainWindowWithTitle: -- without this collection behavior
#            -toggleFullScreen: is silently a no-op.
#        2b. Fire the toggle from applicationDidFinishLaunching AFTER
#            activateIgnoringOtherApps: -- toggling before the app is active
#            and the window is key also no-ops.  (An earlier single edit that
#            toggled from setupGraphicWindow, before activation, is why the VM
#            stayed windowed despite the patch being "applied".)
#      Honors 9VZ_NOFULLSCREEN=1 to keep the old windowed behavior.
#
#   3. Let AppKit own the run loop ([super run]) instead of VZApplication's
#      hand-rolled nextEventMatchingMask do/while, with teardown moved to
#      [super stop:] + a posted dummy event.  Edits 2a/2b only got us from
#      "nothing happens" to "wrong thing happens": the toggle now fires but
#      only MAXIMIZES the window over the current desktop instead of moving it
#      to its own fullscreen Space.  The custom loop not driving the native
#      Space transition is the leading suspect; this edit is the candidate fix.
#      STATUS: UNTESTED on hardware -- real fullscreen Space not yet confirmed,
#      and the new teardown path must be verified to exit cleanly.  See the
#      working notes (/usr/dave/9vz-audio-and-fullscreen.md, section (a)).

set -e

f="vendor/github.com/Code-Hex/vz/v3/virtualization_view.m"

if [ ! -f "$f" ]; then
	echo "patches/apply.sh: $f not found (run 'go mod vendor' first)" >&2
	exit 1
fi

# --- Edit 1: automaticallyReconfiguresDisplay -> NO -------------------------
# Done with awk (not sed) because BSD sed on macOS does not interpret \n in the
# replacement as a newline; awk behaves identically on BSD and GNU.  We rewrite
# the "= YES;" line in place to a "= NO;" line plus a short 9vz note, preserving
# the original 8-space indentation.
if grep -q 'automaticallyReconfiguresDisplay = NO;' "$f"; then
	echo "  [skip] automaticallyReconfiguresDisplay already NO"
elif grep -q 'automaticallyReconfiguresDisplay = YES;' "$f"; then
	awk '
		!done && /automaticallyReconfiguresDisplay = YES;/ {
			print "        // 9vz: keep OFF so our configured scanout sticks and is"
			print "        // upscaled (YES resizes the guest display to native Retina"
			print "        // density, overriding the scanout and defeating -scale)."
			print "        view.automaticallyReconfiguresDisplay = NO;"
			done = 1
			next
		}
		{ print }
	' "$f" > "$f.tmp"
	mv "$f.tmp" "$f"
	echo "  [ok]   automaticallyReconfiguresDisplay -> NO"
else
	echo "patches/apply.sh: could not find automaticallyReconfiguresDisplay assignment in $f" >&2
	echo "  (vz layout changed?  re-check this patch against the new version)" >&2
	exit 1
fi

# --- Edit 2a: make the window FullScreenPrimary-eligible -------------------
# Anchor on the unique "[window setTitle:title];" line in
# createMainWindowWithTitle: and add the collection behavior right after it.
# Without FullScreenPrimary, -toggleFullScreen: silently no-ops and the VM
# stays windowed -- this was the real reason the old fullscreen patch did
# nothing.
if grep -q 'NSWindowCollectionBehaviorFullScreenPrimary' "$f"; then
	echo "  [skip] fullscreen collection behavior already present"
elif grep -q '\[window setTitle:title\];' "$f"; then
	awk '
		!done && /\[window setTitle:title\];/ {
			print
			print "    // 9vz: make the window eligible for the native (green-button)"
			print "    // fullscreen transition.  Without FullScreenPrimary in the"
			print "    // collection behavior, -toggleFullScreen: is silently a no-op,"
			print "    // which is why the start-fullscreen patch appeared to do nothing"
			print "    // and the VM stayed windowed."
			print "    [window setCollectionBehavior:[window collectionBehavior] | NSWindowCollectionBehaviorFullScreenPrimary];"
			done = 1
			next
		}
		{ print }
	' "$f" > "$f.tmp"
	mv "$f.tmp" "$f"
	echo "  [ok]   fullscreen collection behavior added"
else
	echo "patches/apply.sh: could not find [window setTitle:title] in $f" >&2
	echo "  (vz layout changed?  re-check this patch against the new version)" >&2
	exit 1
fi

# --- Edit 2b: toggle fullscreen after activation ---------------------------
# Anchor on the unique "[NSApp activateIgnoringOtherApps:YES];" line in
# applicationDidFinishLaunching and insert the toggle right after it.  This
# MUST come after activation: a window that is not yet key in an active,
# regular-policy app ignores -toggleFullScreen:.
if grep -q '9vz: start the VM window fullscreen' "$f"; then
	echo "  [skip] fullscreen-on-launch toggle already present"
elif grep -q '\[NSApp activateIgnoringOtherApps:YES\];' "$f"; then
	awk '
		!done && /\[NSApp activateIgnoringOtherApps:YES\];/ {
			print
			print ""
			print "    // 9vz: start the VM window fullscreen.  The windowed view interacts"
			print "    // poorly (title bar, resize, partial-screen pointer mapping).  This"
			print "    // must run AFTER the activation calls above -- a window that is not"
			print "    // yet key in an active, regular-policy app ignores -toggleFullScreen:,"
			print "    // which was why an earlier toggle from setupGraphicWindow appeared to"
			print "    // do nothing.  The window is made FullScreenPrimary-eligible in"
			print "    // createMainWindowWithTitle:.  Honors 9VZ_NOFULLSCREEN=1 to stay windowed."
			print "    if (getenv(\"9VZ_NOFULLSCREEN\") == NULL) {"
			print "        dispatch_async(dispatch_get_main_queue(), ^{"
			print "            if (([self->_window styleMask] & NSWindowStyleMaskFullScreen) == 0)"
			print "                [self->_window toggleFullScreen:nil];"
			print "        });"
			print "    }"
			done = 1
			next
		}
		{ print }
	' "$f" > "$f.tmp"
	mv "$f.tmp" "$f"
	echo "  [ok]   fullscreen-on-launch toggle inserted"
else
	echo "patches/apply.sh: could not find activateIgnoringOtherApps in $f" >&2
	echo "  (vz layout changed?  re-check this patch against the new version)" >&2
	exit 1
fi

# --- Edit 3: let AppKit own the run loop -----------------------------------
# VZApplication overrides -run with a hand-rolled nextEventMatchingMask
# do/while.  That custom loop does NOT drive the Core Animation / run-loop
# machinery behind the native fullscreen *Space* transition, so
# -toggleFullScreen: only resizes the window to cover the screen instead of
# moving it onto its own fullscreen Space (the "maximized window over the
# desktop" bug).  Replace the body of -run with [super run] and end it from
# -terminate: via [super stop:] + a posted dummy event.
#
# This is a whole-method rewrite, so rather than awk-inserting we substitute
# the entire @implementation VZApplication block.  We detect the original by
# its unique "shouldKeepRunning = YES;" + "nextEventMatchingMask" do/while and
# the patched form by the "9vz: use AppKit's own run loop" marker comment.
if grep -q "9vz: use AppKit's own run loop" "$f"; then
	echo "  [skip] VZApplication run-loop rewrite already present"
elif grep -q 'nextEventMatchingMask:NSEventMaskAny' "$f"; then
	awk '
		# Start of the original -run method body we want to replace.
		!done && $0 ~ /^- \(void\)run$/ {
			inrun = 1
		}
		inrun && /shouldKeepRunning = YES;/ {
			# Emit the replacement -run body, then swallow the original
			# do/while up to and including its closing "}" line.
			print "    // 9vz: use AppKit'\''s own run loop ([super run]) instead of a hand-rolled"
			print "    // nextEventMatchingMask do/while.  The native fullscreen *Space*"
			print "    // transition (-toggleFullScreen:) is animated and driven by Core Animation"
			print "    // transactions and run-loop observers on the main thread; a bare"
			print "    // nextEventMatchingMask loop in NSDefaultRunLoopMode services events but"
			print "    // does NOT drive that machinery, so the window only got resized to cover"
			print "    // the screen instead of moving onto its own fullscreen Space.  Letting"
			print "    // AppKit own the loop makes -toggleFullScreen: do the real transition."
			print "    // Teardown is handled in -terminate: via [super stop:] + a dummy event."
			print "    @autoreleasepool {"
			print "        shouldKeepRunning = YES;"
			print "        [super run];"
			print "    }"
			swallow = 1
			next
		}
		# Swallow the original do { ... } while (shouldKeepRunning); body.
		swallow {
			if (/} while \(shouldKeepRunning\);/) {
				swallow = 0
				inrun = 0
				next
			}
			next
		}
		# Replace the postEvent teardown in -terminate: with stop: + dummy event.
		!tdone && /\[self postEvent:self.currentEvent atStart:NO\];/ {
			print "    // 9vz: -[NSApplication stop:] only takes effect after the current event is"
			print "    // handled, so post a dummy event to wake the run loop if it is idle in"
			print "    // nextEventMatchingMask.  Together these end [super run] cleanly."
			print "    [super stop:sender];"
			print "    NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined"
			print "                                       location:NSZeroPoint"
			print "                                  modifierFlags:0"
			print "                                      timestamp:0"
			print "                                   windowNumber:0"
			print "                                        context:nil"
			print "                                        subtype:0"
			print "                                          data1:0"
			print "                                          data2:0];"
			print "    [self postEvent:wake atStart:YES];"
			tdone = 1
			done = 1
			next
		}
		{ print }
	' "$f" > "$f.tmp"
	mv "$f.tmp" "$f"
	echo "  [ok]   VZApplication run-loop rewrite applied"
else
	echo "patches/apply.sh: could not find VZApplication nextEventMatchingMask loop in $f" >&2
	echo "  (vz layout changed?  re-check this patch against the new version)" >&2
	exit 1
fi

echo "patches/apply.sh: done."
