// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// uvcDescriptors builds the descriptor blob the Beast's camera reports, as the
// kernel serves it: an interface, then the class-specific format and frame
// descriptors nested after it.
//
// ⭐ IT IS THE REAL TABLE, read off the device by cmd/xrusb. A camera invented
// for a test would have agreed with whatever the code did; this one disagreed
// and changed the design — see TestFastestPrefersRateOverSize.
func uvcDescriptors() []byte {
	iface := func(num, alt, class, sub int) []byte {
		return []byte{9, descriptorTypeInterface, byte(num), byte(alt), 0,
			byte(class), byte(sub), 0, 0}
	}
	// A VS_FORMAT_UNCOMPRESSED descriptor is 27 bytes with the GUID at 5.
	yuy2 := func(index int) []byte {
		b := make([]byte, 27)
		b[0], b[1], b[2], b[3] = 27, uvcCSInterface, uvcVSFormatUncompressed, byte(index)
		copy(b[5:], yuy2GUID[:])
		return b
	}
	mjpeg := func(index int) []byte {
		b := make([]byte, 11)
		b[0], b[1], b[2], b[3] = 11, uvcCSInterface, uvcVSFormatMJPEG, byte(index)
		return b
	}
	frame := func(subtype, index, w, h int, interval uint32) []byte {
		b := make([]byte, 30)
		b[0], b[1], b[2], b[3] = 30, uvcCSInterface, byte(subtype), byte(index)
		binary.LittleEndian.PutUint16(b[5:], uint16(w))
		binary.LittleEndian.PutUint16(b[7:], uint16(h))
		binary.LittleEndian.PutUint32(b[21:], interval)
		return b
	}

	var b []byte
	b = append(b, deviceDescriptor()...)
	b = append(b, iface(0, 0, USBClassVideo, USBSubclassVideoControl)...)
	b = append(b, iface(1, 0, USBClassVideo, USBSubclassVideoStreaming)...)
	// MJPEG: 30fps at every size, which is what the device reported.
	b = append(b, mjpeg(1)...)
	b = append(b, frame(uvcVSFrameMJPEG, 1, 1920, 1080, 333333)...)
	b = append(b, frame(uvcVSFrameMJPEG, 6, 640, 480, 333333)...)
	// YUY2: slow when large, fast when small.
	b = append(b, yuy2(2)...)
	b = append(b, frame(uvcVSFrameUncompressed, 1, 1920, 1080, 2000000)...)
	b = append(b, frame(uvcVSFrameUncompressed, 4, 1280, 720, 1000000)...)
	b = append(b, frame(uvcVSFrameUncompressed, 6, 640, 480, 400000)...)
	b = append(b, frame(uvcVSFrameUncompressed, 9, 160, 120, 400000)...)
	// A later alternate carries only an endpoint, and no formats.
	b = append(b, iface(1, 6, USBClassVideo, USBSubclassVideoStreaming)...)
	b = append(b, 7, descriptorTypeEndpoint, 0x81, 0x05, 0x00, 0x14, 1)
	return b
}

func TestUVCFormatsReadsWhatNoGeneralParserReports(t *testing.T) {
	fs, err := USBDescriptors(uvcDescriptors()).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats: %v", err)
	}
	if len(fs) != 2 {
		t.Fatalf("found %d formats, want 2: %v", len(fs), fs)
	}
	if fs[0].Format != FormatMJPEG || fs[0].Index != 1 {
		t.Fatalf("the first format is %s index %d", fs[0].Format, fs[0].Index)
	}
	// ⭐ THE GUID IS WHAT NAMES AN UNCOMPRESSED FORMAT. The subtype only says
	// "uncompressed"; a parser that stopped there would call every raw format
	// YUY2, including the ones whose bytes are in another order.
	if fs[1].Format != FormatYUY2 || fs[1].Index != 2 {
		t.Fatalf("the second format is %s index %d", fs[1].Format, fs[1].Index)
	}
	// ⛔ A FRAME BELONGS TO THE FORMAT BEFORE IT. Attached to the wrong one, a
	// commit names a size the camera refuses and the refusal names neither.
	if len(fs[0].Sizes) != 2 || len(fs[1].Sizes) != 4 {
		t.Fatalf("sizes split %d/%d, want 2/4", len(fs[0].Sizes), len(fs[1].Sizes))
	}
	if fs[1].Sizes[0].Width != 1920 || fs[1].Sizes[0].Interval != 2000000 {
		t.Fatalf("the first YUY2 size is %s", fs[1].Sizes[0])
	}
	if fps := fs[1].Sizes[0].FPS(); fps < 4.9 || fps > 5.1 {
		t.Fatalf("an interval of 2000000 is %.3g fps, want 5", fps)
	}
	if (UVCFrameSize{}).FPS() != 0 {
		t.Fatal("a size with no interval must report no rate rather than divide by zero")
	}
}

// ⛔⛔ RATE, NOT SIZE — and the device is what settled it. Picking the largest
// frame chose 1920x1080 at FIVE frames a second: 200 ms a frame, worse than the
// 46–107 ms the same algorithm already achieves elsewhere, for a precision head
// tracking does not use.
func TestFastestPrefersRateOverSize(t *testing.T) {
	fs, err := USBDescriptors(uvcDescriptors()).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats: %v", err)
	}
	z, ok := fs[1].Fastest()
	if !ok {
		t.Fatal("YUY2 offers four sizes and Fastest found none")
	}
	if z.Width != 640 || z.Interval != 400000 {
		t.Fatalf("Fastest chose %s; the fastest YUY2 frame at or above %d across is "+
			"640x480 @25fps", z, MinTrackingWidth)
	}
	// ⛔ AND NOT THE NARROWEST ONE AT THE SAME RATE. 160x120 shares the 25fps
	// interval, and forty bins is not a profile.
	if z.Width == 160 {
		t.Fatal("Fastest chose 160x120; below MinTrackingWidth there is nothing to match")
	}

	// A format whose every size is too narrow offers nothing.
	narrow := UVCFormatDesc{Sizes: []UVCFrameSize{{Index: 1, Width: 160, Height: 120, Interval: 400000}}}
	if _, ok := narrow.Fastest(); ok {
		t.Fatal("a format with only narrow sizes must offer none")
	}
	// A size with no interval is not a rate, and must not win by being zero.
	noRate := UVCFormatDesc{Sizes: []UVCFrameSize{{Index: 1, Width: 640, Height: 480}}}
	if _, ok := noRate.Fastest(); ok {
		t.Fatal("a size with no interval was chosen; zero is not the fastest rate")
	}
}

// PickFormat reflects WHAT IS IMPLEMENTED rather than what the hardware can do:
// MJPEG is faster on this camera, and is the better choice the day a JPEG
// decode exists in the frame path.
func TestPickFormatPrefersTheOneWithNoDecoder(t *testing.T) {
	fs, err := USBDescriptors(uvcDescriptors()).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats: %v", err)
	}
	f, z, err := PickFormat(fs)
	if err != nil {
		t.Fatalf("PickFormat: %v", err)
	}
	if f.Format != FormatYUY2 || z.Width != 640 {
		t.Fatalf("PickFormat chose %s %s", f.Format, z)
	}

	// With no YUY2, MJPEG is taken — at its fastest size, not its largest.
	onlyMJPEG := fs[:1]
	f, z, err = PickFormat(onlyMJPEG)
	if err != nil {
		t.Fatalf("PickFormat over MJPEG alone: %v", err)
	}
	if f.Format != FormatMJPEG {
		t.Fatalf("PickFormat chose %s with no YUY2 available", f.Format)
	}
	if z.Interval != 333333 {
		t.Fatalf("PickFormat chose %s; every MJPEG size here is 30fps", z)
	}

	// ⚠ A FORMAT THIS PACKAGE CANNOT NAME IS NOT ATTEMPTED, and the refusal
	// says how many there were: "none I can read" and "none at all" are
	// different findings.
	_, _, err = PickFormat([]UVCFormatDesc{{Index: 3, Format: FormatUnknown,
		Sizes: []UVCFrameSize{{Index: 1, Width: 640, Height: 480, Interval: 333333}}}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("PickFormat over an unknown format = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "1 format") {
		t.Fatalf("the refusal says %q, which does not say how many were offered", err)
	}
	if _, _, err := PickFormat(nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PickFormat over nothing = %v", err)
	}
}

func TestUVCFormatsRefusesWhatItCannotRead(t *testing.T) {
	// An interface that is not there at all.
	if _, err := USBDescriptors(uvcDescriptors()).UVCFormats(7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UVCFormats on a missing interface = %v, want ErrNotFound", err)
	}
	// A blob that cannot be walked.
	if _, err := USBDescriptors([]byte{0, descriptorTypeDevice}).UVCFormats(1); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("UVCFormats over an impossible descriptor = %v", err)
	}
	// ⛔ A FRAME BEFORE ANY FORMAT HAS NOTHING TO BELONG TO, and attaching it
	// to whatever came next would put a size under the wrong format.
	orphan := append(deviceDescriptor(),
		9, descriptorTypeInterface, 1, 0, 0, USBClassVideo, USBSubclassVideoStreaming, 0, 0)
	orphan = append(orphan, make([]byte, 30)...)
	orphan[len(orphan)-30], orphan[len(orphan)-29] = 30, uvcCSInterface
	orphan[len(orphan)-28] = uvcVSFrameUncompressed
	fs, err := USBDescriptors(orphan).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("a frame with no format produced %v", fs)
	}
}

// ⛔ CLASS DESCRIPTORS BELONG TO ONE INTERFACE. A device with two streaming
// interfaces must not have their formats merged, and alternate settings other
// than 0 carry endpoints rather than formats.
func TestUVCFormatsStaysInsideItsInterface(t *testing.T) {
	iface := func(num, alt int) []byte {
		return []byte{9, descriptorTypeInterface, byte(num), byte(alt), 0,
			USBClassVideo, USBSubclassVideoStreaming, 0, 0}
	}
	mjpeg := func(index int) []byte {
		return []byte{11, uvcCSInterface, uvcVSFormatMJPEG, byte(index), 0, 0, 0, 0, 0, 0, 0}
	}
	var b []byte
	b = append(b, deviceDescriptor()...)
	b = append(b, iface(1, 0)...)
	b = append(b, mjpeg(1)...)
	b = append(b, iface(2, 0)...)
	b = append(b, mjpeg(1)...)
	b = append(b, mjpeg(2)...)

	one, err := USBDescriptors(b).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats(1): %v", err)
	}
	two, err := USBDescriptors(b).UVCFormats(2)
	if err != nil {
		t.Fatalf("UVCFormats(2): %v", err)
	}
	if len(one) != 1 || len(two) != 2 {
		t.Fatalf("formats split %d/%d, want 1/2 — the interfaces were merged",
			len(one), len(two))
	}
}

func TestTheUVCTypesSayWhatTheyAre(t *testing.T) {
	for f, want := range map[UVCFormat]string{
		FormatYUY2: "YUY2", FormatMJPEG: "MJPEG", FormatUnknown: "unknown",
		UVCFormat(9): "unknown",
	} {
		if got := f.String(); got != want {
			t.Fatalf("UVCFormat(%d).String = %q, want %q", int(f), got, want)
		}
	}
	z := UVCFrameSize{Index: 6, Width: 640, Height: 480, Interval: 400000}
	if got := z.String(); !strings.Contains(got, "640x480") || !strings.Contains(got, "25") {
		t.Fatalf("UVCFrameSize.String = %q", got)
	}
	d := UVCFormatDesc{Index: 2, Format: FormatYUY2, Sizes: []UVCFrameSize{z}}
	if got := d.String(); !strings.Contains(got, "format 2 YUY2") || !strings.Contains(got, "640x480") {
		t.Fatalf("UVCFormatDesc.String = %q", got)
	}

	// ⛔ A NEGOTIATION SAYS WHEN THE CAMERA CHANGED THE TERMS, because it may,
	// and reporting the request as the outcome is how a stream comes out at a
	// rate nobody chose.
	n := UVCNegotiation{Format: 2, Frame: 6, Interval: 400000, MaxPayload: 2400, Asked: z}
	if n.Changed() {
		t.Fatalf("a negotiation that matched what was asked reports Changed: %s", n)
	}
	if got := n.String(); strings.Contains(got, "CHANGED") {
		t.Fatalf("String says CHANGED on an unchanged negotiation: %q", got)
	}
	if fps := n.FPS(); fps < 24.9 || fps > 25.1 {
		t.Fatalf("FPS = %.3g, want 25", fps)
	}
	slower := UVCNegotiation{Format: 2, Frame: 1, Interval: 2000000, Asked: z}
	if !slower.Changed() {
		t.Fatal("a camera that answered with another frame and interval reports no change")
	}
	if got := slower.String(); !strings.Contains(got, "CHANGED") {
		t.Fatalf("String = %q, which does not say the terms changed", got)
	}
	if (UVCNegotiation{}).FPS() != 0 {
		t.Fatal("a negotiation with no interval must report no rate")
	}
}

// The probe block is a CONVERSATION: fields this package does not understand
// travel back unchanged, because a host that rewrote them would be answering
// questions it was not asked.
func TestAProbeBlockKeepsWhatItWasNotAskedAbout(t *testing.T) {
	p := make(probeBlock, 34)
	for i := range p {
		p[i] = 0xAA
	}
	p.setFormat(2, 6, 400000)
	format, frame, interval, payload := p.format()
	if format != 2 || frame != 6 || interval != 400000 {
		t.Fatalf("round-tripped to format %d frame %d interval %d", format, frame, interval)
	}
	if payload != 0xAAAAAAAA {
		t.Fatalf("the payload field was %#x; setFormat must not touch it", payload)
	}
	for i, b := range p[8:] {
		if b != 0xAA {
			t.Fatalf("byte %d past the interval was changed to %#02x", i+8, b)
		}
	}
	// A block too short to hold a payload field reports zero rather than
	// reading past itself.
	short := make(probeBlock, MinProbeBytes-1)
	if _, _, _, got := short.format(); got != 0 {
		t.Fatalf("a %d-byte block reported a payload of %d", len(short), got)
	}
}

// An interface descriptor too short to read is skipped, like everywhere else
// that reads one: Walk only checks that bLength fits, not that it is long
// enough for the type it claims.
func TestUVCFormatsSkipsAnUnreadableInterfaceDescriptor(t *testing.T) {
	b := append(deviceDescriptor(), 4, descriptorTypeInterface, 1, 0)
	b = append(b, 9, descriptorTypeInterface, 1, 0, 0,
		USBClassVideo, USBSubclassVideoStreaming, 0, 0)
	b = append(b, 11, uvcCSInterface, uvcVSFormatMJPEG, 1, 0, 0, 0, 0, 0, 0, 0)
	fs, err := USBDescriptors(b).UVCFormats(1)
	if err != nil {
		t.Fatalf("UVCFormats: %v", err)
	}
	if len(fs) != 1 {
		t.Fatalf("found %d formats, want 1 — the 4-byte interface must be stepped over "+
			"rather than read", len(fs))
	}
}

// ⛔ THE REQUEST TYPE IS THE DIFFERENCE BETWEEN ASKING AND TELLING, and both
// halves are one byte apart. A GET built with the OUT direction sends the
// buffer instead of filling it, which looks like a camera answering zeroes.
func TestTheControlRequestTypesAddressAnInterfaceClass(t *testing.T) {
	in, out := ControlIn(), ControlOut()
	if in&0x80 == 0 {
		t.Fatalf("ControlIn = %#02x, which carries data TO the device", in)
	}
	if out&0x80 != 0 {
		t.Fatalf("ControlOut = %#02x, which brings data back", out)
	}
	for name, v := range map[string]uint8{"ControlIn": in, "ControlOut": out} {
		if v&0x60 != 0x20 {
			t.Fatalf("%s = %#02x, which is not a CLASS request; UVC controls are not "+
				"standard USB ones", name, v)
		}
		if v&0x1f != 0x01 {
			t.Fatalf("%s = %#02x, which does not address an INTERFACE; wIndex names the "+
				"streaming interface and would be read as something else", name, v)
		}
	}
}
