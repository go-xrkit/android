// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// ⛔⛔ THE IOCTL NUMBERS CARRY A STRUCT SIZE, so a Go mirror that drifted from
// the kernel's C struct changes them — and a wrong ioctl number is not a
// compile error, not a type error, and not necessarily even an error. It is
// EINVAL if nothing else claims that number, and something ELSE HAPPENING if
// something does.
//
// These are the values include/uapi/linux/usbdevice_fs.h produces on 64-bit
// Linux, which is what Android arm64 is. They are written out HERE, in the
// test, and computed in the code: the two agreeing is the check. Writing them
// out in both places would be one transcription checked against itself.
func TestTheIoctlNumbersAreTheKernelsOwn(t *testing.T) {
	if pointerSize != 8 {
		t.Skipf("the published constants are 64-bit; this is a %d-bit build", pointerSize*8)
	}
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"USBDEVFS_CLAIMINTERFACE", usbdevfsClaimInterface, 0x8004550f},
		{"USBDEVFS_RELEASEINTERFACE", usbdevfsReleaseInterface, 0x80045510},
		{"USBDEVFS_SETINTERFACE", usbdevfsSetInterface, 0x80085504},
		{"USBDEVFS_SUBMITURB", usbdevfsSubmitURB, 0x8038550a},
		{"USBDEVFS_REAPURBNDELAY", usbdevfsReapURBNDelay, 0x4008550d},
		{"USBDEVFS_DISCARDURB", usbdevfsDiscardURB, 0x550b},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x — the Go mirror of its struct has drifted "+
				"from the kernel's", c.name, c.got, c.want)
		}
	}
}

// And the struct layouts the numbers are derived FROM, asserted field by field.
// A size that happens to come out right with two fields swapped would pass the
// test above and still submit a transfer against the wrong endpoint.
func TestTheUsbfsStructsMatchTheKernelsLayout(t *testing.T) {
	if pointerSize != 8 {
		t.Skipf("the published offsets are 64-bit; this is a %d-bit build", pointerSize*8)
	}
	var u usbdevfsURB
	for _, c := range []struct {
		field string
		got   uintptr
		want  uintptr
	}{
		{"Type", unsafe.Offsetof(u.Type), 0},
		{"Endpoint", unsafe.Offsetof(u.Endpoint), 1},
		{"Status", unsafe.Offsetof(u.Status), 4},
		{"Flags", unsafe.Offsetof(u.Flags), 8},
		{"Buffer", unsafe.Offsetof(u.Buffer), 16},
		{"BufferLength", unsafe.Offsetof(u.BufferLength), 24},
		{"ActualLength", unsafe.Offsetof(u.ActualLength), 28},
		{"StartFrame", unsafe.Offsetof(u.StartFrame), 32},
		{"NumberOfPackets", unsafe.Offsetof(u.NumberOfPackets), 36},
		{"ErrorCount", unsafe.Offsetof(u.ErrorCount), 40},
		{"Signr", unsafe.Offsetof(u.Signr), 44},
		{"UserContext", unsafe.Offsetof(u.UserContext), 48},
	} {
		if c.got != c.want {
			t.Errorf("usbdevfs_urb.%s is at offset %d, want %d", c.field, c.got, c.want)
		}
	}
	if urbSize != 56 {
		t.Errorf("sizeof(struct usbdevfs_urb) = %d, want 56", urbSize)
	}
	if setInterfaceSize != 8 {
		t.Errorf("sizeof(struct usbdevfs_setinterface) = %d, want 8", setInterfaceSize)
	}
	if isoPacketSize != 12 {
		t.Errorf("sizeof(struct usbdevfs_iso_packet_desc) = %d, want 12", isoPacketSize)
	}
}

// ⛔⛔ THE TWO NUMBERINGS DISAGREE, AND THE OVERLAP IS SILENT. An endpoint whose
// bmAttributes say 1 is ISOCHRONOUS; a URB whose type is 1 is INTERRUPT. One
// table used where the other belongs submits a real transfer of the wrong kind
// to a real endpoint.
func TestAnEndpointsTransferTypeIsNotAUrbType(t *testing.T) {
	iso := urbTypeFor(USBTransferIsochronous)
	if iso != urbTypeIsochronous {
		t.Fatalf("isochronous maps to URB type %d, want %d", iso, urbTypeIsochronous)
	}
	// The trap itself, stated as an assertion: the endpoint attribute and the
	// URB type for isochronous are DIFFERENT numbers.
	if int(USBTransferIsochronous) == iso {
		t.Fatal("the endpoint attribute and the URB type for isochronous are the same " +
			"number here, which means one of the two tables is wrong")
	}
	for _, c := range []struct {
		in   USBTransferType
		want int
	}{
		{USBTransferControl, urbTypeControl},
		{USBTransferBulk, urbTypeBulk},
		{USBTransferInterrupt, urbTypeInterrupt},
	} {
		if got := urbTypeFor(c.in); got != c.want {
			t.Fatalf("urbTypeFor(%s) = %d, want %d", c.in, got, c.want)
		}
	}
	// ⭐ TOTAL, AND THAT IS THE POINT. A transfer type is two bits, so every
	// input is one of the four and an error return would be unreachable. The
	// mask is what makes that true rather than hoped for.
	if got := urbTypeFor(USBTransferType(9)); got != urbTypeFor(USBTransferType(1)) {
		t.Fatalf("urbTypeFor(9) = %d; it must mask to the low two bits", got)
	}
}

// deviceDescriptor is the 18 bytes a device descriptor is, with the fields this
// package reads filled in: a composite device, VITURE's vendor, the Beast.
func deviceDescriptor() []byte {
	return []byte{
		18, descriptorTypeDevice, 0x00, 0x03, // bLength, bDescriptorType, bcdUSB
		0xef, 0x02, 0x01, // bDeviceClass/SubClass/Protocol — composite
		64,         // bMaxPacketSize0
		0xca, 0x35, // idVendor  0x35ca, little-endian
		0x01, 0x12, // idProduct 0x1201
		0x00, 0x01, 1, 2, 3, 1,
	}
}

func TestADeviceDescriptorReadsItsOwnFields(t *testing.T) {
	v, p, cl, sub, proto, err := USBDescriptors(deviceDescriptor()).Device()
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if v != VitureVendorID || p != 0x1201 {
		t.Fatalf("got %04x:%04x, want %04x:1201", v, p, VitureVendorID)
	}
	if cl != 0xef || sub != 0x02 || proto != 0x01 {
		t.Fatalf("class triple is %#02x/%#02x/%#02x, want 0xef/0x02/0x01", cl, sub, proto)
	}

	if _, _, _, _, _, err := USBDescriptors(make([]byte, 17)).Device(); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("17 bytes reported %v, want ErrShortPayload", err)
	}
	// ⛔ AND THE FIRST DESCRIPTOR MUST BE A DEVICE. A blob that began with a
	// configuration would otherwise be read as a device, and the vendor id
	// would come back as whatever bytes 8 and 9 happened to be.
	notDevice := deviceDescriptor()
	notDevice[1] = descriptorTypeConfig
	if _, _, _, _, _, err := USBDescriptors(notDevice).Device(); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("a blob starting with a config reported %v, want ErrBadPayload", err)
	}
}

// ⛔ A DESCRIPTOR OF LENGTH ZERO LOOPS FOREVER and one running past the end
// reads somebody else's memory. These bytes crossed a process boundary, so both
// are refused rather than trusted.
func TestWalkRefusesADescriptorThatCannotBeOne(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		is   error
		says string
	}{
		{"zero length", []byte{0, descriptorTypeDevice}, ErrBadPayload, "0 bytes long"},
		{"one byte", []byte{1, descriptorTypeDevice}, ErrBadPayload, "1 bytes long"},
		{"longer than what is left", []byte{40, descriptorTypeDevice, 0, 0}, ErrShortPayload, "claims 40"},
		{"a trailing half-header", append(deviceDescriptor(), 9), ErrShortPayload, "trailing byte"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := USBDescriptors(c.in).Walk(func(int, []byte) bool { return true })
			if !errors.Is(err, c.is) {
				t.Fatalf("Walk reported %v, want %v", err, c.is)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the refusal says %q, which does not mention %q", err, c.says)
			}
		})
	}
}

func TestWalkStopsWhenAskedTo(t *testing.T) {
	// Two descriptors; the walk must stop after the first and report no error.
	b := append(deviceDescriptor(), 9, descriptorTypeInterface, 0, 0, 0, 0, 0, 0, 0)
	n := 0
	if err := (USBDescriptors(b)).Walk(func(int, []byte) bool { n++; return false }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if n != 1 {
		t.Fatalf("the walk visited %d descriptors after being told to stop at the first", n)
	}
}

// The descriptors the Pixel's census reported for the Beast's camera, as the
// kernel would serve them: a UVC control interface, then streaming alternates
// whose endpoints are the answer to whether Android can read it.
func TestInterfacesAreRecoveredWithTheirEndpoints(t *testing.T) {
	iface := func(num, alt, class, sub int) []byte {
		return []byte{9, descriptorTypeInterface, byte(num), byte(alt), 0, byte(class), byte(sub), 0, 0}
	}
	endp := func(addr, attrs, max, interval int) []byte {
		return []byte{7, descriptorTypeEndpoint, byte(addr), byte(attrs),
			byte(max & 0xff), byte(max >> 8), byte(interval)}
	}
	var b []byte
	b = append(b, deviceDescriptor()...)
	b = append(b, iface(0, 0, USBClassVideo, USBSubclassVideoControl)...)
	b = append(b, endp(0x83, 0x03, 16, 6)...)
	b = append(b, iface(1, 0, USBClassVideo, USBSubclassVideoStreaming)...)
	b = append(b, iface(1, 1, USBClassVideo, USBSubclassVideoStreaming)...)
	b = append(b, endp(0x81, 0x05, 3072, 1)...)

	got, err := USBDescriptors(b).Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("found %d interfaces, want 3: %v", len(got), got)
	}
	// ⛔ AN ENDPOINT BELONGS TO THE INTERFACE IT FOLLOWS. A parser that
	// attached them all to the first would report the zero-bandwidth
	// alternate as having a streaming endpoint, which is the one thing the
	// whole census is for.
	if len(got[0].Endpoints) != 1 || got[0].Endpoints[0].Address != 0x83 {
		t.Fatalf("the control interface got %v", got[0].Endpoints)
	}
	if len(got[1].Endpoints) != 0 {
		t.Fatalf("alternate 0 is the zero-bandwidth setting and must have no endpoints, got %v",
			got[1].Endpoints)
	}
	if len(got[2].Endpoints) != 1 {
		t.Fatalf("alternate 1 got %v", got[2].Endpoints)
	}
	e := got[2].Endpoints[0]
	if e.Type() != USBTransferIsochronous || e.MaxPacketSize != 3072 || !e.In() {
		t.Fatalf("the streaming endpoint parsed as %s", e)
	}
	if !got[2].VideoStreaming() {
		t.Fatalf("the streaming interface was not recognised: %s", got[2])
	}
	// An endpoint before any interface has nothing to belong to and is dropped
	// rather than attached to whatever comes next.
	orphan := append(deviceDescriptor(), endp(0x81, 0x02, 512, 0)...)
	if got, err := USBDescriptors(orphan).Interfaces(); err != nil || len(got) != 0 {
		t.Fatalf("an endpoint with no interface gave %v, %v", got, err)
	}
}

func TestInterfacesReportsADescriptorItCannotWalk(t *testing.T) {
	if _, err := USBDescriptors([]byte{0, descriptorTypeDevice}).Interfaces(); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("Interfaces over an impossible descriptor reported %v", err)
	}
}

// ⛔ A DESCRIPTOR CAN BE WELL-FORMED AS A DESCRIPTOR AND TOO SHORT TO BE WHAT
// IT CLAIMS. Walk only checks that bLength is sane and fits; it cannot know
// that an interface descriptor needs nine bytes and an endpoint seven. Reading
// b[7] out of a four-byte descriptor would panic on bytes that crossed a
// process boundary, so each case checks its own length — and these are the
// inputs that reach those checks.
func TestADescriptorTooShortForItsOwnTypeIsSkipped(t *testing.T) {
	// A four-byte "interface": valid to walk past, impossible to read.
	short := append(deviceDescriptor(), 4, descriptorTypeInterface, 0, 0)
	got, err := USBDescriptors(short).Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a 4-byte interface descriptor produced %v", got)
	}

	// And a four-byte "endpoint" AFTER a good interface, which is the case the
	// orphan test cannot reach: there cur is non-nil and the length is what
	// stops it.
	b := append(deviceDescriptor(),
		9, descriptorTypeInterface, 1, 1, 0, USBClassVideo, USBSubclassVideoStreaming, 0, 0)
	b = append(b, 4, descriptorTypeEndpoint, 0x81, 0x05)
	got, err = USBDescriptors(b).Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("found %d interfaces, want 1", len(got))
	}
	if len(got[0].Endpoints) != 0 {
		t.Fatalf("a 4-byte endpoint descriptor was read as %v", got[0].Endpoints)
	}
}
