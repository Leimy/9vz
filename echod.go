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
	"io"
	"log"

	"github.com/Code-Hex/vz/v3"
)

const echoPort = uint32(9200)

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
