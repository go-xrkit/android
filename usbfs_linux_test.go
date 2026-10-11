// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// beastCamera is the device cmd/xrcam found: a Sonix UVC part, which is NOT a
// VITURE device — the headset arrives as three and the camera is its own chip.
func beastCamera() USBDevice {
	return USBDevice{
		Name: "/dev/bus/usb/001/003", VendorID: 0x0c45, ProductID: 0x6368,
		Manufacturer: "Sonix Technology Co., Ltd.", Product: "USB 2.0 Camera",
		Class: 0xef, Subclass: 0x02, Protocol: 0x01,
	}
}

// cameraDescriptors is what that camera's descriptor blob looks like: a device,
// a UVC control interface, the zero-bandwidth streaming alternate, and the
// largest one, whose endpoint is the question.
func cameraDescriptors() []byte {
	b := deviceDescriptor()
	b = append(b, 9, descriptorTypeInterface, 0, 0, 1, USBClassVideo, USBSubclassVideoControl, 0, 0)
	b = append(b, 7, descriptorTypeEndpoint, 0x83, 0x03, 16, 0, 6)
	b = append(b, 9, descriptorTypeInterface, 1, 0, 0, USBClassVideo, USBSubclassVideoStreaming, 0, 0)
	b = append(b, 9, descriptorTypeInterface, 1, 6, 1, USBClassVideo, USBSubclassVideoStreaming, 0, 0)
	b = append(b, 7, descriptorTypeEndpoint, 0x81, 0x05, 0x00, 0x14, 1) // 5120, isochronous, IN
	return b
}

// openCamera stands a fake host up and opens the camera against it.
func openCamera(t *testing.T) (*fakeWall, *USBHandle) {
	t.Helper()
	fw := newFakeWall(t, 0)
	fw.setUSBDescriptors(cameraDescriptors())
	h, err := OpenUSB(screenCtx(t), beastCamera())
	if err != nil {
		t.Fatalf("OpenUSB: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return fw, h
}

// ⭐ THE DESCRIPTORS COME BACK THROUGH A REAL read(2) ON A REAL DESCRIPTOR that
// crossed a socket as SCM_RIGHTS. That is the whole handover, exercised rather
// than mocked: the one thing a stand-in cannot fake is whether the kernel hands
// the process something it can actually read.
func TestTheLentDescriptorIsReadableAndSaysWhatItIs(t *testing.T) {
	_, h := openCamera(t)
	if h.Name() != beastCamera().Name {
		t.Fatalf("Name = %q", h.Name())
	}
	d, err := h.Descriptors()
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	v, p, _, _, _, err := d.Device()
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if v != VitureVendorID || p != 0x1201 {
		t.Fatalf("the blob decoded as %04x:%04x", v, p)
	}
	ifaces, err := d.Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	if len(ifaces) != 3 {
		t.Fatalf("found %d interfaces, want 3", len(ifaces))
	}
	// The one the probe would pick: the largest isochronous IN endpoint.
	last := ifaces[2]
	if !last.VideoStreaming() || len(last.Endpoints) != 1 {
		t.Fatalf("the last interface is %s", last)
	}
	if e := last.Endpoints[0]; e.MaxPacketSize != 5120 || e.Type() != USBTransferIsochronous || !e.In() {
		t.Fatalf("the streaming endpoint is %s", e)
	}
}

func TestOpenUSBRefusesWhatCannotBeOpened(t *testing.T) {
	t.Run("a device with no name", func(t *testing.T) {
		newFakeWall(t, 0)
		if _, err := OpenUSB(screenCtx(t), USBDevice{}); !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("OpenUSB with no name = %v, want ErrInvalidOption", err)
		}
	})
	t.Run("no host", func(t *testing.T) {
		old := lookupEnv
		t.Cleanup(func() { lookupEnv = old })
		lookupEnv = func(string) (string, bool) { return "", false }
		if _, err := OpenUSB(screenCtx(t), beastCamera()); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("OpenUSB with no host = %v, want ErrUnsupported", err)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		t.Setenv(EnvWallSocket, "xr-usb-nothing-listens-here")
		oldB, oldP := dialBudget, dialPause
		dialBudget, dialPause = 30*time.Millisecond, 5*time.Millisecond
		t.Cleanup(func() { dialBudget, dialPause = oldB, oldP })
		if _, err := OpenUSB(screenCtx(t), beastCamera()); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("OpenUSB against a dead socket = %v, want ErrUnsupported", err)
		}
	})
	t.Run("the user refusing", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setOnOpenUSB(func(p *fakePanel, name string) bool {
			p.send(MsgError, EncodeError(ErrorMessage{Code: codeUSBPermissionDenied,
				Op: "requestPermission", Detail: "the user did not allow " + name}))
			return true
		})
		_, err := OpenUSB(screenCtx(t), beastCamera())
		if !errors.Is(err, ErrUSBPermissionDenied) {
			t.Fatalf("a refused permission = %v, want ErrUSBPermissionDenied", err)
		}
	})
	t.Run("a handle with no descriptor", func(t *testing.T) {
		// ⛔ THE ANSWER IS THE DESCRIPTOR. A MsgUSBHandle without one is a
		// handle that names nothing, and accepting it would turn every later
		// ioctl into a failure against fd -1.
		fw := newFakeWall(t, 0)
		fw.setOnOpenUSB(func(p *fakePanel, _ string) bool {
			p.send(MsgUSBHandle, nil)
			return true
		})
		_, err := OpenUSB(screenCtx(t), beastCamera())
		if err == nil || !strings.Contains(err.Error(), "no descriptor") {
			t.Fatalf("a handle with no descriptor = %v", err)
		}
	})
	t.Run("a silent host", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setOnOpenUSB(func(*fakePanel, string) bool { return true })
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if _, err := OpenUSB(ctx, beastCamera()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("OpenUSB against a silent host = %v", err)
		}
	})
}

func TestDescriptorsRefusesABlobTooShortToBeADevice(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setUSBDescriptors([]byte{1, 2, 3})
	h, err := OpenUSB(screenCtx(t), beastCamera())
	if err != nil {
		t.Fatalf("OpenUSB: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := h.Descriptors(); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("Descriptors over 3 bytes = %v, want ErrShortPayload", err)
	}
}

// ⛔⛔ THE IOCTLS ARE THE QUESTION, AND A memfd ANSWERS ENOTTY. That is the
// right negative: it is a refusal BY THE KERNEL for this descriptor, which is
// exactly the shape of the answer the probe is looking for and must report
// rather than swallow.
func TestEveryIoctlReportsTheKernelsOwnRefusal(t *testing.T) {
	_, h := openCamera(t)
	ep := USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120, Interval: 1}

	for _, c := range []struct {
		name string
		call func() error
		says string
	}{
		{"claim", func() error { return h.ClaimInterface(1) }, "claiming interface 1"},
		{"set-alternate", func() error { return h.SetAltSetting(1, 6) }, "selecting alternate 6"},
		{"submit", func() error { return h.SubmitISO(ep) }, "isochronous request to endpoint 0x81"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("a memfd cannot serve a usbfs ioctl, and this reported success")
			}
			if !errors.Is(err, syscall.ENOTTY) {
				t.Fatalf("%v, want ENOTTY — the kernel's own refusal", err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the error says %q, which does not say what was attempted", err)
			}
			// ⛔ AND IT NAMES THE DEVICE. A probe transcript that said "EINVAL"
			// without saying which device and which step is a transcript
			// nobody can act on.
			if !strings.Contains(err.Error(), beastCamera().Name) {
				t.Fatalf("the error does not name the device: %q", err)
			}
		})
	}
}

// The success paths, through the seam, because no descriptor a test can hold
// will accept a usbfs ioctl.
func TestTheIoctlsPassTheKernelWhatTheKernelExpects(t *testing.T) {
	_, h := openCamera(t)
	old := ioctl
	t.Cleanup(func() { ioctl = old })

	type call struct {
		req uintptr
		arg unsafe.Pointer
	}
	var calls []call
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		calls = append(calls, call{req, arg})
		return nil
	}

	if err := h.ClaimInterface(1); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if calls[0].req != usbdevfsClaimInterface {
		t.Fatalf("claim used ioctl %#x, want %#x", calls[0].req, usbdevfsClaimInterface)
	}
	if n := *(*uint32)(calls[0].arg); n != 1 {
		t.Fatalf("claim passed interface %d, want 1", n)
	}

	if err := h.SetAltSetting(1, 6); err != nil {
		t.Fatalf("SetAltSetting: %v", err)
	}
	if calls[1].req != usbdevfsSetInterface {
		t.Fatalf("set-alternate used ioctl %#x", calls[1].req)
	}
	si := *(*usbdevfsSetInterfaceArg)(calls[1].arg)
	if si.Interface != 1 || si.AltSetting != 6 {
		t.Fatalf("set-alternate passed %+v, want interface 1 alternate 6", si)
	}

	ep := USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120, Interval: 1}
	if err := h.SubmitISO(ep); err != nil {
		t.Fatalf("SubmitISO: %v", err)
	}
	if calls[2].req != usbdevfsSubmitURB {
		t.Fatalf("submit used ioctl %#x, want %#x", calls[2].req, usbdevfsSubmitURB)
	}
	u := *(*usbdevfsURB)(calls[2].arg)
	// ⛔⛔ THE URB TYPE IS NOT THE ENDPOINT ATTRIBUTE. An endpoint whose
	// bmAttributes say 1 is isochronous; a URB whose type is 1 is INTERRUPT.
	if u.Type != urbTypeIsochronous {
		t.Fatalf("the URB type is %d, want %d — the endpoint attribute was used instead "+
			"of the URB table", u.Type, urbTypeIsochronous)
	}
	if u.Endpoint != 0x81 {
		t.Fatalf("the URB names endpoint %#02x", u.Endpoint)
	}
	if u.NumberOfPackets != ISOPacketsPerURB {
		t.Fatalf("the URB carries %d packets, want %d", u.NumberOfPackets, ISOPacketsPerURB)
	}
	// ⛔⛔ THE DECODED SIZE, NOT wMaxPacketSize. This assertion USED TO SAY
	// MaxPacketSize*n and therefore asserted the bug: the kernel answered
	// EMSGSIZE on the device while the suite stayed green.
	if want := int32(ep.ISOPacketSize() * ISOPacketsPerURB); u.BufferLength != want {
		t.Fatalf("the URB's buffer is %d bytes, want %d", u.BufferLength, want)
	}
	if int(u.BufferLength) == ep.MaxPacketSize*ISOPacketsPerURB {
		t.Fatalf("the URB's buffer is the RAW wMaxPacketSize times %d; bits 12:11 are "+
			"transactions per microframe, not size", ISOPacketsPerURB)
	}
	if u.Buffer == 0 {
		t.Fatal("the URB names no buffer")
	}
	// Submitting is followed by discarding: a request left outstanding against
	// a camera nobody reads would stream into a buffer about to be dropped.
	if calls[3].req != usbdevfsDiscardURB {
		t.Fatalf("the submit was not followed by a discard: %#x", calls[3].req)
	}

	// ⛔ AND CLOSE RELEASES WHAT WAS CLAIMED. An interface left claimed keeps
	// the kernel's own driver off the camera after this process is gone.
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls[4].req != usbdevfsReleaseInterface {
		t.Fatalf("Close did not release the claimed interface: %#x", calls[4].req)
	}
}

func TestSubmitISORefusesAnEndpointItCannotTransferOn(t *testing.T) {
	_, h := openCamera(t)
	if err := h.SubmitISO(USBEndpoint{Address: 0x81, Attributes: 0x09}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("an endpoint with transfer type 1 but no packet size = %v", err)
	}
	if err := h.SubmitISO(USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 0}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("a zero maximum packet size = %v", err)
	}
}

func TestEveryMethodRefusesAfterClose(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setUSBDescriptors(cameraDescriptors())
	h, err := OpenUSB(screenCtx(t), beastCamera())
	if err != nil {
		t.Fatalf("OpenUSB: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent, because a deferred Close after an explicit one is ordinary.
	if err := h.Close(); err != nil {
		t.Fatalf("the second Close = %v, want nil", err)
	}
	if _, err := h.Descriptors(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Descriptors after Close = %v", err)
	}
	if err := h.ClaimInterface(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("ClaimInterface after Close = %v", err)
	}
	if err := h.SetAltSetting(1, 6); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetAltSetting after Close = %v", err)
	}
	ep := USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120}
	if err := h.SubmitISO(ep); !errors.Is(err, ErrClosed) {
		t.Fatalf("SubmitISO after Close = %v", err)
	}
	// ⛔ AND THE HOST LETS GO. The platform drops the device when its
	// UsbDeviceConnection closes, so the socket must not outlive the handle.
	waitFor(t, "the host to drop the connection", func() bool { return fw.live() == 0 })
}

// rawIoctl itself, which the seam replaces everywhere else: a real syscall on a
// real descriptor, BOTH WAYS.
//
// ⛔ A SEAM TESTED ONLY THROUGH ITS REPLACEMENT IS A SEAM NOBODY HAS RUN. The
// production path is this function, and a mistake in how it reads errno would
// be invisible to every test that swaps it out -- so it is called directly, on
// a descriptor that refuses and on one that answers.
func TestRawIoctlReportsBothOutcomes(t *testing.T) {
	_, h := openCamera(t)
	arg := uint32(0)
	if err := rawIoctl(h.fd, usbdevfsClaimInterface, unsafe.Pointer(&arg)); !errors.Is(err, syscall.ENOTTY) {
		t.Fatalf("rawIoctl on a memfd = %v, want ENOTTY", err)
	}

	// And an ioctl that SUCCEEDS, so the errno == 0 path is executed rather
	// than assumed. FIONREAD on a socket is served by the kernel and needs no
	// privilege; usbfs commands are refused by everything a test can hold.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = closeFD(fds[0]); _ = closeFD(fds[1]) })
	if _, err := syscall.Write(fds[1], []byte("xr")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var n int32
	if err := rawIoctl(fds[0], syscall.TIOCINQ, unsafe.Pointer(&n)); err != nil {
		t.Fatalf("rawIoctl(FIONREAD) = %v, want success", err)
	}
	if n != 2 {
		t.Fatalf("FIONREAD says %d bytes are waiting, want 2 -- the argument did not "+
			"reach the kernel", n)
	}
}

func TestDescriptorsReportsAnUnreadableDescriptor(t *testing.T) {
	_, h := openCamera(t)
	old := readDescriptors
	t.Cleanup(func() { readDescriptors = old })
	readDescriptors = func(int) ([]byte, error) { return nil, syscall.EACCES }
	if _, err := h.Descriptors(); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Descriptors = %v, want EACCES", err)
	}
}

func TestAHostRefusalMapsOntoErrUSBPermissionDenied(t *testing.T) {
	if err := hostError(ErrorMessage{Code: codeUSBPermissionDenied, Detail: "no"}); !errors.Is(err, ErrUSBPermissionDenied) {
		t.Fatalf("code %d mapped to %v", codeUSBPermissionDenied, err)
	}
}

// ⭐⭐ EBUSY IS AN OCCUPANT, NOT A REFUSAL — and detaching it is what made the
// whole route work. The kernel's uvcvideo holds a UVC camera and publishes
// /dev/video0 from it, so CLAIMINTERFACE answers "device or resource busy".
// This is the sequence that gets past it, and Close must put the driver back:
// a detached interface takes /dev/video0 away from the WHOLE PHONE.
func TestABusyInterfaceIsTakenFromItsDriverAndGivenBack(t *testing.T) {
	_, h := openCamera(t)
	old := ioctl
	t.Cleanup(func() { ioctl = old })

	type call struct {
		req  uintptr
		ifno int32
		code int32
	}
	var calls []call
	busy := true
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case usbdevfsClaimInterface:
			calls = append(calls, call{req: req, ifno: int32(*(*uint32)(arg))})
			if busy {
				return syscall.EBUSY
			}
			return nil
		case usbdevfsIoctl:
			a := *(*usbdevfsIoctlArg)(arg)
			calls = append(calls, call{req: req, ifno: a.Ifno, code: a.IoctlCode})
			busy = false // the driver has let go
			return nil
		}
		calls = append(calls, call{req: req})
		return nil
	}

	if err := h.ClaimInterface(1); err != nil {
		t.Fatalf("ClaimInterface over a busy interface: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("took %d calls to claim a busy interface, want 3: %+v", len(calls), calls)
	}
	if calls[0].req != usbdevfsClaimInterface {
		t.Fatalf("the first call was %#x; it must TRY before detaching anything", calls[0].req)
	}
	if calls[1].req != usbdevfsIoctl || calls[1].ifno != 1 {
		t.Fatalf("the second call was %#x on interface %d", calls[1].req, calls[1].ifno)
	}
	if want := int32(ioc(iocNone, usbdevfsDisconnect, 0)); calls[1].code != want {
		t.Fatalf("the driver was sent code %#x, want DISCONNECT %#x", calls[1].code, want)
	}
	if calls[2].req != usbdevfsClaimInterface {
		t.Fatalf("the claim was not retried after the detach: %#x", calls[2].req)
	}

	// ⚠ AND THE DRIVER COMES BACK. This is not politeness: /dev/video0 is gone
	// for every application on the phone until it does.
	calls = nil
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var released, reconnected bool
	for _, c := range calls {
		if c.req == usbdevfsReleaseInterface {
			released = true
		}
		if c.req == usbdevfsIoctl && c.code == int32(ioc(iocNone, usbdevfsConnect, 0)) {
			reconnected = true
		}
	}
	if !released {
		t.Fatal("Close did not release the interface it claimed")
	}
	if !reconnected {
		t.Fatal("Close did not reconnect the driver it detached, which leaves /dev/video0 " +
			"missing for the whole phone until the cable is pulled")
	}
}

// A driver that will not let go is reported WITH BOTH ERRORS, because either
// alone misleads: the detach failure says what was attempted, and the EBUSY
// says why it was attempted at all.
func TestADriverThatWillNotLetGoIsReportedWithBothReasons(t *testing.T) {
	_, h := openCamera(t)
	old := ioctl
	t.Cleanup(func() { ioctl = old })
	ioctl = func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == usbdevfsClaimInterface {
			return syscall.EBUSY
		}
		return syscall.EPERM
	}
	err := h.ClaimInterface(1)
	if !errors.Is(err, syscall.EPERM) || !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("ClaimInterface = %v; it must carry the detach failure AND the EBUSY", err)
	}
	if !strings.Contains(err.Error(), "held by a kernel driver") {
		t.Fatalf("the error says %q, which does not say what is in the way", err)
	}
}

// ⛔ A READ LOOP WITH NO CEILING IS A READ LOOP. The descriptor is served by a
// kernel this process does not control, and one that kept answering would
// otherwise be read until memory ran out.
func TestPreadAllStopsAtItsCeiling(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = closeFD(fds[0]); _ = closeFD(fds[1]) })
	// pread on a socket is ESPIPE, which is the error path rather than the
	// ceiling — the ceiling needs a descriptor that answers forever, and
	// /dev/zero is exactly that.
	if _, err := preadAll(fds[0]); err == nil {
		t.Fatal("preadAll on a socket reported success")
	}

	z, err := os.Open("/dev/zero")
	if err != nil {
		t.Skipf("no /dev/zero here: %v", err)
	}
	t.Cleanup(func() { _ = z.Close() })
	if _, err := preadAll(int(z.Fd())); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("preadAll over an endless descriptor = %v, want ErrBadPayload", err)
	}
}

// ISOPacketSize is portable, so its test lives in usbfs_test.go — a test for a
// portable function that only runs on one GOOS leaves it uncovered everywhere
// else, which the coverage gate catches on the OTHER lane.

func TestSubmitISORefusesAnEndpointThatCarriesNothing(t *testing.T) {
	_, h := openCamera(t)
	// MaxPacketSize 0 decodes to 0 bytes an interval, which is not a transfer.
	if err := h.SubmitISO(USBEndpoint{Address: 0x81, Attributes: 0x05}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("an endpoint carrying 0 bytes an interval = %v", err)
	}
}

// ⛔⛔ A RECONNECT THAT FAILS MUST BE REPORTED. This was `_ = ioctl(…)`, and a
// Close that silently failed to give the driver back left the phone's camera
// with no driver on its streaming interface while returning nil. The damage is
// to something SHARED, which is exactly when silence is worst.
func TestCloseReportsADriverItCouldNotGiveBack(t *testing.T) {
	_, h := openCamera(t)
	old := ioctl
	t.Cleanup(func() { ioctl = old })

	// Claim through a busy interface so there is something detached to give
	// back, then refuse the reconnect.
	busy := true
	ioctl = func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case usbdevfsClaimInterface:
			if busy {
				return syscall.EBUSY
			}
			return nil
		case usbdevfsIoctl:
			a := *(*usbdevfsIoctlArg)(arg)
			if a.IoctlCode == int32(ioc(iocNone, usbdevfsConnect, 0)) {
				return syscall.ENODEV
			}
			busy = false
			return nil
		}
		return nil
	}
	if err := h.ClaimInterface(1); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if got := h.Detached(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Detached = %v, want [1] — the debt must be visible to the caller", got)
	}
	err := h.Close()
	if !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("Close = %v; a reconnect that failed must be reported", err)
	}
	if !strings.Contains(err.Error(), "could not be given back") {
		t.Fatalf("the error says %q, which does not say what was left behind", err)
	}
	// ⛔ AND IT MUST NOT KEEP REPORTING. Close is idempotent, and a deferred
	// one after an explicit one is ordinary.
	if err := h.Close(); err != nil {
		t.Fatalf("the second Close = %v, want nil", err)
	}
}

// Detached is empty when nothing was taken, which is the common case: an
// interface no driver holds is claimed without detaching anything.
func TestDetachedIsEmptyWhenNothingWasTaken(t *testing.T) {
	_, h := openCamera(t)
	old := ioctl
	t.Cleanup(func() { ioctl = old })
	ioctl = func(int, uintptr, unsafe.Pointer) error { return nil }
	if err := h.ClaimInterface(1); err != nil {
		t.Fatalf("ClaimInterface: %v", err)
	}
	if got := h.Detached(); len(got) != 0 {
		t.Fatalf("Detached = %v on an interface that was free", got)
	}
}

// closeFD failing is the last branch of Close, and it only loses to a reconnect
// failure: a descriptor this process could not close is its own problem, while
// an interface left without a driver is the whole phone's.
func TestCloseReportsADescriptorItCouldNotClose(t *testing.T) {
	_, h := openCamera(t)
	old := closeFD
	t.Cleanup(func() { closeFD = old })
	closeFD = func(int) error { return syscall.EIO }
	if err := h.Close(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Close = %v, want the close failure", err)
	}
}
