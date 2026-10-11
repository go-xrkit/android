// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"fmt"
	"unsafe"
)

// This file is the USB layer, and it exists for ONE question.
//
// # Why there is any of this
//
// A headset's camera is how a head gets followed — the VITURE Beast publishes no
// orientation and go-xrkit/xrkit/headflow recovers the yaw from its pictures
// instead. On this phone the census in census.go found the only two routes
// Android offers are both shut: camera2 enumerates no external camera, and every
// UVC streaming endpoint is ISOCHRONOUS, which UsbDeviceConnection cannot submit
// at all.
//
// What is left is usbfs: the kernel's own USB interface, reached by ioctl on the
// descriptor Android hands out from UsbDeviceConnection.getFileDescriptor().
// That is plain syscalls, so a CGO-free Go process can do it — on paper.
//
// # ⛔ AND WHETHER ANDROID ACTUALLY ALLOWS IT IS UNKNOWN
//
// SELinux governs what an untrusted application may do with a usbfs descriptor,
// and nothing documents whether USBDEVFS_SUBMITURB is among them. Every piece of
// a UVC implementation — descriptor parsing, probe/commit negotiation, frame
// assembly, MJPEG decoding — is worthless if that one ioctl is refused.
//
// So this package does the smallest thing that answers it, and stops there.
// cmd/xrusb opens the device, reads its descriptors, claims an interface and
// submits ONE isochronous request, reporting which of those four the kernel
// allowed. Nothing above it is written until the answer is yes.

// ErrUSBPermissionDenied reports the user declining the system's USB permission
// dialog, or an application asking for a device it was never granted.
//
// It is a decision rather than a failure: opening a USB device shows a dialog
// naming the device and the application, and a person may say no.
//
// A device that is no longer attached reports [ErrNotFound] instead, which this
// package already had: a headset on the end of a cable is unplugged more often
// than anything else here, and a second sentinel for it would be two names for
// one condition.
var ErrUSBPermissionDenied = errors.New("android: permission to open the USB device was refused")

// The _IOC encoding Linux builds ioctl numbers with, from
// include/uapi/asm-generic/ioctl.h. usbfs uses 'U' as its type.
//
// ⛔ THE NUMBER CARRIES THE STRUCT'S SIZE, so it is COMPUTED here rather than
// copied. A Go mirror of a C struct that is one field out produces a different
// ioctl number, and the kernel then answers EINVAL — or, if that number happens
// to be another command, does something else entirely. Deriving it from the Go
// struct makes the two agree by construction, and the suite asserts the result
// against the values the kernel's own headers produce.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
	usbfsIOC = 'U'
)

func ioc(dir, nr, size int) uintptr {
	return uintptr(dir)<<30 | uintptr(size)<<16 | uintptr(usbfsIOC)<<8 | uintptr(nr)
}

// The usbfs commands this package issues. The direction bits read backwards on
// purpose: usbfs declares them from the KERNEL's point of view, so what
// userspace writes is _IOR.
var (
	// usbdevfsClaimInterface takes an interface away from any kernel driver so
	// this process may use it.
	usbdevfsClaimInterface = ioc(iocRead, 15, 4)
	// usbdevfsReleaseInterface gives it back.
	usbdevfsReleaseInterface = ioc(iocRead, 16, 4)
	// usbdevfsSetInterface selects an alternate setting, which for a UVC
	// streaming interface is how its bandwidth is chosen.
	usbdevfsSetInterface = ioc(iocRead, 4, setInterfaceSize)
	// usbdevfsSubmitURB queues one transfer. THE QUESTION THIS PACKAGE ASKS is
	// whether an untrusted Android application may issue it with an
	// isochronous type.
	usbdevfsSubmitURB = ioc(iocRead, 10, urbSize)
	// usbdevfsReapURBNDelay collects a completed transfer without blocking. It
	// takes a POINTER to a pointer, so its size is a pointer's.
	usbdevfsReapURBNDelay = ioc(iocWrite, 13, pointerSize)
	// usbdevfsDiscardURB cancels one that has not completed. It carries
	// nothing, so it has no size and no direction.
	usbdevfsDiscardURB = ioc(iocNone, 11, 0)

	// usbdevfsIoctl carries ANOTHER ioctl down to one interface's driver. It is
	// the only route to disconnect and reconnect, which userspace may not issue
	// directly.
	//
	// ⛔ IT IS _IOWR, NOT _IOR. The kernel writes back through it, and the
	// direction bits are part of the number: getting them wrong names a
	// different command rather than failing.
	usbdevfsIoctl = ioc(iocRead|iocWrite, 18, ioctlArgSize)

	// usbdevfsControl performs one control transfer, which is how a UVC device
	// is asked what it can do and told what to do: the whole probe/commit
	// negotiation is control transfers on the streaming interface.
	usbdevfsControl = ioc(iocRead|iocWrite, 0, ctrlTransferSize)
)

// The codes carried INSIDE usbdevfsIoctl. They are values in a struct rather
// than commands this process issues, which is why they are plain small numbers.
const (
	// usbdevfsDisconnect detaches whatever kernel driver holds an interface.
	//
	// ⭐ IT IS WHY EBUSY IS NOT THE END. A UVC camera is held by the kernel's
	// own uvcvideo, so CLAIMINTERFACE answers "device or resource busy" —
	// measured on a Pixel 11 Pro Fold. This is the documented way out, the one
	// libusb's detach_kernel_driver takes: no root, no policy change, and
	// reversible.
	usbdevfsDisconnect = 22
	// usbdevfsConnect gives the interface back to the kernel.
	//
	// ⚠ IT IS MORE THAN GOOD MANNERS. Detaching uvcvideo takes /dev/video0
	// away from the whole phone for as long as this process holds the
	// interface, so a process that exited without reconnecting would leave the
	// camera unusable to everything until it was unplugged.
	usbdevfsConnect = 23
)

// URB transfer types, from usbdevice_fs.h. They are NOT the same numbering as
// an endpoint's bmAttributes — isochronous is 0 here and 1 there — which is
// exactly the kind of difference that produces a working-looking call that does
// the wrong thing.
const (
	urbTypeIsochronous = 0
	urbTypeInterrupt   = 1
	urbTypeControl     = 2
	urbTypeBulk        = 3
)

// urbTypes maps an endpoint's transfer type onto the URB type usbfs wants.
//
// ⛔ THE TWO NUMBERINGS DISAGREE AND THE OVERLAP IS SILENT. An endpoint whose
// bmAttributes say 1 is isochronous; a URB whose type is 1 is INTERRUPT. Passing
// one where the other is expected submits a transfer of the wrong kind to a real
// endpoint, which fails in whatever way that endpoint fails — never in a way
// that says "you used the wrong table".
//
// ⭐ IT IS A TABLE RATHER THAN A FUNCTION THAT CAN FAIL. A transfer type is the
// low two bits of bmAttributes and is therefore always one of these four, so an
// error return would be a branch NO INPUT CAN REACH — dead code wearing a
// safety belt. The 100% coverage gate is what found it.
var urbTypes = [4]int{
	USBTransferControl:     urbTypeControl,
	USBTransferIsochronous: urbTypeIsochronous,
	USBTransferBulk:        urbTypeBulk,
	USBTransferInterrupt:   urbTypeInterrupt,
}

// urbTypeFor is total: it masks to the two bits a transfer type has.
func urbTypeFor(t USBTransferType) int {
	return urbTypes[t&0x3]
}

// USBDescriptors is a device's raw descriptor bytes, as usbfs serves them: the
// device descriptor followed by every configuration descriptor and everything
// nested inside them.
//
// They are kept RAW because the class-specific parts are what a camera is
// described by — UVC's format and frame descriptors live inside the interface
// descriptors and no general USB parser reports them — and because a byte slice
// read from a kernel is a thing a test can hold.
type USBDescriptors []byte

// Bounds on a descriptor blob.
const (
	// MinDescriptorBytes is the smallest a blob can be and still hold a device
	// descriptor. Anything shorter is a short read rather than a device.
	MinDescriptorBytes = 18

	// MaxDescriptorBytes bounds how much is read from a descriptor.
	//
	// ⛔ A READ LOOP WITH NO CEILING IS A READ LOOP. usbfs serves a few hundred
	// bytes, and 64 KiB is far past any real device -- but the thing on the
	// other end is a descriptor this process was handed, and a kernel that kept
	// answering would otherwise be read until memory ran out.
	MaxDescriptorBytes = 64 << 10
)

// Device reports the device descriptor's own fields: the vendor and product
// ids, and the device's class triple.
//
// It reads the first 18 bytes and nothing else, so it is valid for any blob
// long enough to be a device at all.
func (d USBDescriptors) Device() (vendor, product, class, subclass, protocol int, err error) {
	if len(d) < MinDescriptorBytes {
		return 0, 0, 0, 0, 0, fmt.Errorf("%w: %d descriptor bytes, want at least %d",
			ErrShortPayload, len(d), MinDescriptorBytes)
	}
	if d[1] != descriptorTypeDevice {
		return 0, 0, 0, 0, 0, fmt.Errorf("%w: the first descriptor is type %#02x, want a "+
			"device (%#02x)", ErrBadPayload, d[1], descriptorTypeDevice)
	}
	class, subclass, protocol = int(d[4]), int(d[5]), int(d[6])
	vendor = int(d[8]) | int(d[9])<<8
	product = int(d[10]) | int(d[11])<<8
	return vendor, product, class, subclass, protocol, nil
}

// Descriptor types this package names, from the USB specification.
const (
	descriptorTypeDevice    = 0x01
	descriptorTypeConfig    = 0x02
	descriptorTypeInterface = 0x04
	descriptorTypeEndpoint  = 0x05
)

// Walk calls fn for every descriptor in the blob, with its type and its whole
// bytes including the two-byte header.
//
// ⛔ A DESCRIPTOR OF LENGTH ZERO WOULD LOOP FOREVER, and a length running past
// the end would read somebody else's memory. Both are refused: a kernel will not
// produce either, and this parses bytes that crossed a process boundary.
func (d USBDescriptors) Walk(fn func(typ int, b []byte) bool) error {
	for i := 0; i < len(d); {
		if len(d)-i < 2 {
			return fmt.Errorf("%w: %d trailing byte at offset %d is not a descriptor header",
				ErrShortPayload, len(d)-i, i)
		}
		n := int(d[i])
		switch {
		case n < 2:
			return fmt.Errorf("%w: descriptor at offset %d says it is %d bytes long",
				ErrBadPayload, i, n)
		case i+n > len(d):
			return fmt.Errorf("%w: descriptor at offset %d claims %d bytes with %d left",
				ErrShortPayload, i, n, len(d)-i)
		}
		if !fn(int(d[i+1]), d[i:i+n]) {
			return nil
		}
		i += n
	}
	return nil
}

// Interfaces returns every interface descriptor's identity, in the order the
// device reports them: number, alternate setting and class triple.
//
// It is how a caller finds the UVC streaming interface and its alternate
// settings without this package growing a full USB parser.
func (d USBDescriptors) Interfaces() ([]USBInterface, error) {
	var out []USBInterface
	var cur *USBInterface
	err := d.Walk(func(typ int, b []byte) bool {
		switch typ {
		case descriptorTypeInterface:
			if len(b) < 9 {
				return true // too short to be one; the kernel will not emit it
			}
			out = append(out, USBInterface{
				Number: int(b[2]), Alternate: int(b[3]),
				Class: int(b[5]), Subclass: int(b[6]), Protocol: int(b[7]),
			})
			cur = &out[len(out)-1]
		case descriptorTypeEndpoint:
			if cur == nil || len(b) < 7 {
				return true
			}
			cur.Endpoints = append(cur.Endpoints, USBEndpoint{
				Address:       int(b[2]),
				Attributes:    int(b[3]),
				MaxPacketSize: int(b[4]) | int(b[5])<<8,
				Interval:      int(b[6]),
			})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ISOPacketsPerURB is how many isochronous packets one request carries.
//
// A UVC camera is scheduled one packet per microframe, so 8 packets is a
// millisecond of video. It is small deliberately: this package submits ONE
// request to find out whether the kernel accepts it at all, and a large request
// that failed for being large would answer a different question.
const ISOPacketsPerURB = 8

// The usbfs structures, mirroring include/uapi/linux/usbdevice_fs.h.
//
// ⛔ THE LAYOUT IS THE CONTRACT. These cross into the kernel by address, so a
// field of the wrong width or in the wrong place is not a compile error and not
// a runtime error — it is a transfer submitted against the wrong endpoint with
// the wrong length. The sizes below are asserted in the suite against the ioctl
// numbers the kernel's headers produce, which is the one check that catches a
// mirror that drifted.
type (
	// usbdevfsSetInterfaceArg is struct usbdevfs_setinterface.
	usbdevfsSetInterfaceArg struct {
		Interface  uint32
		AltSetting uint32
	}

	// usbdevfsCtrlTransfer is struct usbdevfs_ctrltransfer.
	//
	// ⛔ THE POINTER IS ALIGNED, AND THAT IS IN THE SIZE. The six fields before
	// it come to twelve bytes; the pointer sits at sixteen on a 64-bit kernel,
	// making the struct twenty-four rather than twenty. The ioctl number
	// carries that size, so a mirror that packed it tightly would name a
	// different command.
	usbdevfsCtrlTransfer struct {
		RequestType uint8
		Request     uint8
		Value       uint16
		Index       uint16
		Length      uint16
		Timeout     uint32
		Data        uintptr
	}

	// usbdevfsIoctlArg is struct usbdevfs_ioctl: an ioctl addressed to one
	// interface's driver rather than to the device.
	usbdevfsIoctlArg struct {
		Ifno      int32
		IoctlCode int32
		Data      uintptr
	}

	// usbdevfsISOPacketDesc is struct usbdevfs_iso_packet_desc: one packet of
	// an isochronous request, which the kernel fills in on completion.
	usbdevfsISOPacketDesc struct {
		Length       uint32
		ActualLength uint32
		Status       uint32
	}

	// usbdevfsURB is struct usbdevfs_urb, WITHOUT its trailing flexible array
	// of packet descriptors. Those are allocated after it; see isoURB.
	usbdevfsURB struct {
		Type            uint8
		Endpoint        uint8
		Status          int32
		Flags           uint32
		Buffer          uintptr
		BufferLength    int32
		ActualLength    int32
		StartFrame      int32
		NumberOfPackets int32
		ErrorCount      int32
		Signr           uint32
		UserContext     uintptr
	}
)

// The sizes the ioctl numbers are built from. unsafe.Sizeof keeps them in step
// with the structs above on every word size, which is why the numbers are not
// written out: a 32-bit build needs different ones and would otherwise get the
// 64-bit values silently.
const (
	setInterfaceSize = int(unsafe.Sizeof(usbdevfsSetInterfaceArg{}))
	urbSize          = int(unsafe.Sizeof(usbdevfsURB{}))
	pointerSize      = int(unsafe.Sizeof(uintptr(0)))
	isoPacketSize    = int(unsafe.Sizeof(usbdevfsISOPacketDesc{}))
	ioctlArgSize     = int(unsafe.Sizeof(usbdevfsIoctlArg{}))
)

// ctrlTransferSize is the size the control ioctl's number carries.
const ctrlTransferSize = int(unsafe.Sizeof(usbdevfsCtrlTransfer{}))

// Control transfer directions and recipients, from the USB specification's
// bmRequestType.
const (
	// ctrlToDevice is a transfer that carries data to the device.
	ctrlToDevice = 0x00
	// ctrlToHost is one that brings data back.
	ctrlToHost = 0x80
	// ctrlClass addresses the device's class driver rather than USB itself,
	// which is what every UVC request is.
	ctrlClass = 0x20
	// ctrlInterface addresses one interface, named in wIndex.
	ctrlInterface = 0x01
)

// ControlTimeout bounds one control transfer.
//
// ⚠ A CONTROL TRANSFER WITH NO TIMEOUT CAN HANG THE CALLER FOREVER. A camera
// that stops answering mid-negotiation would otherwise take the goroutine with
// it, and a probe that hangs is worse than one that fails: it reports nothing
// at all. One second is far longer than a camera takes to answer a nine-byte
// request.
const ControlTimeout = 1000

// ControlIn builds the bmRequestType of a class request that brings data back
// from one interface, which is every UVC GET.
func ControlIn() uint8 { return ctrlToHost | ctrlClass | ctrlInterface }

// ControlOut builds the bmRequestType of a class request that carries data to
// one interface, which is every UVC SET.
func ControlOut() uint8 { return ctrlToDevice | ctrlClass | ctrlInterface }
