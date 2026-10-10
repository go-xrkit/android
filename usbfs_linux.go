// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Indirections the suite replaces to reach the branches a real kernel will not
// produce on demand. ioctl is the one that matters: every question this file
// asks is an ioctl, and the ANSWER being a refusal is the expected outcome.
var (
	ioctl   = rawIoctl
	readAll = os.ReadFile
)

func rawIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
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
	b, err := readAll(fmt.Sprintf("/proc/self/fd/%d", fd))
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
	if err := ioctl(h.fd, usbdevfsClaimInterface, unsafe.Pointer(&arg)); err != nil {
		return fmt.Errorf("android: claiming interface %d of %s: %w", n, h.name, err)
	}
	h.claimed = append(h.claimed, n)
	return nil
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
	if endpoint.MaxPacketSize <= 0 {
		return fmt.Errorf("%w: endpoint %#02x has a maximum packet size of %d",
			ErrInvalidOption, endpoint.Address, endpoint.MaxPacketSize)
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
	buf := make([]byte, endpoint.MaxPacketSize*n)
	raw := make([]byte, urbSize+isoPacketSize*n)
	u := (*usbdevfsURB)(unsafe.Pointer(&raw[0]))
	u.Type = uint8(typ)
	u.Endpoint = uint8(endpoint.Address)
	u.Buffer = uintptr(unsafe.Pointer(&buf[0]))
	u.BufferLength = int32(len(buf))
	u.NumberOfPackets = int32(n)
	for i := 0; i < n; i++ {
		p := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&raw[urbSize+isoPacketSize*i]))
		p.Length = uint32(endpoint.MaxPacketSize)
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
	fd, claimed := h.fd, h.claimed
	h.mu.Unlock()

	for _, n := range claimed {
		arg := uint32(n)
		_ = ioctl(fd, usbdevfsReleaseInterface, unsafe.Pointer(&arg))
	}
	err := closeFD(fd)
	h.session.shutdown(ErrClosed)
	return err
}
