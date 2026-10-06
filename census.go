// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import "fmt"

// This file is the CENSUS: what hardware the phone can see, asked of the host
// that needs no permission, no consent and no foreground service to answer.
//
// # Why a census at all, and why it is this one
//
// A head has to be followed for an XR ribbon to be worth anything, and a VITURE
// Beast WILL NOT SAY WHERE IT IS POINTING. It holds its own 3DOF tracking and
// anchors the picture it is given with it, but publishes no orientation:
// measured three ways on 2026-09-07 — listening for unsolicited frames, asking
// for the documented orientation stream, and sweeping every readable message
// with the head still and then moving — and none produced a number. See
// go-xrkit/xrkit/headflow.
//
// Its camera, however, is an ordinary UVC device, and a turn of the head is
// plainly visible in it. On macOS that camera is an AVFoundation device and the
// matter ends there. On Android it is not obvious that an application can reach
// it AT ALL, and there are exactly two routes:
//
//   - the camera2 API, if this phone's HAL enumerates external USB cameras.
//     Android defines LENS_FACING_EXTERNAL for precisely this and leaves
//     supporting it to the vendor, so it is a per-device question with no
//     documentation that settles it;
//   - the USB host API, claiming the UVC interface directly. Which is only
//     usable if the streaming endpoints are BULK: Android's Java USB API cannot
//     submit an isochronous transfer at all, and most UVC cameras are
//     isochronous.
//
// ⛔ SO THE ROUTE IS A MEASUREMENT, NOT A DESIGN DECISION. Writing the transport
// first and discovering afterwards that the phone never offered the frames is
// the expensive order. [Cameras] and [USBDevices] ask, and cmd/xrcam says which
// of the two is open — including when the answer is neither.

// CameraFacing says which way a camera points, as android.hardware.camera2's
// LENS_FACING.
type CameraFacing int

// The facings camera2 defines.
const (
	// FacingFront is the selfie camera.
	FacingFront CameraFacing = 0
	// FacingBack is the main camera.
	FacingBack CameraFacing = 1
	// FacingExternal is a camera that is not fixed to the device and may come
	// and go — a USB camera, which is what a headset's is.
	//
	// ⭐ THIS IS THE ONE THAT MATTERS HERE, and the one a phone may simply not
	// offer: supporting external cameras is left to the vendor's HAL.
	FacingExternal CameraFacing = 2
)

// String renders the facing.
func (f CameraFacing) String() string {
	switch f {
	case FacingFront:
		return "front"
	case FacingBack:
		return "back"
	case FacingExternal:
		return "external"
	}
	return fmt.Sprintf("facing(%d)", int(f))
}

// Camera is one camera the camera2 API can see.
type Camera struct {
	// ID is the camera2 id, which is what opens it. It is a small number as a
	// string for built-in cameras and is not necessarily one for an external.
	ID string
	// Facing is which way it points; see [FacingExternal].
	Facing CameraFacing
	// Width and Height are the largest still size it advertises, in pixels.
	// Zero when the host could not read its characteristics.
	Width, Height int
}

// External reports whether this camera is one that can be unplugged — the only
// kind a headset's can be.
func (c Camera) External() bool { return c.Facing == FacingExternal }

// String renders the camera for logs.
func (c Camera) String() string {
	return fmt.Sprintf("camera %q %s %dx%d", c.ID, c.Facing, c.Width, c.Height)
}

// USB class codes this package names, from the USB-IF class definitions.
const (
	// USBClassPerInterface is a device whose class is declared by each
	// interface rather than by the device. Every composite device is one, and a
	// headset that is a camera and something else is composite.
	USBClassPerInterface = 0x00
	// USBClassVideo is the Video class: UVC, which is what a webcam speaks and
	// what a headset's camera is.
	USBClassVideo = 0x0e
)

// USB video subclasses, from the UVC specification.
const (
	// USBSubclassVideoControl is the interface that describes the camera's
	// units and terminals. A UVC device has exactly one.
	USBSubclassVideoControl = 0x01
	// USBSubclassVideoStreaming is the interface that carries the frames, and
	// the one whose endpoints decide whether Android can read them.
	USBSubclassVideoStreaming = 0x02
)

// USBTransferType is an endpoint's transfer type, the low two bits of its
// bmAttributes.
type USBTransferType int

// The transfer types USB defines.
const (
	USBTransferControl     USBTransferType = 0
	USBTransferIsochronous USBTransferType = 1
	USBTransferBulk        USBTransferType = 2
	USBTransferInterrupt   USBTransferType = 3
)

// String renders the transfer type.
func (t USBTransferType) String() string {
	switch t {
	case USBTransferControl:
		return "control"
	case USBTransferIsochronous:
		return "isochronous"
	case USBTransferBulk:
		return "bulk"
	case USBTransferInterrupt:
		return "interrupt"
	}
	return fmt.Sprintf("transfer(%d)", int(t))
}

// USBEndpoint is one endpoint of a USB interface.
type USBEndpoint struct {
	// Address is bEndpointAddress: the endpoint number, with bit 7 set for an
	// IN endpoint.
	Address int
	// Attributes is bmAttributes, whose low two bits are the transfer type.
	Attributes int
	// MaxPacketSize is wMaxPacketSize for this endpoint's current setting.
	MaxPacketSize int
	// Interval is bInterval, the polling period.
	Interval int
}

// In reports whether this endpoint carries data towards the host. A camera's
// streaming endpoint is one.
func (e USBEndpoint) In() bool { return e.Address&0x80 != 0 }

// Type is the endpoint's transfer type.
func (e USBEndpoint) Type() USBTransferType { return USBTransferType(e.Attributes & 0x3) }

// String renders the endpoint for logs.
func (e USBEndpoint) String() string {
	dir := "out"
	if e.In() {
		dir = "in"
	}
	return fmt.Sprintf("ep %#02x %s %s max %d interval %d",
		e.Address, dir, e.Type(), e.MaxPacketSize, e.Interval)
}

// USBInterface is one interface of a USB device, in one alternate setting.
type USBInterface struct {
	// Number is bInterfaceNumber, and Alternate is bAlternateSetting.
	Number, Alternate int
	// Class, Subclass and Protocol are the interface's own class triple.
	Class, Subclass, Protocol int
	// Endpoints are the endpoints of this alternate setting.
	Endpoints []USBEndpoint
}

// Video reports whether this interface is a UVC one.
func (i USBInterface) Video() bool { return i.Class == USBClassVideo }

// VideoStreaming reports whether this is the UVC interface that carries frames.
func (i USBInterface) VideoStreaming() bool {
	return i.Video() && i.Subclass == USBSubclassVideoStreaming
}

// String renders the interface for logs.
func (i USBInterface) String() string {
	s := fmt.Sprintf("interface %d alt %d class %#02x/%#02x/%#02x",
		i.Number, i.Alternate, i.Class, i.Subclass, i.Protocol)
	if i.VideoStreaming() {
		s += " (UVC streaming)"
	} else if i.Video() {
		s += " (UVC control)"
	}
	for _, e := range i.Endpoints {
		s += "\n      " + e.String()
	}
	return s
}

// USBDevice is one device attached to the phone's USB host port.
type USBDevice struct {
	// Name is the device node the system calls it, which is what a USB
	// permission is granted against.
	Name string
	// VendorID and ProductID identify the device. VITURE is 0x35ca.
	VendorID, ProductID int
	// Manufacturer and Product are the device's own strings, when it has them.
	Manufacturer, Product string
	// Class, Subclass and Protocol are the DEVICE's class triple, which is
	// [USBClassPerInterface] for anything composite.
	Class, Subclass, Protocol int
	// Interfaces are every interface of every alternate setting.
	Interfaces []USBInterface
}

// VitureVendorID is VITURE's USB vendor id, recorded in
// go-xrkit/xrkit/glasses: every VITURE headset reports it.
const VitureVendorID = 0x35ca

// Viture reports whether this device is a VITURE one.
func (d USBDevice) Viture() bool { return d.VendorID == VitureVendorID }

// VideoStreaming returns the UVC streaming interfaces this device has, across
// every alternate setting.
func (d USBDevice) VideoStreaming() []USBInterface {
	var out []USBInterface
	for _, i := range d.Interfaces {
		if i.VideoStreaming() {
			out = append(out, i)
		}
	}
	return out
}

// BulkVideo reports whether any of this device's UVC streaming interfaces
// delivers frames over a BULK endpoint.
//
// ⛔ IT IS THE WHOLE QUESTION FOR THE USB ROUTE. Android's Java USB API submits
// control, bulk and interrupt transfers and NOTHING ELSE: there is no
// isochronous request in UsbDeviceConnection or UsbRequest, which is why every
// UVC library on Android carries a native libusb. A camera whose streaming
// endpoints are all isochronous cannot be read through that API at all, however
// much permission it is given.
func (d USBDevice) BulkVideo() bool {
	for _, i := range d.VideoStreaming() {
		for _, e := range i.Endpoints {
			if e.In() && e.Type() == USBTransferBulk {
				return true
			}
		}
	}
	return false
}

// String renders the device for logs.
func (d USBDevice) String() string {
	s := fmt.Sprintf("usb %s %04x:%04x %q %q class %#02x/%#02x/%#02x",
		d.Name, d.VendorID, d.ProductID, d.Manufacturer, d.Product,
		d.Class, d.Subclass, d.Protocol)
	for _, i := range d.Interfaces {
		s += "\n    " + i.String()
	}
	return s
}

// CameraRoute says how an application could reach a headset's camera on this
// phone, which is a measurement rather than a choice; see the file comment.
type CameraRoute int

// The routes, in the order they are preferred.
const (
	// RouteNone is no route at all: the phone offers no external camera and no
	// USB device that could be read.
	RouteNone CameraRoute = iota
	// RouteCamera2 is the camera2 API with an external camera, which is the
	// route to want: the platform does the UVC negotiation, the format
	// conversion and the buffering.
	RouteCamera2
	// RouteUSBBulk is claiming the UVC interface over the USB host API. It
	// needs the streaming endpoints to be bulk, and it means writing the UVC
	// negotiation by hand.
	RouteUSBBulk
	// RouteUSBIsochronous is a UVC device whose frames are isochronous. It is
	// listed because it is what a census will usually find, and NOT because it
	// is usable through Android's Java USB API — reaching it means usbfs ioctls
	// on the descriptor that API hands out.
	RouteUSBIsochronous
)

// String renders the route.
func (r CameraRoute) String() string {
	switch r {
	case RouteNone:
		return "none"
	case RouteCamera2:
		return "camera2 external"
	case RouteUSBBulk:
		return "USB host, bulk UVC"
	case RouteUSBIsochronous:
		return "USB, isochronous UVC"
	}
	return fmt.Sprintf("route(%d)", int(r))
}

// Usable reports whether this route can be taken with Android's own APIs and
// nothing else.
func (r CameraRoute) Usable() bool { return r == RouteCamera2 || r == RouteUSBBulk }

// Readable returns the interfaces of this device whose IN endpoints Android's
// Java USB API can actually carry — bulk and interrupt, never isochronous.
//
// ⭐ IT IS WHAT IS LEFT WHEN THE CAMERA IS CLOSED. A headset whose camera is
// isochronous is not a headset with nothing to read: the VITURE Beast also
// presents a CDC-ACM serial interface with bulk endpoints and a HID interface
// with interrupt ones, and both of those UsbDeviceConnection can read today.
// What travels on them is a separate question this package does not answer —
// see go-xrkit/xrkit/headflow on the Beast publishing no orientation — but
// "unreachable" and "reachable and silent" are different findings and a census
// that could not tell them apart would be worth little.
func (d USBDevice) Readable() []USBInterface {
	var out []USBInterface
	for _, i := range d.Interfaces {
		for _, e := range i.Endpoints {
			if e.In() && (e.Type() == USBTransferBulk || e.Type() == USBTransferInterrupt) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// ChooseRoute says how a headset's camera could be reached, given what the
// census found.
//
// It prefers camera2 whenever an external camera is there, because the platform
// then does the UVC negotiation, the format conversion and the buffering that
// the USB route would mean writing by hand.
//
// ⚠ IT ANSWERS FROM THE CENSUS, NOT FROM THE HEADSET. A phone with some other
// USB camera attached would be reported as reachable, and it would be — just not
// the headset's. [USBDevice.Viture] is how a caller narrows it, and cmd/xrcam
// prints the device it is talking about alongside the verdict.
func ChooseRoute(cams []Camera, devs []USBDevice) CameraRoute {
	for _, c := range cams {
		if c.External() {
			return RouteCamera2
		}
	}
	iso := false
	for _, d := range devs {
		if d.BulkVideo() {
			return RouteUSBBulk
		}
		if len(d.VideoStreaming()) > 0 {
			iso = true
		}
	}
	if iso {
		return RouteUSBIsochronous
	}
	return RouteNone
}
