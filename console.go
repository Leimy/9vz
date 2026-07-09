// console.go — drawterm-as-console for 9vz.
//
// 9vz listens on vsock port 9004 for guest connections and spawns one
// bundled drawterm process per accepted connection, with the connection
// on the child's fds 0 and 1 (inetd style; drawterm's -0 flag).  The
// guest side is the vzcons supervisor (see drawterm-console.md §3.2):
// it dials vsock!2!9004, blocks reading the rcpu script length line,
// and serves the session script drawterm sends.
//
// Dial direction is inverted relative to normal drawterm use: the guest
// dials out to this listener (devvsock version 0 has no guest-side
// listening), and drawterm — which normally dials — is handed the
// already-established stream.  The rcpu protocol does not care who
// dialed; drawterm still sends the script and serves 9P.
//
// No auth, no TLS: the trust boundary is the local VM, same stance as
// pbd (drawterm-console.md §6).
//
// Accept/lifecycle model (v0):
//   - The vendored Code-Hex/vz binding AUTO-ACCEPTS guest connections:
//     shouldAcceptNewConnectionHandler returns true unconditionally and
//     queues the connection until AcceptVirtioSocketConnection drains
//     it.  The guest's dial therefore completes as soon as the VM is
//     up; the guest supervisor must (and does) treat the script length
//     line, not connect-completion, as the session rendezvous.
//   - One drawterm child per accepted connection; connections are
//     accepted eagerly, so with the v0 guest supervisor (one pending
//     dial, re-armed after each session ends) closing a console window
//     causes the guest to re-dial and a fresh window to appear —
//     getty-style respawn.  A "window wanted" gate (menu item, signal)
//     can be added later without touching the guest.
//   - The framework's VZVirtioSocketConnection objc object is retained
//     by the patched binding (patches/apply.sh edit 5); without that,
//     the framework tears the connection down at an unpredictable
//     autorelease-pool drain and kills healthy sessions.  See spawn().
//   - When the child exits, 9vz closes its side of the connection; the
//     guest session sees the hangup and the supervisor re-arms.  When
//     the connection dies first (guest reboot), drawterm's exportfs
//     sees EOF and exits; the Wait() here reaps it.
//   - If 9vz itself dies, the framework's socketpair peer closes, the
//     children see EOF and exit on their own; no explicit kill needed.
//
// The child's fd 2 is pointed at a log file (-consolelog): with 0/1
// carrying the connection, drawterm's diagnostics need somewhere else
// to go.
package main

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

const consolePort = uint32(9004)

type consoleState struct {
	listener *vz.VirtioSocketListener
	drawterm string // path to the drawterm binary (PATH-resolved by exec)
	logPath  string // file receiving drawterm's stderr; "" = inherit 9vz's stderr
	stopCh   chan struct{}
	stopOnce sync.Once
}

// startConsole is called from main after NewVirtualMachine but before
// vm.Start (so the listener is registered before the guest can dial).
// Failure is fatal, matching the pbd pattern: if -console was asked
// for, a missing vsock device or listener is a configuration error.
func startConsole(vm *vz.VirtualMachine, drawterm, logPath string) *consoleState {
	devs := vm.SocketDevices()
	if len(devs) == 0 {
		log.Fatalf("console: no vsock device found (VirtioSocketDeviceConfiguration missing from config?)")
	}
	listener, err := devs[0].Listen(consolePort)
	if err != nil {
		log.Fatalf("console: listen on vsock port %d: %v", consolePort, err)
	}

	s := &consoleState{
		listener: listener,
		drawterm: drawterm,
		logPath:  logPath,
		stopCh:   make(chan struct{}),
	}
	go s.runAcceptLoop()
	log.Printf("console: drawterm console listening on vsock port %d", consolePort)
	return s
}

// stop shuts the console listener down.  Running drawterm children are
// not killed: their connections die with the VM (or with this process),
// at which point exportfs sees EOF and they exit on their own.
func (s *consoleState) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.listener.Close()
	})
}

func (s *consoleState) runAcceptLoop() {
	type acceptResult struct {
		conn *vz.VirtioSocketConnection
		err  error
	}

	for {
		// Run Accept in a goroutine so we can also watch stopCh.  As in
		// pbd, the goroutine is intentionally leaked on VM stop: the
		// process exits shortly after stopCh closes.
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
				log.Printf("console: accept error: %v", res.err)
				select {
				case <-time.After(100 * time.Millisecond):
				case <-s.stopCh:
					return
				}
				continue
			}
		}

		// One drawterm per connection; sessions are independent, so
		// spawn concurrently (milestone 5 multi-window falls out of
		// this for free once the guest supervisor re-arms per script).
		go s.spawn(res.conn)
	}
}

// spawn runs one drawterm child for one guest connection and reaps it.
func (s *consoleState) spawn(conn *vz.VirtioSocketConnection) {
	// Close our copy of the connection only after the child exits,
	// so a child exit still propagates hangup to the guest promptly.
	//
	// NOTE conn is NOT what keeps the session alive.  The binding
	// flattens the framework's VZVirtioSocketConnection to a dup'd
	// fd (socket.go newVirtioSocketConnection); the Go wrapper holds
	// no objc reference and has no finalizer.  Left unretained, the
	// objc object is dealloc'd at the next autorelease-pool drain on
	// the VM dispatch queue (~30-100s observed, varying with VM
	// activity), which tears down the virtio connection even though
	// the child holds dup'd fds: the guest saw "vsock: connection
	// reset", drawterm saw EOF on exportfs and died.  The real fix
	// is in the vendored binding (patches/apply.sh edit 5): the
	// listener delegate retains the connection object.  An earlier
	// theory blamed GC of this Go wrapper; holding it here (this
	// deferred Close) turned out to be necessary hygiene but not
	// sufficient -- it keeps a dup'd fd alive, not the objc object.
	defer conn.Close()

	// Dup the descriptor out of the binding's net.Conn so the child
	// gets an independent fd (File() provides the dup).
	f, err := conn.File()
	if err != nil {
		log.Printf("console: cannot get connection fd: %v", err)
		return
	}

	var logf *os.File
	if s.logPath != "" {
		logf, err = os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("console: open %s: %v; drawterm stderr goes to 9vz's stderr", s.logPath, err)
			logf = nil
		}
	}

	cmd := exec.Command(s.drawterm, "-0")
	cmd.Stdin = f  // child fd 0: the connection
	cmd.Stdout = f // child fd 1: the connection
	if logf != nil {
		cmd.Stderr = logf
	} else {
		cmd.Stderr = os.Stderr
	}

	err = cmd.Start()

	// The child holds its own dups after Start; the parent's copy of
	// the dup'd fd and the log fd can go now.  conn itself must NOT
	// be closed here — see the comment at the top of this function.
	f.Close()
	if logf != nil {
		logf.Close()
	}

	if err != nil {
		log.Printf("console: start %s: %v", s.drawterm, err)
		return
	}
	pid := cmd.Process.Pid
	log.Printf("console: drawterm started (pid %d)", pid)

	if err := cmd.Wait(); err != nil {
		log.Printf("console: drawterm (pid %d) exited: %v", pid, err)
	} else {
		log.Printf("console: drawterm (pid %d) exited", pid)
	}
}
