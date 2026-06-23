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
# Two edits, both to vendor/.../virtualization_view.m, both needed for a
# readable -gui console on a Retina Mac:
#
#   1. automaticallyReconfiguresDisplay = YES -> NO.
#      With YES (upstream default on macOS 14+) the view resizes the GUEST
#      display to the view's backing-pixel size, overriding the scanout 9vz
#      configures -- which defeated -scale (guest forced back to native Retina
#      density => tiny font).  NO makes our fixed scanout stick and be upscaled.
#
#   2. Start the window fullscreen (toggleFullScreen: after it is shown).
#      The windowed view interacts poorly (title bar, resize, partial-screen
#      pointer mapping).  Honors 9VZ_NOFULLSCREEN=1 to keep the old behavior.

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

# --- Edit 2: start fullscreen ----------------------------------------------
# Anchor on the unique "makeKeyAndOrderFront" call in setupGraphicWindow and
# insert the fullscreen toggle right after it.
if grep -q '9vz: start the VM window fullscreen' "$f"; then
	echo "  [skip] fullscreen-on-launch already present"
elif grep -q '\[_window makeKeyAndOrderFront:nil\];' "$f"; then
	# Use awk to insert a block after the first matching line only.
	awk '
		!done && /\[_window makeKeyAndOrderFront:nil\];/ {
			print
			print ""
			print "    // 9vz: start the VM window fullscreen.  The windowed view interacts"
			print "    // poorly (title bar, resize, partial-screen pointer mapping).  Toggle"
			print "    // on the main queue after the window is on screen.  Honors"
			print "    // 9VZ_NOFULLSCREEN=1 to keep the old windowed behavior."
			print "    if (getenv(\"9VZ_NOFULLSCREEN\") == NULL) {"
			print "        dispatch_async(dispatch_get_main_queue(), ^{"
			print "            if (([_window styleMask] & NSWindowStyleMaskFullScreen) == 0)"
			print "                [_window toggleFullScreen:nil];"
			print "        });"
			print "    }"
			done = 1
			next
		}
		{ print }
	' "$f" > "$f.tmp"
	mv "$f.tmp" "$f"
	echo "  [ok]   fullscreen-on-launch inserted"
else
	echo "patches/apply.sh: could not find makeKeyAndOrderFront in $f" >&2
	echo "  (vz layout changed?  re-check this patch against the new version)" >&2
	exit 1
fi

echo "patches/apply.sh: done."
