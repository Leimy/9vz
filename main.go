// 9vz — experimental harness for booting 9front (arm64) under Apple
// Virtualization.framework, via the Code-Hex/vz Go bindings.
//
// Two boot paths:
//
//	-kernel path     direct kernel load (VZLinuxBootLoader). Works only if
//	                 the 9front virt kernel carries the Linux arm64 Image
//	                 header (run check_kernel.sh first).
//	-efi             EFI firmware boot (VZEFIBootLoader). Use to sanity-check
//	                 the serial/console plumbing (EDK2 prints to the virtio
//	                 console even with no bootable disk), and later as the
//	                 U-Boot-as-EFI-payload fallback path.
//
// Serial is wired to stdin/stdout (virtio-console on the guest side —
// see README "Known risks" #1). Ctrl-C requests a stop; twice forces exit.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

var (
	kernelPath = flag.String("kernel", "", "path to guest kernel (direct boot)")
	initrdPath = flag.String("initrd", "", "optional initrd/initramfs (direct boot only)")
	cmdline    = flag.String("cmdline", "", "kernel command line / bootargs passed via DTB /chosen")
	diskPath   = flag.String("disk", "", "path to RAW disk image (convert qcow2 first; see README)")
	diskRO     = flag.Bool("ro", false, "attach disk read-only")
	useEFI     = flag.Bool("efi", false, "boot EFI firmware instead of direct kernel load")
	efiStore   = flag.String("efistore", "efistore.bin", "EFI variable store path (created if missing)")
	cpus       = flag.Uint("cpus", 2, "virtual CPU count")
	memGiB     = flag.Uint64("mem", 2, "guest memory in GiB")
	noNet      = flag.Bool("nonet", false, "disable virtio-net (NAT)")
)

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("9vz: ")

	if !*useEFI && *kernelPath == "" {
		log.Fatal("need -kernel for direct boot, or -efi for firmware boot")
	}

	bootLoader, err := makeBootLoader()
	if err != nil {
		log.Fatalf("bootloader: %v", err)
	}

	config, err := vz.NewVirtualMachineConfiguration(
		bootLoader,
		*cpus,
		*memGiB*1024*1024*1024,
	)
	if err != nil {
		log.Fatalf("vm config: %v", err)
	}

	// --- serial console on stdio ---
	restore, err := rawMode(os.Stdin)
	if err != nil {
		log.Printf("warning: raw mode: %v (serial input may be line-buffered)", err)
	} else {
		defer restore()
	}
	serialAttach, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	if err != nil {
		log.Fatalf("serial attachment: %v", err)
	}
	consoleCfg, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttach)
	if err != nil {
		log.Fatalf("serial config: %v", err)
	}
	config.SetSerialPortsVirtualMachineConfiguration(
		[]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleCfg})

	// --- storage (virtio-blk) ---
	if *diskPath != "" {
		att, err := vz.NewDiskImageStorageDeviceAttachment(*diskPath, *diskRO)
		if err != nil {
			log.Fatalf("disk attachment (%s): %v", *diskPath, err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			log.Fatalf("virtio-blk: %v", err)
		}
		config.SetStorageDevicesVirtualMachineConfiguration(
			[]vz.StorageDeviceConfiguration{blk})
	}

	// --- network (virtio-net, NAT) ---
	if !*noNet {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			log.Fatalf("NAT attachment: %v", err)
		}
		net, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if err != nil {
			log.Fatalf("virtio-net: %v", err)
		}
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			log.Fatalf("mac: %v", err)
		}
		net.SetMACAddress(mac)
		config.SetNetworkDevicesVirtualMachineConfiguration(
			[]*vz.VirtioNetworkDeviceConfiguration{net})
	}

	// --- entropy ---
	if ent, err := vz.NewVirtioEntropyDeviceConfiguration(); err == nil {
		config.SetEntropyDevicesVirtualMachineConfiguration(
			[]*vz.VirtioEntropyDeviceConfiguration{ent})
	}

	// --- vsock (for the eventual 9P-over-vsock control plane) ---
	if vs, err := vz.NewVirtioSocketDeviceConfiguration(); err == nil {
		config.SetSocketDevicesVirtualMachineConfiguration(
			[]vz.SocketDeviceConfiguration{vs})
	}

	if ok, err := config.Validate(); !ok || err != nil {
		log.Fatalf("config validation failed: %v", err)
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		log.Fatalf("vm creation: %v", err)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if err := vm.Start(); err != nil {
		log.Fatalf("vm start: %v", err)
	}
	fmt.Fprintln(os.Stderr, "9vz: VM started — serial follows (^C to stop)")

	stopping := false
	for {
		select {
		case <-sigCh:
			if stopping {
				log.Println("force exit")
				if restore != nil {
					restore()
				}
				os.Exit(1)
			}
			stopping = true
			if ok, err := vm.RequestStop(); err != nil || !ok {
				log.Printf("graceful stop unavailable (%v), stopping hard", err)
				_ = vm.Stop()
			}
		case st := <-vm.StateChangedNotify():
			switch st {
			case vz.VirtualMachineStateRunning:
				log.Println("state: running")
			case vz.VirtualMachineStateError:
				log.Fatal("state: error")
			case vz.VirtualMachineStateStopped:
				log.Println("state: stopped")
				return
			}
		}
	}
}

func makeBootLoader() (vz.BootLoader, error) {
	if *useEFI {
		var store *vz.EFIVariableStore
		var err error
		if _, statErr := os.Stat(*efiStore); statErr == nil {
			store, err = vz.NewEFIVariableStore(*efiStore)
		} else {
			store, err = vz.NewEFIVariableStore(*efiStore, vz.WithCreatingEFIVariableStore())
		}
		if err != nil {
			return nil, fmt.Errorf("efi variable store: %w", err)
		}
		return vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	}

	opts := []vz.LinuxBootLoaderOption{}
	if *cmdline != "" {
		opts = append(opts, vz.WithCommandLine(*cmdline))
	}
	if *initrdPath != "" {
		opts = append(opts, vz.WithInitrd(*initrdPath))
	}
	return vz.NewLinuxBootLoader(*kernelPath, opts...)
}

// rawMode puts f into raw-ish mode (no echo, no canonicalization, keep ISIG
// so ^C still reaches us) and returns a restore func.
func rawMode(f *os.File) (func(), error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGet)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.ICRNL
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSet, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSet, old) }, nil
}
