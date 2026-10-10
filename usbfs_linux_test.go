// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
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
	if want := int32(ep.MaxPacketSize * ISOPacketsPerURB); u.BufferLength != want {
		t.Fatalf("the URB's buffer is %d bytes, want %d", u.BufferLength, want)
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
	old := readAll
	t.Cleanup(func() { readAll = old })
	readAll = func(string) ([]byte, error) { return nil, syscall.EACCES }
	if _, err := h.Descriptors(); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Descriptors = %v, want EACCES", err)
	}
}

func TestAHostRefusalMapsOntoErrUSBPermissionDenied(t *testing.T) {
	if err := hostError(ErrorMessage{Code: codeUSBPermissionDenied, Detail: "no"}); !errors.Is(err, ErrUSBPermissionDenied) {
		t.Fatalf("code %d mapped to %v", codeUSBPermissionDenied, err)
	}
}
