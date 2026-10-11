// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"fmt"
	"image"
)

// This file turns UVC payloads into frames, and it is all portable: the
// isochronous packets arrive through usbfs, but deciding where one frame ends
// and the next begins is arithmetic on bytes and is tested on every GOOS.

// UVC payload header bits, from the specification's section 2.4.3.3.
const (
	// payloadFID toggles between frames. ⛔ IT IS THE ONLY RELIABLE BOUNDARY:
	// some cameras never set the end-of-frame bit, and one that did not would
	// have every frame run into the next for as long as it streamed.
	payloadFID = 1 << 0
	// payloadEOF marks the last payload of a frame, when the camera bothers.
	payloadEOF = 1 << 1
	// payloadError says the camera knows this payload is bad.
	payloadError = 1 << 6
)

// MinPayloadHeader is the shortest UVC payload header: a length and the bits.
const MinPayloadHeader = 2

// A UVCPayload is one isochronous packet's worth of a frame.
type UVCPayload struct {
	// Header is the payload's own bmHeaderInfo.
	Header byte
	// Data is the pixels, which is whatever followed the header.
	Data []byte
}

// FID is the frame this payload belongs to, as a single bit that toggles.
func (p UVCPayload) FID() bool { return p.Header&payloadFID != 0 }

// EOF reports whether the camera marked this the last payload of its frame.
func (p UVCPayload) EOF() bool { return p.Header&payloadEOF != 0 }

// Bad reports whether the camera marked the payload itself as in error.
func (p UVCPayload) Bad() bool { return p.Header&payloadError != 0 }

// ParsePayload splits one isochronous packet into its header and its pixels.
//
// ⛔ bHeaderLength IS THE OFFSET AND IT IS NOT CONSTANT. Cameras use 2, 12 and
// more; taking a fixed two bytes would prepend ten bytes of timestamp to every
// frame's pixels, which does not fail — it shears the image by five YUY2 pixels
// and looks like a camera mounted crooked.
//
// A packet shorter than its own header, or one whose header is longer than the
// packet, is refused rather than clamped: both describe a packet that is not
// the one the camera meant to send.
func ParsePayload(b []byte) (UVCPayload, error) {
	if len(b) < MinPayloadHeader {
		return UVCPayload{}, fmt.Errorf("%w: a %d-byte packet cannot hold a payload header",
			ErrShortPayload, len(b))
	}
	n := int(b[0])
	switch {
	case n < MinPayloadHeader:
		return UVCPayload{}, fmt.Errorf("%w: the payload header says it is %d bytes long",
			ErrBadPayload, n)
	case n > len(b):
		return UVCPayload{}, fmt.Errorf("%w: a %d-byte header in a %d-byte packet",
			ErrShortPayload, n, len(b))
	}
	return UVCPayload{Header: b[1], Data: b[n:]}, nil
}

// BytesPerPixelYUY2 is what YUY2 costs: two bytes a pixel, the first of each
// pair being the luminance.
const BytesPerPixelYUY2 = 2

// A FrameAssembler turns a stream of payloads into whole frames.
//
// ⭐ IT ENDS A FRAME ON THE FID TOGGLE, not on the end-of-frame bit. EOF is
// optional in practice — cameras that never set it exist — and a boundary that
// depended on it would run every frame into the next. EOF is still honoured
// when it comes, because it ends the frame one payload earlier and saves
// carrying a frame until the next one starts.
//
// ⛔ AND A SHORT FRAME IS DROPPED RATHER THAN PADDED. A stream that lost a
// packet leaves a frame missing a band; padding it with zeros produces a black
// stripe that a tracker reads as structure, which is worse than no frame at
// all. [FrameAssembler.Dropped] counts them.
type FrameAssembler struct {
	w, h int
	// want is how many bytes a whole frame is.
	want int

	buf  []byte
	fid  bool
	open bool

	frames  int
	dropped int
	bad     int

	// toggles and eofs count the two things that can END a frame, and short
	// counts the bytes the dropped ones were missing.
	//
	// ⛔⛔ THEY EXIST BECAUSE THE COUNTS DID NOT CLOSE. The first on-device
	// stream delivered 10 frames and dropped 10, with 7968 packets — about
	// thirty-one frames' worth of data for twenty frames seen. A census that
	// does not add up is the finding, and "frames" and "dropped" alone cannot
	// say WHICH boundary fired or how far short the losers were. Counting per
	// channel is the only way to tell a camera that splits its frames from a
	// bus that loses packets.
	toggles int
	eofs    int
	short   int
	// dropOnEOF and dropOnToggle split the losses by WHICH boundary ended them.
	//
	// ⛔⛔ WITHOUT THIS SPLIT THE CENSUS IS AMBIGUOUS. 11 toggles and 11 EOFs
	// for 22 completions, half of them 8% full, is consistent with two opposite
	// stories: a camera that sets EOF early (so the EOF ends a fragment and the
	// toggle ends the real frame) or one that toggles early. They call for
	// opposite fixes, and one counter each tells them apart.
	dropOnEOF    int
	dropOnToggle int
}

// Why a frame ended, for the per-boundary census.
type endedBy int

const (
	endedByToggle endedBy = iota
	endedByEOF
)

// MaxFrameBytes is the largest frame this package will assemble.
//
// ⛔⛔ IT EXISTS BECAUSE THE SIZE COMES FROM THE DEVICE. A frame's dimensions
// are read out of USB descriptors — two 16-bit fields — so a broken or hostile
// camera can ask for 65535x65535, and 65535*65535*2 is 8.6 GB that this process
// would try to allocate twice over (the assembly buffer and the image) before
// anything else happened. The device is on the other side of a cable and is not
// trusted with an allocation.
//
// The ceiling is 64 MB — 33,554,432 YUY2 pixels, or 4096x8192 — which is some
// sixteen times the largest frame any camera here advertises; the Beast's own
// largest is 1920x1080, or 4 MB.
const MaxFrameBytes = 64 << 20

// NewFrameAssembler returns an assembler for frames of the given size.
func NewFrameAssembler(w, h int) (*FrameAssembler, error) {
	if w < MinTrackingWidth || h <= 0 {
		return nil, fmt.Errorf("%w: a %dx%d frame is not one this package assembles; the "+
			"width must be at least %d", ErrInvalidOption, w, h, MinTrackingWidth)
	}
	if w > MaxFrameBytes/BytesPerPixelYUY2/h {
		// ⛔ WRITTEN AS A DIVISION, AND THE PRODUCT IS NOT PRINTED EITHER. The
		// multiplication this guards is the one that would overflow, and
		// forming it in the error message to say how big it was would overflow
		// in exactly the case the message exists for — Go wraps silently, so
		// the refusal would carry a number that is not the answer.
		return nil, fmt.Errorf("%w: a %dx%d YUY2 frame is more than the %d bytes this "+
			"package assembles; the size came from the device's descriptors and is not "+
			"trusted with an allocation", ErrInvalidOption, w, h, MaxFrameBytes)
	}
	want := w * h * BytesPerPixelYUY2
	return &FrameAssembler{w: w, h: h, want: want, buf: make([]byte, 0, want)}, nil
}

// Size is the frame size this assembler was built for.
func (a *FrameAssembler) Size() (int, int) { return a.w, a.h }

// Stats reports how many whole frames came out, how many were dropped for being
// short, and how many payloads the camera itself marked bad.
func (a *FrameAssembler) Stats() (frames, dropped, bad int) {
	return a.frames, a.dropped, a.bad
}

// Boundaries reports how each frame ended and how much the dropped ones were
// missing: identifier toggles, end-of-frame bits, and the total shortfall in
// bytes.
//
// ⭐ IT IS A CENSUS PER CHANNEL. "frames" and "dropped" cannot say whether a
// camera split its frames or a bus lost packets — the first shows up as more
// boundaries than frames, the second as a shortfall spread thinly. Averaging the
// shortfall over the dropped count says which.
func (a *FrameAssembler) Boundaries() (toggles, eofs, shortBytes int) {
	return a.toggles, a.eofs, a.short
}

// Losses reports which boundary ended the frames that were dropped.
//
// ⛔ THE SPLIT IS THE DIAGNOSIS. Equal numbers of toggles and EOFs with half the
// frames nearly empty fits two opposite stories — a camera setting EOF early, or
// one toggling early — and they call for opposite fixes.
func (a *FrameAssembler) Losses() (onToggle, onEOF int) {
	return a.dropOnToggle, a.dropOnEOF
}

// Feed takes one payload and returns a whole frame's bytes when one completed.
//
// The bytes are BORROWED: they are valid until the next Feed, which starts
// writing the following frame over them. A caller that keeps a frame copies it,
// and [FrameAssembler.Decode] does.
func (a *FrameAssembler) Feed(p UVCPayload) ([]byte, bool) {
	if p.Bad() {
		// The camera says this packet is wrong. Whatever frame it belongs to is
		// incomplete, so the frame is abandoned rather than quietly short.
		a.bad++
		a.open = false
		a.buf = a.buf[:0]
		a.fid = p.FID()
		return nil, false
	}

	var done []byte
	if a.open && p.FID() != a.fid {
		a.toggles++
		// The frame ended at the previous payload: this one belongs to the
		// next. ⛔ THE TOGGLE IS THE BOUNDARY, and it is checked BEFORE the
		// payload is appended — appending first would put the first packet of
		// the new frame at the end of the old one.
		done = a.complete(endedByToggle)
	}
	a.fid = p.FID()
	a.open = true
	if len(a.buf)+len(p.Data) <= a.want {
		a.buf = append(a.buf, p.Data...)
	} else {
		// More bytes than a frame holds. The camera and this assembler
		// disagree about the size, and truncating silently would deliver a
		// frame whose rows are offset from the one before.
		a.buf = append(a.buf, p.Data[:a.want-len(a.buf)]...)
	}
	if p.EOF() {
		a.eofs++
		if d := a.complete(endedByEOF); d != nil {
			done = d
		}
	}
	return done, done != nil
}

// complete closes the frame being assembled, returning its bytes when it is
// whole and counting it as dropped when it is not.
func (a *FrameAssembler) complete(why endedBy) []byte {
	n := len(a.buf)
	a.open = false
	if n < a.want {
		a.dropped++
		a.short += a.want - n
		if why == endedByEOF {
			a.dropOnEOF++
		} else {
			a.dropOnToggle++
		}
		a.buf = a.buf[:0]
		return nil
	}
	a.frames++
	out := a.buf
	a.buf = make([]byte, 0, a.want)
	return out
}

// Decode turns one assembled YUY2 frame into an RGBA image.
//
// ⭐ IT WRITES LUMINANCE INTO ALL THREE CHANNELS, which is not a shortcut. The
// consumer is go-xrkit/xrkit/headflow, whose Profile sums R+G+B down each
// column: a grey image carries exactly what it reads, and a full YUV-to-RGB
// conversion would compute chrominance for it to add up and discard. Every
// other byte of YUY2 IS the luminance, so this is one pass and no arithmetic.
//
// dst is reused when it is already the right size, so a steady stream
// allocates nothing.
func (a *FrameAssembler) Decode(dst *image.RGBA, frame []byte) (*image.RGBA, error) {
	if len(frame) < a.want {
		return nil, fmt.Errorf("%w: %d bytes for a %dx%d YUY2 frame, want %d",
			ErrShortPayload, len(frame), a.w, a.h, a.want)
	}
	return a.decodeInto(dst, frame), nil
}

// decodeInto is Decode once the length is known to be right.
//
// ⛔ IT EXISTS BECAUSE THE COVERAGE GATE FOUND THE CHECK ABOVE TO BE DEAD on
// the stream's path, and dead code is what that gate is for — [FrameAssembler]
// only ever completes a frame of exactly want bytes, so a stream handing one
// back could never take the error branch and nothing could ever test it. The
// check stays on the exported entry, where a caller may hand over bytes from
// anywhere; the stream calls this. It is the same answer urbTypeFor got when
// its error return turned out to be unreachable.
func (a *FrameAssembler) decodeInto(dst *image.RGBA, frame []byte) *image.RGBA {
	if dst == nil || dst.Rect.Dx() != a.w || dst.Rect.Dy() != a.h {
		dst = image.NewRGBA(image.Rect(0, 0, a.w, a.h))
	}
	for y := 0; y < a.h; y++ {
		src := frame[y*a.w*BytesPerPixelYUY2:]
		row := dst.Pix[y*dst.Stride:]
		for x := 0; x < a.w; x++ {
			l := src[x*BytesPerPixelYUY2]
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = l, l, l, 0xFF
		}
	}
	return dst
}

// UVCStreamStats counts what a stream has seen.
//
// It is declared here rather than beside the stream because it is data and
// travels everywhere: declaring a type once per platform is how String ends up
// on only one of them, which go vet caught once already in this package.
type UVCStreamStats struct {
	// Packets is every isochronous packet reaped, empty ones included.
	Packets int
	// Empty is how many carried nothing, which between frames is most of them.
	Empty int
	// Bytes is the payload actually delivered, headers and all.
	Bytes int
	// Frames, Dropped and Bad come from the assembler: whole frames, frames
	// abandoned for being short, and payloads the camera marked in error.
	Frames, Dropped, Bad int
	// Late is whole frames thrown away because the CALLER had not come back for
	// the ones before them.
	//
	// ⭐ IT IS SEPARATE FROM Dropped ON PURPOSE. A dropped frame is the
	// camera's or the bus's fault and says the link is lossy; a late one is
	// OURS and says the consumer is slower than the camera. They are different
	// problems with different fixes, and a single counter would make a slow
	// tracker look like a failing cable.
	Late int
	// Foreign is completions collected from this descriptor that belonged to
	// some other request.
	//
	// ⛔ IT IS COUNTED BECAUSE A COMPLETION QUEUE BELONGS TO THE DESCRIPTOR,
	// not to whoever submitted. Anything else reading the same device — or, as
	// happened here, this package's own [USBHandle.SubmitISO] cancelling a test
	// request without collecting it — puts a completion in the way. Skipping it
	// is right; skipping it silently would make a stream that is eating other
	// people's requests look merely slow.
	Foreign int
}

// String renders the counters for logs.
func (s UVCStreamStats) String() string {
	out := fmt.Sprintf("%d frames, %d dropped, %d late, %d bad payloads; "+
		"%d packets (%d empty), %d bytes",
		s.Frames, s.Dropped, s.Late, s.Bad, s.Packets, s.Empty, s.Bytes)
	if s.Foreign > 0 {
		// Only when there are any: a counter that is always zero in every log
		// line teaches readers to skip the line.
		out += fmt.Sprintf("; %d foreign completion(s)", s.Foreign)
	}
	return out
}
