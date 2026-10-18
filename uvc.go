// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"encoding/binary"
	"fmt"
)

// This file is UVC: what a camera says it can do, and how it is told what to
// do. It is the layer between usbfs.go, which moves bytes, and a frame.
//
// ⭐ FOR HEAD TRACKING, LUMINANCE IS ENOUGH. go-xrkit/xrkit/headflow sums
// R+G+B per pixel down each column and matches the profiles of two frames, so a
// grey image carries everything it reads. That is why YUY2 is the format to
// want here: every other byte IS the luminance, and no colour conversion
// happens at all. It is a real simplification rather than a shortcut.

// UVC descriptor types and subtypes, from the USB Video Class specification.
const (
	// uvcCSInterface is a class-specific interface descriptor, which is where
	// everything a camera says about its formats lives. A general USB parser
	// walks past these without reading them.
	uvcCSInterface = 0x24

	// The VideoStreaming subtypes this package reads.
	uvcVSInputHeader        = 0x01
	uvcVSFormatUncompressed = 0x04
	uvcVSFrameUncompressed  = 0x05
	uvcVSFormatMJPEG        = 0x06
	uvcVSFrameMJPEG         = 0x07
)

// UVC requests and controls, from the specification's section 4.
const (
	// uvcSetCur writes a control; uvcGetCur reads the current value.
	uvcSetCur = 0x01
	uvcGetCur = 0x81
	// uvcGetLen asks how long a control is, which is how the probe block's
	// size is learned instead of guessed from a version number.
	uvcGetLen = 0x85

	// uvcProbeControl is the block a host and a camera negotiate with;
	// uvcCommitControl is the same block, accepted.
	uvcProbeControl  = 0x01
	uvcCommitControl = 0x02
)

// UVC format identities this package names.
type UVCFormat int

// The formats a camera here is expected to offer.
const (
	// FormatUnknown is a format descriptor this package did not recognise. It
	// is kept rather than dropped: a camera that offers only formats we cannot
	// name is a different finding from a camera that offers none.
	FormatUnknown UVCFormat = iota
	// FormatYUY2 is uncompressed 4:2:2, two bytes a pixel, luminance first.
	// ⭐ It is the one to want: see the note at the top of this file.
	FormatYUY2
	// FormatMJPEG is a JPEG per frame, which Go's image/jpeg decodes.
	FormatMJPEG
)

// String renders the format.
func (f UVCFormat) String() string {
	switch f {
	case FormatYUY2:
		return "YUY2"
	case FormatMJPEG:
		return "MJPEG"
	}
	return "unknown"
}

// A UVCFrameSize is one size a format offers, with the shortest interval
// between frames the camera will sustain at it.
type UVCFrameSize struct {
	// Index is the camera's own bFrameIndex, which is what a commit names.
	Index int
	// Width and Height are in pixels.
	Width, Height int
	// Interval is the default frame interval in 100-nanosecond units, which is
	// how UVC states frame rates: 333333 is 30 per second.
	Interval uint32
}

// FPS is the frame rate the interval states, or 0 when the camera gave none.
func (s UVCFrameSize) FPS() float64 {
	if s.Interval == 0 {
		return 0
	}
	return 1e7 / float64(s.Interval)
}

// String renders the size for logs.
func (s UVCFrameSize) String() string {
	return fmt.Sprintf("%dx%d @%.3gfps (interval %d)", s.Width, s.Height, s.FPS(), s.Interval)
}

// A UVCFormatDesc is one format a streaming interface offers, with its sizes.
type UVCFormatDesc struct {
	// Index is the camera's own bFormatIndex.
	Index int
	// Format is what this package made of its GUID or subtype.
	Format UVCFormat
	// Sizes are the frame sizes it offers, in the order the camera lists them.
	Sizes []UVCFrameSize
}

// String renders the format and its sizes.
func (d UVCFormatDesc) String() string {
	s := fmt.Sprintf("format %d %s", d.Index, d.Format)
	for _, z := range d.Sizes {
		s += "\n      " + z.String()
	}
	return s
}

// MinTrackingWidth is the narrowest frame worth asking for.
//
// headflow bins columns [headflow.BinWidth] wide — four — so 320 pixels across
// is already eighty bins to match a profile on, and its measured reach is
// bounded in bins rather than in pixels. Below this there is little left to
// match; above it, more width buys precision a head does not move with.
const MinTrackingWidth = 320

// Fastest returns the size with the HIGHEST FRAME RATE this format offers, at
// no less than [MinTrackingWidth] across, breaking ties toward the wider frame.
//
// ⛔⛔ RATE, NOT SIZE, AND THAT WAS MEASURED THE WRONG WAY ROUND FIRST. This
// picked the largest frame, reasoning that more pixels across means more to
// match. The camera's own table says what that costs:
//
//	YUY2  1920x1080 @5fps    1280x720 @10fps    800x600 @20fps    640x480 @25fps
//	MJPEG 1920x1080 @30fps   … 30fps at every size
//
// So "largest" chose 1920x1080 at FIVE frames a second — 200 ms a frame, worse
// than the 46–107 ms the same algorithm already achieves on macOS, for a
// precision nothing here needs. Latency is what head tracking is bounded by;
// headflow's own documentation says 1:1 locking wants under 20 ms and that
// choosing which screen somebody faces is comfortable at 107 ms.
func (d UVCFormatDesc) Fastest() (UVCFrameSize, bool) {
	var best UVCFrameSize
	for _, z := range d.Sizes {
		if z.Width < MinTrackingWidth || z.Interval == 0 {
			continue
		}
		switch {
		case best.Interval == 0,
			z.Interval < best.Interval,
			z.Interval == best.Interval && z.Width > best.Width:
			best = z
		}
	}
	return best, best.Width > 0
}

// yuy2GUID is the GUID a YUY2 format descriptor carries. UVC names
// uncompressed formats by GUID rather than by subtype, so the subtype alone
// says "uncompressed" and nothing about which one.
var yuy2GUID = [16]byte{
	0x59, 0x55, 0x59, 0x32, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// UVCFormats reads what one streaming interface offers, from the device's raw
// descriptors.
//
// ⛔ IT WALKS THE CLASS-SPECIFIC DESCRIPTORS, WHICH NO GENERAL PARSER REPORTS.
// A camera's formats and frame sizes live in type 0x24 descriptors nested after
// its streaming interface, and [USBDescriptors.Interfaces] steps straight over
// them. Everything needed to commit a format is in there and nowhere else.
//
// It associates each frame descriptor with the format that preceded it, which
// is how UVC orders them, and stops at the next interface so a device with two
// streaming interfaces does not have its formats merged.
func (d USBDescriptors) UVCFormats(iface int) ([]UVCFormatDesc, error) {
	var out []UVCFormatDesc
	inIface, seen := false, false
	err := d.Walk(func(typ int, b []byte) bool {
		switch typ {
		case descriptorTypeInterface:
			if len(b) < 9 {
				return true
			}
			// Alternate 0 of the wanted interface is where the class
			// descriptors hang; the later alternates carry only endpoints.
			inIface = int(b[2]) == iface && int(b[3]) == 0
			if inIface {
				seen = true
			}
			return true
		case uvcCSInterface:
			if !inIface || len(b) < 3 {
				return true
			}
			switch b[2] {
			case uvcVSFormatUncompressed:
				if len(b) >= 21 {
					f := UVCFormatDesc{Index: int(b[3]), Format: FormatUnknown}
					if [16]byte(b[5:21]) == yuy2GUID {
						f.Format = FormatYUY2
					}
					out = append(out, f)
				}
			case uvcVSFormatMJPEG:
				if len(b) >= 4 {
					out = append(out, UVCFormatDesc{Index: int(b[3]), Format: FormatMJPEG})
				}
			case uvcVSFrameUncompressed, uvcVSFrameMJPEG:
				// ⛔ A FRAME BELONGS TO THE FORMAT BEFORE IT. One attached to
				// the wrong format commits a size the camera will refuse, and
				// the refusal names neither.
				if len(out) == 0 || len(b) < 26 {
					return true
				}
				cur := &out[len(out)-1]
				cur.Sizes = append(cur.Sizes, UVCFrameSize{
					Index:    int(b[3]),
					Width:    int(binary.LittleEndian.Uint16(b[5:])),
					Height:   int(binary.LittleEndian.Uint16(b[7:])),
					Interval: binary.LittleEndian.Uint32(b[21:]),
				})
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if !seen {
		return nil, fmt.Errorf("%w: the descriptors hold no interface %d alternate 0",
			ErrNotFound, iface)
	}
	return out, nil
}

// PickFormat chooses what to ask a camera for: the fastest frame in a format
// this package can actually read.
//
// ⭐ YUY2 FIRST, AND THE REASON IS WHAT IS IMPLEMENTED. Head tracking reads
// luminance only, and in YUY2 every other byte IS the luminance — no decoder,
// no colour conversion, nothing allocated per frame. MJPEG on the camera
// measured here is FASTER, 30 frames a second at every size against YUY2's 25,
// and it is the better choice the day there is a JPEG decode in the frame path.
// There is not, so preferring YUY2 reflects the code rather than the hardware,
// and saying so is the point.
//
// ⚠ A format this package cannot name is skipped rather than attempted. A
// camera offering only those is a different finding from one offering none, and
// the error says how many there were.
func PickFormat(fs []UVCFormatDesc) (UVCFormatDesc, UVCFrameSize, error) {
	for _, want := range []UVCFormat{FormatYUY2, FormatMJPEG} {
		for _, f := range fs {
			if f.Format != want {
				continue
			}
			if z, ok := f.Fastest(); ok {
				return f, z, nil
			}
		}
	}
	return UVCFormatDesc{}, UVCFrameSize{}, fmt.Errorf("%w: none of the %d format(s) is one "+
		"this package can read, at %d pixels across or more", ErrNotFound,
		len(fs), MinTrackingWidth)
}

// MinProbeBytes is the shortest probe block any UVC version defines: 26 bytes
// for 1.0. Anything shorter is not a negotiation block.
const MinProbeBytes = 26

// probeBlock is the part of a UVC probe/commit block this package sets. The
// rest is whatever the camera filled in, and IS SENT BACK UNCHANGED: the block
// is a conversation, and a host that rewrote fields it does not understand
// would be answering questions it was not asked.
type probeBlock []byte

// setFormat writes the format, frame and interval into a block the camera
// supplied.
func (p probeBlock) setFormat(format, frame int, interval uint32) {
	p[2] = byte(format)
	p[3] = byte(frame)
	binary.LittleEndian.PutUint32(p[4:], interval)
}

// format reads back what the block says, which after a GET_CUR is what the
// camera AGREED TO rather than what it was asked for.
func (p probeBlock) format() (format, frame int, interval uint32, maxPayload uint32) {
	format, frame = int(p[2]), int(p[3])
	interval = binary.LittleEndian.Uint32(p[4:])
	if len(p) >= 26 {
		maxPayload = binary.LittleEndian.Uint32(p[22:])
	}
	return format, frame, interval, maxPayload
}

// A UVCNegotiation is what a camera agreed to.
//
// ⛔ AGREED TO, NOT ASKED FOR. A UVC camera may answer a probe with something
// other than what it was given — a different interval, a smaller payload — and
// the host is expected to accept that answer. Reporting the REQUEST as though
// it were the outcome is how a stream comes out at a frame rate nobody chose.
//
// ⚠ IT IS DECLARED HERE, NOT BESIDE Negotiate. The type is data and travels
// everywhere; only the negotiation needs a device. Declaring it twice, once per
// platform, left String on one of them — which go vet caught as a %s against a
// type with no String, on the lane that could not see the other copy.
type UVCNegotiation struct {
	// Format and Frame are the indices the camera settled on.
	Format, Frame int
	// Interval is the frame interval it settled on, in 100ns units.
	Interval uint32
	// MaxPayload is the largest transfer it will send, which bounds how much
	// one isochronous request can usefully carry.
	MaxPayload uint32
	// Asked is what it was given, kept so a caller can SEE a change rather
	// than infer one.
	Asked UVCFrameSize
	// Kind is the format's KIND, as against Format which is the camera's own
	// index for it.
	//
	// ⛔ A NEGOTIATION RESULT SHOULD SAY WHAT WAS AGREED, and an index is a name
	// the camera chose rather than a fact about the bytes that follow. Without
	// it a stream cannot tell whether to assemble by LENGTH or by MARKERS — and
	// that distinction is what makes a compressed frame possible at all. The
	// alternative was a second argument on [USBHandle.Stream], which would have
	// put the same fact in two places and let them disagree.
	Kind UVCFormat
}

// Changed reports whether the camera settled on something other than what it
// was asked for.
func (n UVCNegotiation) Changed() bool {
	return n.Frame != n.Asked.Index || n.Interval != n.Asked.Interval
}

// FPS is the frame rate the camera settled on.
func (n UVCNegotiation) FPS() float64 {
	if n.Interval == 0 {
		return 0
	}
	return 1e7 / float64(n.Interval)
}

// String renders the outcome for logs.
func (n UVCNegotiation) String() string {
	s := fmt.Sprintf("format %d frame %d interval %d (%.3gfps) payload %d",
		n.Format, n.Frame, n.Interval, n.FPS(), n.MaxPayload)
	if n.Changed() {
		s += fmt.Sprintf(" — CHANGED from frame %d interval %d", n.Asked.Index, n.Asked.Interval)
	}
	return s
}
