// pbd.go — pasteboard bridge daemon for 9vz.
//
// pbd listens on vsock port 9001 for a single guest connection and
// bidirectionally synchronises the macOS NSPasteboard with the guest's
// snarf buffer.  The guest side is snarflink/snarffs (§5.4 of devsock.md).
//
// Wire format (§5.2):
//
//	frame := len[8 hex] SP type[4] SP data[len]
//
// Types: SNRF (guest→host update), PSTE (host→guest update),
//
//	PING (keepalive request), PONG (keepalive reply).
//
// Loop prevention (§5.3): the Cocoa goroutine (the only goroutine that
// touches NSPasteboard) stores the SHA-256 hash and changeCount of the
// last value it wrote.  When the poll op returns, it compares the new
// changeCount and content hash against that stored state; if they match,
// it reports "suppress" so the poller does not echo the content back to
// the guest as a spurious PSTE.
//
// Concurrency model:
//   - One accept goroutine, started once and runs for the VM lifetime.
//   - Per-connection: one reader goroutine, one writer goroutine (fed via a
//     buffered channel), one poller goroutine, one keepalive goroutine.
//   - A single "Cocoa goroutine" is locked to an OS thread and serialises
//     ALL NSPasteboard calls.  Other goroutines communicate with it via a
//     typed channel.  Because the goroutine is serial, "write pasteboard +
//     record changeCount" is atomic with respect to subsequent poll reads.
//   - A stop channel (closed on VM stop) signals all goroutines to exit.
package main

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework AppKit
#include "pbd_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
)

const (
	pbdPort         = uint32(9001)
	pbdMaxPayload   = 1 << 20 // 1 MiB
	pbdPollInterval = 250 * time.Millisecond

	// Keepalive: send PING every 30 s, expect PONG within 10 s.
	pbdPingInterval = 30 * time.Second
	pbdPongTimeout  = 10 * time.Second
	// ReadDeadline is generous: at most one PING interval + PONG timeout + slack.
	pbdReadDeadline  = pbdPingInterval + pbdPongTimeout + 5*time.Second
	pbdWriteDeadline = 10 * time.Second
)

// ---------------------------------------------------------------------------
// Cocoa goroutine — serialised NSPasteboard access
// ---------------------------------------------------------------------------

// cocoaOpKind identifies which pasteboard operation to perform.
type cocoaOpKind int

const (
	// cocoaOpPoll reads the current changeCount and string, returning them
	// together with a "suppress" flag that is true when the content matches
	// the last value we wrote (loop prevention §5.3).
	cocoaOpPoll cocoaOpKind = iota

	// cocoaOpSet writes a new string to the pasteboard and records the
	// resulting changeCount and its hash for loop suppression.
	cocoaOpSet

	// cocoaOpReset clears the suppression bookkeeping (called on reconnect,
	// §5.3: "A reconnect resets notification bookkeeping").
	cocoaOpReset
)

type cocoaOp struct {
	kind   cocoaOpKind
	data   []byte      // cocoaOpSet only: the text to write
	result chan cocoaResult
}

type cocoaResult struct {
	// cocoaOpPoll
	changeCount int64
	text        string
	hasText     bool
	suppress    bool // true: content matches last write → skip PSTE

	// cocoaOpSet
	ok bool // write succeeded
}

// cocoaGoroutine is the single goroutine that calls NSPasteboard.  It is
// locked to an OS thread so Objective-C object lifetimes are predictable
// under -fno-objc-arc.
type cocoaGoroutine struct {
	ch chan cocoaOp

	// Suppression state: the changeCount returned by the last pbdSetString
	// call AND the SHA-256 of the text we wrote.  Both are set atomically
	// (from the goroutine's own perspective) right before the result of
	// cocoaOpSet is sent back.  Subsequent cocoaOpPoll calls that see the
	// same changeCount or the same content hash treat the result as a
	// bridge-originated change and set suppress=true.
	lastSetCount int64
	lastSetHash  string
}

func newCocoaGoroutine() *cocoaGoroutine {
	cg := &cocoaGoroutine{
		ch:           make(chan cocoaOp, 8),
		lastSetCount: -1,
	}
	go cg.run()
	return cg
}

func (cg *cocoaGoroutine) run() {
	runtime.LockOSThread()
	// NSPasteboard does not require the main thread, but locking to a single
	// OS thread ensures autorelease-pool scope is consistent.
	for op := range cg.ch {
		switch op.kind {
		case cocoaOpPoll:
			cc := int64(C.pbdGetChangeCount())
			cstr := C.pbdGetString()
			res := cocoaResult{changeCount: cc}
			if cstr != nil {
				txt := C.GoString(cstr)
				C.free(unsafe.Pointer(cstr))
				res.text = txt
				res.hasText = true
				// Suppress if this is our own last write.
				if cc == cg.lastSetCount {
					res.suppress = true
				} else if cg.lastSetHash != "" && pbdHashOf([]byte(txt)) == cg.lastSetHash {
					res.suppress = true
				}
			}
			op.result <- res

		case cocoaOpSet:
			var newCC int64
			if len(op.data) == 0 {
				newCC = int64(C.pbdSetString(nil, 0))
			} else {
				cdata := C.CBytes(op.data)
				newCC = int64(C.pbdSetString((*C.char)(cdata), C.int(len(op.data))))
				C.free(cdata)
			}
			res := cocoaResult{ok: newCC >= 0}
			if res.ok {
				cg.lastSetCount = newCC
				cg.lastSetHash = pbdHashOf(op.data)
			}
			op.result <- res

		case cocoaOpReset:
			cg.lastSetCount = -1
			cg.lastSetHash = ""
			op.result <- cocoaResult{ok: true}
		}
	}
}

// poll asks the Cocoa goroutine for the current pasteboard state.
func (cg *cocoaGoroutine) poll(stopCh <-chan struct{}) (cocoaResult, bool) {
	res := make(chan cocoaResult, 1)
	select {
	case cg.ch <- cocoaOp{kind: cocoaOpPoll, result: res}:
	case <-stopCh:
		return cocoaResult{}, false
	}
	r := <-res
	return r, true
}

// set writes text to the pasteboard via the Cocoa goroutine.
func (cg *cocoaGoroutine) set(text []byte, stopCh <-chan struct{}) bool {
	res := make(chan cocoaResult, 1)
	select {
	case cg.ch <- cocoaOp{kind: cocoaOpSet, data: text, result: res}:
	case <-stopCh:
		return false
	}
	r := <-res
	return r.ok
}

// reset clears suppression bookkeeping; called at the start of each new
// connection (§5.3: "A reconnect resets notification bookkeeping").
func (cg *cocoaGoroutine) reset(stopCh <-chan struct{}) {
	res := make(chan cocoaResult, 1)
	select {
	case cg.ch <- cocoaOp{kind: cocoaOpReset, result: res}:
	case <-stopCh:
		return
	}
	<-res
}

// stop closes the Cocoa goroutine's channel, ending its loop.
func (cg *cocoaGoroutine) stop() {
	close(cg.ch)
}

// pbdHashOf returns the lowercase hex SHA-256 of data.
func pbdHashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// pbdState — top-level bridge state, one per 9vz process
// ---------------------------------------------------------------------------

type pbdState struct {
	listener *vz.VirtioSocketListener
	cocoa    *cocoaGoroutine
	stopCh   chan struct{}
	stopOnce sync.Once
}

// startPBD is called from main after NewVirtualMachine but before vm.Start.
// Failure is fatal (log.Fatalf) matching the -audio/-gui pattern.
func startPBD(vm *vz.VirtualMachine) *pbdState {
	devs := vm.SocketDevices()
	if len(devs) == 0 {
		log.Fatalf("pbd: no vsock device found (VirtioSocketDeviceConfiguration missing from config?)")
	}
	listener, err := devs[0].Listen(pbdPort)
	if err != nil {
		log.Fatalf("pbd: listen on vsock port %d: %v", pbdPort, err)
	}

	s := &pbdState{
		listener: listener,
		cocoa:    newCocoaGoroutine(),
		stopCh:   make(chan struct{}),
	}
	go s.runAcceptLoop()
	log.Printf("pbd: pasteboard bridge listening on vsock port %d", pbdPort)
	return s
}

// stop shuts down the bridge cleanly.  Called from runStateLoop on VM stop.
func (s *pbdState) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.listener.Close()
		s.cocoa.stop()
	})
}

// ---------------------------------------------------------------------------
// Accept loop
// ---------------------------------------------------------------------------

func (s *pbdState) runAcceptLoop() {
	var connActive atomic.Bool

	// acceptResult carries the result of one AcceptVirtioSocketConnection call.
	type acceptResult struct {
		conn *vz.VirtioSocketConnection
		err  error
	}

	for {
		// Run Accept in a goroutine so we can also watch stopCh.
		// The goroutine is intentionally leaked on VM stop: the process calls
		// os.Exit shortly after stopCh is closed, so any blocked accept will
		// be cleaned up by the OS.
		ch := make(chan acceptResult, 1)
		go func() {
			c, err := s.listener.AcceptVirtioSocketConnection()
			ch <- acceptResult{c, err}
		}()

		var res acceptResult
		select {
		case <-s.stopCh:
			return
		case res = <-ch:
		}

		if res.err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("pbd: accept error: %v", res.err)
				select {
				case <-time.After(100 * time.Millisecond):
				case <-s.stopCh:
					return
				}
				continue
			}
		}

		if !connActive.CompareAndSwap(false, true) {
			log.Printf("pbd: rejecting second connection (only one active at a time)")
			res.conn.Close()
			continue
		}

		go func(c *vz.VirtioSocketConnection) {
			defer connActive.Store(false)
			s.handleConnection(c)
			log.Printf("pbd: connection closed; ready to accept next")
		}(res.conn)
	}
}

// ---------------------------------------------------------------------------
// Frame writer
// ---------------------------------------------------------------------------

// pbdFrameWriter serialises frame writes onto the connection.
// One goroutine drains sendCh; all others queue frames via send().
type pbdFrameWriter struct {
	conn   net.Conn
	sendCh chan []byte
}

func newPBDFrameWriter(conn net.Conn) *pbdFrameWriter {
	return &pbdFrameWriter{
		conn:   conn,
		sendCh: make(chan []byte, 32),
	}
}

func (fw *pbdFrameWriter) run() {
	for buf := range fw.sendCh {
		_ = fw.conn.SetWriteDeadline(time.Now().Add(pbdWriteDeadline))
		if _, err := fw.conn.Write(buf); err != nil {
			// Write error; return.  Remaining items in sendCh will be dropped
			// when the channel is eventually closed by fw.close().  Senders
			// use a non-blocking select so they never block on a full channel.
			return
		}
	}
}

// send enqueues a frame for writing.  Non-blocking: drops if channel is full
// (connection is likely wedged; the reader will detect and close it).
func (fw *pbdFrameWriter) send(typ [4]byte, data []byte) {
	hdr := fmt.Sprintf("%08x %s ", len(data), string(typ[:]))
	buf := make([]byte, len(hdr)+len(data))
	copy(buf, hdr)
	copy(buf[len(hdr):], data)
	select {
	case fw.sendCh <- buf:
	default:
		// Queue full; drop.
	}
}

// close shuts down the frame writer.
func (fw *pbdFrameWriter) close() {
	// Drain and close.
	close(fw.sendCh)
}

// ---------------------------------------------------------------------------
// Connection handler
// ---------------------------------------------------------------------------

// handleConnection manages one guest connection:
//  1. Sends authoritative Mac PSTE snapshot.
//  2. Starts poller goroutine (Mac→guest).
//  3. Starts keepalive goroutine (PING/PONG).
//  4. Reader loop (guest→host).
func (s *pbdState) handleConnection(conn *vz.VirtioSocketConnection) {
	defer conn.Close()
	log.Printf("pbd: guest connected on vsock port %d", pbdPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel context when VM stops.
	go func() {
		select {
		case <-s.stopCh:
			cancel()
			conn.Close()
		case <-ctx.Done():
		}
	}()

	fw := newPBDFrameWriter(conn)
	go fw.run()
	defer fw.close()

	// Reset suppression bookkeeping for this connection (§5.3).
	s.cocoa.reset(s.stopCh)

	// --- Step 1: authoritative Mac snapshot ---
	initRes, ok := s.cocoa.poll(s.stopCh)
	if !ok {
		return
	}
	if initRes.hasText {
		fw.send([4]byte{'P', 'S', 'T', 'E'}, []byte(initRes.text))
		log.Printf("pbd: sent initial PSTE (%d bytes)", len(initRes.text))
	} else {
		fw.send([4]byte{'P', 'S', 'T', 'E'}, []byte{})
		log.Printf("pbd: sent initial PSTE (empty)")
	}
	lastPollCount := initRes.changeCount

	// doneCh is closed when the reader exits; poller and keepalive watch it.
	doneCh := make(chan struct{})
	defer close(doneCh)

	// --- Step 2: poller goroutine ---
	go func() {
		ticker := time.NewTicker(pbdPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-doneCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				res, ok := s.cocoa.poll(s.stopCh)
				if !ok {
					return
				}
				if res.changeCount == lastPollCount {
					continue
				}
				lastPollCount = res.changeCount

				if !res.hasText {
					// Non-text content (image, file, …); §5.3: leave guest alone.
					continue
				}
				if res.suppress {
					// Bridge's own last write; don't echo back.
					continue
				}
				payload := []byte(res.text)
				if !utf8.Valid(payload) {
					// NSString always returns valid UTF-8, but be defensive.
					continue
				}
				fw.send([4]byte{'P', 'S', 'T', 'E'}, payload)
			}
		}
	}()

	// --- Step 3: keepalive goroutine ---
	var pendingPong atomic.Bool
	var pongDl atomic.Value // stores time.Time

	go func() {
		ticker := time.NewTicker(pbdPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-doneCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if pendingPong.Load() {
					dl, _ := pongDl.Load().(time.Time)
					if !dl.IsZero() && time.Now().After(dl) {
						log.Printf("pbd: keepalive timeout; closing connection")
						conn.Close()
						return
					}
					// Still within window; try again next tick.
					continue
				}
				pendingPong.Store(true)
				pongDl.Store(time.Now().Add(pbdPongTimeout))
				fw.send([4]byte{'P', 'I', 'N', 'G'}, []byte{})
			}
		}
	}()

	// --- Step 4: reader loop ---
	hdr := make([]byte, 14) // 8 (hex len) + 1 (SP) + 4 (type) + 1 (SP)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(pbdReadDeadline))
		if _, err := io.ReadFull(conn, hdr); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("pbd: read header: %v", err)
			return
		}

		// Validate separators.
		if hdr[8] != ' ' || hdr[13] != ' ' {
			log.Printf("pbd: malformed frame: bad separators in header")
			return
		}

		// Parse length (8 lowercase hex digits).
		payLen, ok := pbdParseHex8(hdr[0:8])
		if !ok {
			log.Printf("pbd: malformed frame: non-hex length field")
			return
		}

		// Validate type.
		typ := string(hdr[9:13])
		switch typ {
		case "SNRF", "PSTE", "PING", "PONG":
		default:
			log.Printf("pbd: unknown frame type %q; closing", typ)
			return
		}

		// Reject oversized payload before allocating.
		if payLen > pbdMaxPayload {
			log.Printf("pbd: oversized payload %d (max %d); closing", payLen, pbdMaxPayload)
			// §5.2: silent truncation is data corruption; reject and close.
			return
		}

		// Read payload.
		var payload []byte
		if payLen > 0 {
			payload = make([]byte, payLen)
			_ = conn.SetReadDeadline(time.Now().Add(pbdReadDeadline))
			if _, err := io.ReadFull(conn, payload); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("pbd: read payload: %v", err)
				return
			}
		}

		// Dispatch frame.
		switch typ {
		case "SNRF":
			// Guest → Mac pasteboard.
			if payLen > 0 && !utf8.Valid(payload) {
				// §5.2: reject invalid UTF-8; leave previous clipboard intact.
				// Do NOT close the connection; just skip this frame.
				log.Printf("pbd: SNRF: invalid UTF-8; ignoring frame")
				continue
			}
			// Apply synchronously so the Cocoa goroutine records the hash
			// before the next poller poll can race.
			if !s.cocoa.set(payload, s.stopCh) {
				log.Printf("pbd: SNRF: failed to write to pasteboard")
			}

		case "PSTE":
			// We are the host; we don't expect PSTE from the guest.
			log.Printf("pbd: unexpected PSTE from guest; ignoring")

		case "PING":
			fw.send([4]byte{'P', 'O', 'N', 'G'}, []byte{})

		case "PONG":
			pendingPong.Store(false)
		}
	}
}

// pbdParseHex8 parses exactly 8 hex digit bytes and returns the value and
// whether the parse succeeded.
func pbdParseHex8(b []byte) (int, bool) {
	if len(b) != 8 {
		return 0, false
	}
	v := 0
	for _, c := range b {
		d := 0
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}
