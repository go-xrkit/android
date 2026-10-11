// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Indirections the suite replaces to reach the branches a real kernel will not
// produce on demand. ioctl is the one that matters: every question this file
// asks is an ioctl, and the ANSWER being a refusal is the expected outcome.
var (
	ioctl           = rawIoctl
	ioctlN          = rawIoctlN
	readDescriptors = preadAll
)

// preadAll reads a descriptor's whole contents from offset zero.
//
// ⛔⛔ IT READS THE DESCRIPTOR, NOT A PATH TO IT. This was os.ReadFile on
// /proc/self/fd/N, which REOPENS the file — and reopening /dev/bus/usb/… is
// exactly what an Android application may not do:
//
//	STOPPED at descriptors: open /proc/self/fd/6: permission denied
//
// The whole point of being handed a descriptor over SCM_RIGHTS is that the open
// already happened, in a component allowed to make it. Going back through the
// filesystem throws that away and asks for the permission again — from the one
// process that cannot have it.
//
// pread rather than read, so the file offset is left alone: the ioctls that
// follow share this descriptor.
func preadAll(fd int) ([]byte, error) {
	out := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for off := int64(0); off < MaxDescriptorBytes; {
		n, err := unix.Pread(fd, buf, off)
		if n > 0 {
			out = append(out, buf[:n]...)
			off += int64(n)
		}
		if err != nil {
			return nil, err
		}
		if n < len(buf) {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: more than %d descriptor bytes", ErrBadPayload, MaxDescriptorBytes)
}

// rawIoctlN issues one ioctl and keeps its RETURN VALUE, which some usbfs
// commands use: the control transfer answers with the number of bytes that
// moved, and discarding it would leave a caller reading whatever happened to be
// in its buffer already.
func rawIoctlN(fd int, req uintptr, arg unsafe.Pointer) (int, error) {
	n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// rawIoctl is the same call for the commands whose answer is only success or
// failure. It delegates, so there is ONE place the errno is read.
func rawIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, err := rawIoctlN(fd, req, arg)
	return err
}

// A USBHandle is one USB device this application was given permission to open,
// held as the kernel's own descriptor.
//
// ⛔ IT IS NOT A STREAM AND NOT AN API TO BUILD ON. It exists to answer whether
// Android lets an untrusted application drive usbfs at all — see usbfs.go. Every
// method here is a step of that question, and the package stops when the
// question is answered.
//
// A USBHandle is safe for concurrent use; the device it names is not, and two
// callers claiming the same interface will have the kernel refuse the second.
type USBHandle struct {
	name string

	mu     sync.Mutex
	fd     int
	closed bool
	// claimed is the interfaces to release on Close, in the order taken.
	claimed []int
	// detached is the interfaces whose kernel driver was disconnected to get
	// them. Close reconnects every one: /dev/video0 is gone for the WHOLE PHONE
	// while they are detached, so leaving them that way would make the camera
	// unusable to everything until it was unplugged.
	detached []int
	// session is the connection the host lent the descriptor over. It is held
	// open because the HOST holds the UsbDeviceConnection: dropping the socket
	// lets the host release the device, and the descriptor here then names
	// nothing.
	session *session
}

// OpenUSB asks the host to open a USB device and lends its descriptor.
//
// ⚠ IT PUTS A DIALOG IN FRONT OF THE USER, naming the device and this
// application, and they may say no — which is reported as
// [ErrUSBPermissionDenied] and is a decision rather than a failure. A device
// that is no longer attached is [ErrNotFound].
//
// Close the handle when done: it holds a socket, a descriptor, and the host's
// claim on the device.
func OpenUSB(ctx context.Context, d USBDevice) (*USBHandle, error) {
	if d.Name == "" {
		return nil, fmt.Errorf("%w: a USB device with no name cannot be opened; use one "+
			"from USBDevices", ErrInvalidOption)
	}
	name := wallSocketName()
	if name == "" {
		return nil, fmt.Errorf("%w: no wall host socket to dial", ErrUnsupported)
	}
	uc, err := dialHost(name)
	if err != nil {
		return nil, fmt.Errorf("%w: dialling the wall host on @%s: %w", ErrUnsupported, name, err)
	}
	s := &session{uc: uc, fc: newFDConn(uc), replies: make(chan reply, repliesDepth), done: make(chan struct{})}
	go s.pump()

	// requestReply, not request: THE ANSWER IS THE DESCRIPTOR, and request keeps
	// only the body. Repeating the handshake here instead would duplicate its
	// send-failure path onto a write nothing can make fail on demand.
	r, err := s.requestReply(ctx, MsgOpenUSBDevice, EncodeOpenUSBDevice(d.Name), MsgUSBHandle)
	if err != nil {
		s.shutdown(ErrClosed)
		return nil, err
	}
	fd := r.fd
	if fd < 0 {
		s.shutdown(ErrClosed)
		return nil, fmt.Errorf("android: the host answered MsgUSBHandle with no descriptor")
	}
	return &USBHandle{name: d.Name, fd: fd, session: s}, nil
}

// Name is the device node this handle was opened for.
func (h *USBHandle) Name() string { return h.name }

// Detached returns the interfaces whose kernel driver this handle took away.
//
// ⛔⛔ IT IS NOT BOOKKEEPING, IT IS A DEBT. Taking a UVC streaming interface
// from uvcvideo is what makes the route work at all, and USBDEVFS_CONNECT does
// NOT reliably undo it: on a Pixel 11 Pro Fold the reconnect ioctl SUCCEEDS and
// the driver still does not rebind —
//
//	before:  1-1.1:1.0 driver=uvcvideo   1-1.1:1.1 driver=uvcvideo
//	after:   1-1.1:1.0 driver=uvcvideo   1-1.1:1.1 driver=
//
// /dev/video0 survives, because the control interface still carries it, but the
// streaming interface is left with no driver until the device is UNPLUGGED AND
// PLUGGED BACK. A caller that shares the camera with anything else on the phone
// needs to know that, which is why this is reported rather than hidden.
func (h *USBHandle) Detached() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.detached...)
}

// Descriptors reads the device's raw descriptor blob.
//
// ⭐ IT IS A PLAIN read(2), NOT AN IOCTL. usbfs serves the descriptors as the
// file's contents, so this is the one thing here that works without the kernel
// granting anything beyond the open — which makes it the right FIRST step of
// the probe: a failure here is the descriptor itself being useless, and a
// success narrows every later failure to the ioctl that produced it.
func (h *USBHandle) Descriptors() (USBDescriptors, error) {
	h.mu.Lock()
	fd, closed := h.fd, h.closed
	h.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	b, err := readDescriptors(fd)
	if err != nil {
		return nil, fmt.Errorf("android: reading the USB descriptors of %s: %w", h.name, err)
	}
	if len(b) < MinDescriptorBytes {
		return nil, fmt.Errorf("%w: %s served %d descriptor bytes", ErrShortPayload, h.name, len(b))
	}
	return b, nil
}

// ClaimInterface takes an interface from whatever kernel driver holds it, which
// is required before any transfer on its endpoints.
//
// On a UVC camera the kernel's own uvcvideo driver usually holds it, and
// whether Android lets an application take it away is part of the question.
func (h *USBHandle) ClaimInterface(n int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	arg := uint32(n)
	err := ioctl(h.fd, usbdevfsClaimInterface, unsafe.Pointer(&arg))
	if errors.Is(err, syscall.EBUSY) {
		// ⭐ EBUSY IS NOT A REFUSAL, IT IS AN OCCUPANT. A UVC camera is held by
		// the kernel's own uvcvideo, which publishes /dev/video0 from it —
		// measured on a Pixel 11 Pro Fold, and exactly where the probe stopped.
		// Detaching is the documented way past it, needs no privilege at all,
		// and [USBHandle.Close] puts the driver back.
		if derr := h.driverIoctl(n, usbdevfsDisconnect); derr != nil {
			return fmt.Errorf("android: interface %d of %s is held by a kernel driver that "+
				"would not let go: %w (claiming it reported %w)", n, h.name, derr, err)
		}
		h.detached = append(h.detached, n)
		err = ioctl(h.fd, usbdevfsClaimInterface, unsafe.Pointer(&arg))
	}
	if err != nil {
		return fmt.Errorf("android: claiming interface %d of %s: %w", n, h.name, err)
	}
	h.claimed = append(h.claimed, n)
	return nil
}

// driverIoctl sends one command to the driver bound to an interface, which is
// how disconnect and reconnect are reached: neither has a number userspace may
// issue on its own.
//
// The caller holds h.mu.
func (h *USBHandle) driverIoctl(iface, code int) error {
	arg := usbdevfsIoctlArg{Ifno: int32(iface), IoctlCode: int32(ioc(iocNone, code, 0))}
	return ioctl(h.fd, usbdevfsIoctl, unsafe.Pointer(&arg))
}

// SetAltSetting selects an alternate setting of an interface. For a UVC
// streaming interface this is how bandwidth is chosen, and setting 0 is the
// zero-bandwidth one every UVC device has.
func (h *USBHandle) SetAltSetting(iface, alt int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	arg := usbdevfsSetInterfaceArg{Interface: uint32(iface), AltSetting: uint32(alt)}
	if err := ioctl(h.fd, usbdevfsSetInterface, unsafe.Pointer(&arg)); err != nil {
		return fmt.Errorf("android: selecting alternate %d of interface %d on %s: %w",
			alt, iface, h.name, err)
	}
	return nil
}

// SubmitISO queues ONE isochronous request against an endpoint, and reports
// what the kernel made of it.
//
// ⛔ THIS IS THE QUESTION THE WHOLE FILE EXISTS FOR. Android's Java USB API
// cannot submit an isochronous transfer at all; whether SELinux lets an
// untrusted application do it through usbfs is undocumented, and every line of
// a UVC implementation depends on the answer.
//
// It does not reap the result: a submission that the kernel ACCEPTS has already
// answered the question, and waiting for pixels would confuse "the transfer was
// allowed" with "the camera was streaming", which are different findings.
func (h *USBHandle) SubmitISO(endpoint USBEndpoint) error {
	typ := urbTypeFor(endpoint.Type())
	// ⛔ THE DECODED SIZE, NOT THE RAW FIELD. See USBEndpoint.ISOPacketSize:
	// passing wMaxPacketSize had the kernel answer EMSGSIZE, which reads as a
	// refusal and was not one.
	packet := endpoint.ISOPacketSize()
	if packet <= 0 {
		return fmt.Errorf("%w: endpoint %#02x carries %d bytes an interval",
			ErrInvalidOption, endpoint.Address, packet)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}

	// The URB and its packet descriptors are ONE allocation, because the kernel
	// reads them as one struct with a flexible array at the end. Two
	// allocations would put the packets wherever Go put them and the kernel
	// would read whatever follows the URB.
	n := ISOPacketsPerURB
	buf := make([]byte, packet*n)
	raw := make([]byte, urbSize+isoPacketSize*n)
	u := (*usbdevfsURB)(unsafe.Pointer(&raw[0]))
	u.Type = uint8(typ)
	u.Endpoint = uint8(endpoint.Address)
	u.Buffer = uintptr(unsafe.Pointer(&buf[0]))
	u.BufferLength = int32(len(buf))
	u.NumberOfPackets = int32(n)
	for i := 0; i < n; i++ {
		p := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&raw[urbSize+isoPacketSize*i]))
		p.Length = uint32(packet)
	}
	if err := ioctl(h.fd, usbdevfsSubmitURB, unsafe.Pointer(&raw[0])); err != nil {
		return fmt.Errorf("android: submitting an isochronous request to endpoint %#02x "+
			"of %s: %w", endpoint.Address, h.name, err)
	}
	// Cancel it straight away: the question was whether the kernel would take
	// it, and leaving a request outstanding against a camera nobody is reading
	// would stream into a buffer this process is about to drop.
	_ = ioctl(h.fd, usbdevfsDiscardURB, unsafe.Pointer(&raw[0]))
	// buf and raw must outlive the ioctls above, and nothing else refers to
	// them; naming them here is what keeps the compiler from deciding
	// otherwise.
	runtime.KeepAlive(buf)
	runtime.KeepAlive(raw)
	return nil
}

// Close releases every interface this handle claimed, closes the descriptor and
// drops the host connection, which lets the host release the device. It is
// idempotent.
func (h *USBHandle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	fd, claimed, detached := h.fd, h.claimed, h.detached
	h.mu.Unlock()

	for _, n := range claimed {
		arg := uint32(n)
		_ = ioctl(fd, usbdevfsReleaseInterface, unsafe.Pointer(&arg))
	}
	// ⛔⛔ GIVING THE DRIVER BACK IS THE PART THAT MUST BE REPORTED. Releasing a
	// claim affects nobody else; leaving an interface DETACHED leaves the
	// phone's camera half dismantled — /dev/video0 still present because the
	// control interface is bound, and the streaming interface with no driver at
	// all:
	//
	//	1-1.1:1.0 driver=uvcvideo
	//	1-1.1:1.1 driver=              ← measured, after a Close that returned nil
	//
	// This was `_ = ioctl(…)`. A reconnect that failed silently left the device
	// in that state and reported success, which is the worst of both: the
	// damage is to something SHARED, and nothing said so. The first failure is
	// now returned, and every interface is still attempted.
	var first error
	for _, n := range detached {
		arg := usbdevfsIoctlArg{Ifno: int32(n), IoctlCode: int32(ioc(iocNone, usbdevfsConnect, 0))}
		if err := ioctl(fd, usbdevfsIoctl, unsafe.Pointer(&arg)); err != nil && first == nil {
			first = fmt.Errorf("android: interface %d of %s was taken from its kernel driver "+
				"and could not be given back (%w); that interface is left with no driver "+
				"until the device is unplugged", n, h.name, err)
		}
	}
	if err := closeFD(fd); err != nil && first == nil {
		first = err
	}
	h.session.shutdown(ErrClosed)
	return first
}

// Control performs one control transfer and returns how many bytes moved.
//
// ⛔⛔ data IS WRITTEN INTO BY THE KERNEL ON AN IN TRANSFER, and its length is
// what the device is told to send. A slice shorter than the device's answer
// gets a short read; one longer invites it to overrun nothing, because the
// length in the request is the ceiling. Either way the number returned is what
// actually moved, and callers must use it rather than len(data).
//
// It is the whole of UVC's negotiation: asking a camera what it can do and
// telling it what to do are both control transfers on its streaming interface.
func (h *USBHandle) Control(requestType, request uint8, value, index uint16, data []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrClosed
	}
	t := usbdevfsCtrlTransfer{
		RequestType: requestType,
		Request:     request,
		Value:       value,
		Index:       index,
		Length:      uint16(len(data)),
		Timeout:     ControlTimeout,
	}
	if len(data) > 0 {
		t.Data = uintptr(unsafe.Pointer(&data[0]))
	}
	n, err := ioctlN(h.fd, usbdevfsControl, unsafe.Pointer(&t))
	runtime.KeepAlive(data)
	if err != nil {
		return 0, fmt.Errorf("android: control transfer %#02x/%#02x value %#04x to %s: %w",
			requestType, request, value, h.name, err)
	}
	return n, nil
}
