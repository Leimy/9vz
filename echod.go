// echod.go — vsock echo listener for latency/throughput testing.
//
// Enabled with -echo.  Listens on vsock port 9200 (the devsock.md
// milestone-0 spike port) and echoes every byte back on every accepted
// connection.  Unlike pbd, multiple concurrent connections are allowed:
// the flow-control milestone tests want several conversations at once.
//
// The guest-side counterpart is vsping(1) (from /sys/src/cmd/vzcmd),
// which dials vsock!2!9200 and measures round trips; the p50 of a
// small-payload run is the calibration constant devsock.md §4 says to
// record at milestone 5.
package main

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/Code-Hex/vz/v3"
)

const echoPort = uint32(9200)

// tcpEchoPort is the plain-TCP counterpart to the vsock echoPort above,
// used for tcping(1) (see /sys/src/cmd/vzcmd/tcping.c) -- a fair
// transport-vs-transport latency comparison needs BOTH ends free of
// Nagle-induced stalls.  Reusing 9200 is safe: TCP and vsock have
// independent port namespaces, and it matches what tcping's usage
// comment already tells guests to dial (tcp!<hostaddr>!9200).
const tcpEchoPort = 9200

// startEcho is called from main after NewVirtualMachine but before
// vm.Start, same as startPBD.  Failure is fatal when -echo is set.
// The accept goroutine runs for the VM lifetime; like pbd's accept
// loop, it is cleaned up by process exit rather than by explicit
// shutdown (this is a test harness, not a service with state).
func startEcho(vm *vz.VirtualMachine) {
	devs := vm.SocketDevices()
	if len(devs) == 0 {
		log.Fatalf("echo: no vsock device found (VirtioSocketDeviceConfiguration missing from config?)")
	}
	listener, err := devs[0].Listen(echoPort)
	if err != nil {
		log.Fatalf("echo: listen on vsock port %d: %v", echoPort, err)
	}

	go func() {
		for {
			conn, err := listener.AcceptVirtioSocketConnection()
			if err != nil {
				// Listener closed (VM stopping) or transient error;
				// either way this test listener just stops accepting.
				log.Printf("echo: accept: %v", err)
				return
			}
			go func(c *vz.VirtioSocketConnection) {
				defer c.Close()
				log.Printf("echo: accepted connection from guest port %d (dest port %d)",
					c.SourcePort(), c.DestinationPort())
				n, err := io.Copy(c, c)
				if err != nil {
					log.Printf("echo: connection ended after %d bytes: %v", n, err)
				} else {
					log.Printf("echo: connection closed after %d bytes", n)
				}
			}(conn)
		}
	}()
	log.Printf("echo: vsock echo listener on port %d", echoPort)
}

// startTCPEcho is the plain-TCP counterpart to startEcho, for a fair
// tcping-vs-vsping comparison (devsock.md's "how much better than
// TCP/IP" question).  It listens on all interfaces -- the guest
// reaches it via whatever gateway/NAT address 9vz's virtio-net gives
// it, same as any other host-side TCP service -- and, critically,
// disables Nagle on every accepted connection.
//
// Without SetNoDelay, a plain nc-based echo (nc lacks any portable way
// to turn this off) hits the classic Nagle + delayed-ACK interaction
// on any payload that splits into more than one TCP segment: the
// trailing sub-MSS segment sits until an ACK arrives, and if that ACK
// isn't forced, the peer's delayed-ack timer has to expire first.
// Plan 9's own TCP stack (/sys/src/9/ip/tcp.c) uses a 50ms delayed-ack
// tick (TCP_ACK), which is exactly the plateau this produced in
// practice (see devsock.md).  Every real 9P-over-TCP implementation
// disables Nagle for this reason; a fair comparison must too.
func startTCPEcho() {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", tcpEchoPort))
	if err != nil {
		log.Fatalf("tcpecho: listen on tcp port %d: %v", tcpEchoPort, err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("tcpecho: accept: %v", err)
				return
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				if err := tc.SetNoDelay(true); err != nil {
					log.Printf("tcpecho: SetNoDelay: %v", err)
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				log.Printf("tcpecho: accepted connection from %s", c.RemoteAddr())
				n, err := io.Copy(c, c)
				if err != nil {
					log.Printf("tcpecho: connection ended after %d bytes: %v", n, err)
				} else {
					log.Printf("tcpecho: connection closed after %d bytes", n)
				}
			}(conn)
		}
	}()
	log.Printf("tcpecho: plain TCP echo listener on port %d (TCP_NODELAY set)", tcpEchoPort)
}
