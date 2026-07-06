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
//
// With -gui, a virtio-gpu device, a USB keyboard and a USB pointing
// device are also attached, and the VM is handed to AppKit's run loop
// so it shows up in a real macOS window. Serial keeps flowing on stdio.
// The guest needs drivers for those devices to use them; see the README
// "Native graphics" section.
//
// With -audio, a virtio-sound device with a host speaker output stream
// is attached (guest playback via the vz64 kernel's audiovz driver,
// #A /dev/audio).  -mic also attaches a host microphone input stream
// for guest capture. See the README "Audio" section.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
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
	gui        = flag.Bool("gui", false, "open a graphics window (virtio-gpu + USB keyboard/mouse); serial still flows on stdio")
	// Audio (virtio-sound).  -audio attaches a virtio-sound device with a
	// host OUTPUT stream (guest -> Mac speaker); the guest's audiovz driver
	// drives it as #A /dev/audio (playback).  -mic additionally attaches a
	// host INPUT stream (Mac microphone -> guest) for capture. Mic capture
	// also needs the
	// com.apple.security.device.audio-input entitlement (see vz.entitlements)
	// and triggers a macOS privacy prompt on first use.
	audio = flag.Bool("audio", false, "attach a virtio-sound device with a host speaker output stream")
	mic   = flag.Bool("mic", false, "also attach a host microphone input stream (implies -audio; needs the audio-input entitlement)")
	// Default to a 16:10 laptop-native aspect ratio rather than the old
	// 4:3 1024x768.  1440x900 is a common MacBook logical resolution and
	// gives rio a roomier, modern shape.  The guest kernel adopts the
	// host scanout size via GET_DISPLAY_INFO, so no kernel change is
	// needed to follow this; override with -width/-height if desired.
	// These set the initial window size AND the guest scanout size (the
	// latter divided by -scale).  The vendored VZ window code tries to start
	// fullscreen through a guarded AppKit scheduler; see the README
	// "Fullscreen window".  Set 9VZ_NOFULLSCREEN=1 to keep a plain window of
	// this size.
	width  = flag.Int("width", 1440, "graphics width (-gui): window size and, /scale, the guest scanout")
	height = flag.Int("height", 900, "graphics height (-gui): window size and, /scale, the guest scanout")
	// HiDPI / font-size scaling for the -gui console.
	//
	// The guest (9front rio, or a Linux text console) draws 1:1 into the
	// virtio-gpu scanout's PIXEL grid; it has no notion of display DPI.  On a
	// Retina panel a 1440x900 scanout is packed into a small physical area,
	// so everything -- notably the Alpine text-console font -- looks tiny.
	//
	// There is no per-display DPI knob for a non-macOS guest in VZ, so we get
	// "HiDPI" the only way the architecture allows: make the guest render
	// FEWER pixels and let the VZVirtualMachineView upscale them to fill the
	// window.  With -scale 2 the scanout becomes width/2 x height/2; the guest
	// draws into that smaller grid and the host stretches each guest pixel to
	// a 2x2 block, so the font (and everything else) is twice as large.  The
	// window keeps its -width/-height logical size; only the guest's pixel
	// density changes.  scale=1 is the historical 1:1 behavior.
	//
	// IMPORTANT: for this to take effect the VZ view's
	// automaticallyReconfiguresDisplay must be OFF.  Upstream Code-Hex/vz
	// turns it ON (macOS 14+), which resizes the guest display to the view's
	// backing-pixel size and silently overrides this scanout -- that was why
	// an earlier -scale "did nothing".  Our vendored copy is patched to set it
	// to NO (by patches/apply.sh), so the scanout size below actually sticks
	// and is upscaled by the view.  (A real fullscreen Space for that view is
	// attempted but NOT WORKING YET; see the README "Fullscreen window".)
	scale = flag.Float64("scale", 1, "HiDPI scale factor for -gui: guest renders at width/scale x height/scale and the window upscales it (>1 = bigger font)")
)

// AppKit (NSApplication / [NSApp run], used by StartGraphicApplication in
// -gui mode) must run on the process's *main* OS thread -- the very first
// thread created at startup -- or it trips a fatal libdispatch/main-thread
// assertion (observed as a SIGTRAP inside startVirtualMachineWindow).
//
// runtime.LockOSThread pins the calling goroutine to whatever OS thread it
// is currently running on.  Calling it from main() is too late: by then the
// Go scheduler may have migrated goroutine 1 (which runs init and main) onto
// some other M/thread, so we would lock AppKit to a non-main thread.  Locking
// here in init() pins goroutine 1 to the startup thread before the scheduler
// gets a chance to move it, which is exactly the thread AppKit needs.  In
// headless mode this is harmless (the lock just stays in effect).
func init() {
	runtime.LockOSThread()
}

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

	// --- audio (virtio-sound) ---
	//
	// A single virtio-sound device (PCI 1AF4:1059) with a host OUTPUT
	// stream (speaker) and, with -mic, a host INPUT stream (microphone).
	// In the guest this is the vz64 kernel's audiovz driver: #A
	// /dev/audio (playback).  -mic implies -audio.  The guest is
	// expected to enumerate two streams when -mic is used: output first,
	// then input.
	if *mic {
		*audio = true
	}
	if *audio {
		snd, err := vz.NewVirtioSoundDeviceConfiguration()
		if err != nil {
			log.Fatalf("virtio-sound: %v", err)
		}
		streams := []vz.VirtioSoundDeviceStreamConfiguration{}
		out, err := vz.NewVirtioSoundDeviceHostOutputStreamConfiguration()
		if err != nil {
			log.Fatalf("audio output stream: %v", err)
		}
		streams = append(streams, out)
		if *mic {
			in, err := vz.NewVirtioSoundDeviceHostInputStreamConfiguration()
			if err != nil {
				log.Fatalf("audio input stream: %v", err)
			}
			streams = append(streams, in)
		}
		snd.SetStreams(streams...)
		config.SetAudioDevicesVirtualMachineConfiguration(
			[]vz.AudioDeviceConfiguration{snd})
		if *mic {
			fmt.Fprintln(os.Stderr, "9vz: audio: virtio-sound with speaker + microphone")
		} else {
			fmt.Fprintln(os.Stderr, "9vz: audio: virtio-sound with speaker (output only)")
		}
	}

	// --- graphics + input (only in -gui mode) ---
	//
	// Attaches a virtio-gpu device with a single scanout (VZ supports at
	// most one), plus a USB keyboard and a USB absolute-coordinate pointing
	// device. The guest needs drivers for all three; until the vz64 kernel
	// grows them, the window paints black but the serial console still works.
	if *gui {
		gpu, err := vz.NewVirtioGraphicsDeviceConfiguration()
		if err != nil {
			log.Fatalf("virtio-gpu: %v", err)
		}
		// Derive the guest scanout (pixel) size from the window size and the
		// HiDPI scale factor.  scale>1 shrinks the scanout so the host view
		// upscales it -> larger on-screen font.  Clamp to a sane minimum so a
		// silly scale can't produce a 0- or 1-pixel framebuffer that fails
		// validation.
		if *scale < 1 {
			log.Printf("warning: -scale %g < 1 makes the font smaller; clamping to 1", *scale)
			*scale = 1
		}
		sw := int64(float64(*width) / *scale)
		sh := int64(float64(*height) / *scale)
		if sw < 64 {
			sw = 64
		}
		if sh < 64 {
			sh = 64
		}
		if *scale != 1 {
			log.Printf("gui: window %dx%d, guest scanout %dx%d (scale %g)",
				*width, *height, sw, sh, *scale)
		}
		scanout, err := vz.NewVirtioGraphicsScanoutConfiguration(sw, sh)
		if err != nil {
			log.Fatalf("graphics scanout: %v", err)
		}
		gpu.SetScanouts(scanout)
		config.SetGraphicsDevicesVirtualMachineConfiguration(
			[]vz.GraphicsDeviceConfiguration{gpu})

		kbd, err := vz.NewUSBKeyboardConfiguration()
		if err != nil {
			log.Fatalf("usb keyboard: %v", err)
		}
		config.SetKeyboardsVirtualMachineConfiguration(
			[]vz.KeyboardConfiguration{kbd})

		ptr, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
		if err != nil {
			log.Fatalf("usb pointing device: %v", err)
		}
		config.SetPointingDevicesVirtualMachineConfiguration(
			[]vz.PointingDeviceConfiguration{ptr})
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

	// In GUI mode the AppKit run loop must own the main OS thread (the
	// goroutine here is already pinned to it in init()), so the signal/state
	// multiplexer moves to a goroutine and StartGraphicApplication blocks here
	// until the window closes (or the guest stops). In headless mode the
	// multiplexer simply runs on the main goroutine as before.
	if *gui {
		go runStateLoop(vm, sigCh, restore)
		err := vm.StartGraphicApplication(
			float64(*width), float64(*height),
			vz.WithWindowTitle("9vz"),
			vz.WithController(true),
		)
		if restore != nil {
			restore()
		}
		if err != nil {
			log.Fatalf("graphics window: %v", err)
		}
		return
	}

	runStateLoop(vm, sigCh, restore)
}

// runStateLoop multiplexes OS signals against the VM state channel: the first
// interrupt requests a graceful stop, the second forces exit, and a Stopped
// transition ends the process. restore (may be nil) puts the terminal back.
func runStateLoop(vm *vz.VirtualMachine, sigCh <-chan os.Signal, restore func()) {
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
				if restore != nil {
					restore()
				}
				os.Exit(0)
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
