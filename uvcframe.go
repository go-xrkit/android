// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"bytes"
	"fmt"
	"image"
	// ⭐ image/jpeg IS THE STANDARD LIBRARY'S, WHICH IS PURE GO. MJPEG was left
	// out of the frame path for one reason — "there is no JPEG decode in it" —
	// and the decoder was in the toolchain all along, needing no cgo and so no
	// exception to this module's CGO_ENABLED=0.
	"image/jpeg"
	"time"
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
	// kind decides what "whole" means: a length for YUY2, markers for MJPEG.
	kind UVCFormat
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
	// badJPEG counts frames that passed the marker test and still would not
	// decode, or decoded to a size other than the one committed.
	//
	// ⛔ IT IS SEPARATE FROM dropped BECAUSE IT IS A DIFFERENT FAULT. A dropped
	// frame never arrived whole; one of these did arrive whole, with both its
	// markers, and is still not a picture — which points at the camera's
	// compressor or at a corrupted middle, not at a lost tail. One counter for
	// both would hide whichever is rarer.
	badJPEG int
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

// NewFrameAssembler returns an assembler for uncompressed YUY2 frames of the
// given size. See [NewFrameAssemblerFor] for the compressed ones.
func NewFrameAssembler(w, h int) (*FrameAssembler, error) {
	return NewFrameAssemblerFor(FormatYUY2, w, h)
}

// NewFrameAssemblerFor returns an assembler for frames of the given format and
// size.
//
// ⛔⛔ THE FORMAT CHANGES WHAT "A WHOLE FRAME" MEANS, which is the whole reason
// this is not one code path with a flag. A YUY2 frame is whole when it is
// w*h*2 bytes: the LENGTH is the test, and a short one is a frame that lost
// packets. An MJPEG frame is a JPEG, so its length is whatever the compressor
// made it and the length says nothing at all — a frame that lost packets is the
// same length as a frame of a plainer scene. What tells them apart is the
// MARKERS, and nothing else does.
//
// Reaching for MJPEG is measured rather than preferred: the Beast's camera
// advertises 30 fps at every size for MJPEG and 25 fps only at 640x480 and below
// for YUY2, and the byte budget of a measured run says why — it fills 832 of the
// 2400 bytes an interval it committed to, which is 10.5 uncompressed frames a
// second whatever is asked of it.
func NewFrameAssemblerFor(kind UVCFormat, w, h int) (*FrameAssembler, error) {
	switch kind {
	case FormatYUY2, FormatMJPEG:
	case FormatUnknown:
		// ⛔ AND THE ZERO VALUE LANDS HERE, WHICH IS THE POINT. FormatUnknown is
		// what a descriptor this package could not name becomes, and it is also
		// what a UVCNegotiation built by hand carries. Either way nobody has
		// said whether the bytes are compressed, and the two ways of assembling
		// them are not interchangeable — so this refuses rather than defaulting
		// to the one that used to be the only option.
		return nil, fmt.Errorf("%w: no format was named, so there is no way to tell "+
			"whether a whole frame is a LENGTH or a pair of markers; set "+
			"UVCNegotiation.Kind, which Negotiate fills in", ErrInvalidOption)
	default:
		return nil, fmt.Errorf("%w: %s is not a format this package assembles",
			ErrInvalidOption, kind)
	}
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
	a := &FrameAssembler{kind: kind, w: w, h: h, want: want}
	if kind == FormatMJPEG {
		// ⛔ want IS NOT A TARGET HERE, IT IS A CEILING. A compressed frame of
		// this size can be any length up to the uncompressed one — beyond that
		// it is not a smaller picture, it is a device filling memory — so the
		// same number bounds the buffer while meaning something else, and
		// nothing may read it as "a whole frame is this long".
		a.buf = make([]byte, 0, want/8) // a tenth or so is typical; it grows
		return a, nil
	}
	a.buf = make([]byte, 0, want)
	return a, nil
}

// Size is the frame size this assembler was built for.
func (a *FrameAssembler) Size() (int, int) { return a.w, a.h }

// Format is the format this assembler was built for.
func (a *FrameAssembler) Format() UVCFormat { return a.kind }

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

// BadJPEG is how many MJPEG frames arrived whole — both markers present — and
// still would not decode, or decoded to a size other than the one committed.
//
// ⛔ IT IS NOT PART OF Stats BECAUSE IT IS A DIFFERENT FAULT. A dropped frame
// never arrived; one of these did, and is still not a picture, which points at
// the camera's compressor or at a corrupted middle rather than at a lost tail.
func (a *FrameAssembler) BadJPEG() int { return a.badJPEG }

// Feed takes one payload and returns a whole frame's bytes when one completed.
//
// ⛔⛔ THE BYTES ARE THE CALLER'S, AND THIS COMMENT USED TO SAY THE OPPOSITE. It
// claimed they were borrowed until the next Feed; [FrameAssembler.complete]
// hands the buffer over and allocates a fresh one, so a completed frame stays
// valid for as long as the caller keeps it. The code was the safer of the two
// and the promise was the wrong one — which matters, because that ownership is
// exactly what makes [UVCStream]'s queue of pending frames legal. A reader who
// trusted the old sentence would have read that queue as a use-after-free.
//
// ⚠ It costs one allocation a frame, measured at 611 KB and under 0.15 ms at
// 640x480 — two orders of magnitude below the frame interval, which is why it
// is spent rather than pooled. A free list would buy nothing here and would put
// an ownership bug where there is now none.
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
		// More bytes than a frame holds. For YUY2 the camera and this assembler
		// disagree about the size, and keeping the overflow would deliver a
		// frame whose rows are offset from the one before. For MJPEG the
		// ceiling is reached only by a device that will not stop sending, and
		// the frame then has no end-of-image marker and is dropped — which is
		// the right answer to "this will not fit in the memory I agreed to".
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

// JPEG markers, from ITU-T T.81: a frame begins with Start Of Image and ends
// with End Of Image, and nothing else in a well-formed stream carries them.
var (
	jpegSOI = []byte{0xFF, 0xD8}
	jpegEOI = []byte{0xFF, 0xD9}
)

// wholeJPEG reports whether the buffer holds a complete JPEG.
//
// ⛔⛔ A LENGTH TEST IS MEANINGLESS ON A COMPRESSED FRAME, which is the whole
// reason this exists. A JPEG of a plain wall is a fraction of the size of one of
// a bookshelf, so "shorter than expected" describes a scene and not a loss. The
// markers are the only thing that distinguishes a frame the camera finished from
// one the bus cut in half, and a truncated JPEG is exactly what a lost tail
// looks like: a valid beginning and no end.
//
// ⚠ IT DOES NOT DECODE. Deciding whether a frame is whole happens inside the
// reap loop, once per frame, and decoding there would put milliseconds where the
// isochronous schedule has microseconds. A buffer that passes this and still
// fails to decode is counted by [FrameAssembler.Decode] instead.
func wholeJPEG(b []byte) bool {
	return len(b) > len(jpegSOI)+len(jpegEOI) &&
		bytes.HasPrefix(b, jpegSOI) && bytes.HasSuffix(b, jpegEOI)
}

// complete closes the frame being assembled, returning its bytes when it is
// whole and counting it as dropped when it is not.
func (a *FrameAssembler) complete(why endedBy) []byte {
	n := len(a.buf)
	a.open = false
	if a.kind == FormatMJPEG {
		if !wholeJPEG(a.buf) {
			a.dropped++
			// ⚠ NOTHING IS ADDED TO short. The shortfall of a compressed frame
			// is unknowable — there is no expected length to subtract from — and
			// a zero here is honest where a guess would be a number people would
			// go on to average.
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
		a.buf = make([]byte, 0, a.want/8)
		return out
	}
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
	if a.kind == FormatMJPEG {
		return a.decodeJPEG(dst, frame)
	}
	if len(frame) < a.want {
		return nil, fmt.Errorf("%w: %d bytes for a %dx%d YUY2 frame, want %d",
			ErrShortPayload, len(frame), a.w, a.h, a.want)
	}
	return a.decodeInto(dst, frame), nil
}

// decodeJPEG turns one MJPEG frame into an RGBA image.
//
// ⭐ IT TAKES THE Y PLANE AND WRITES IT INTO ALL THREE CHANNELS, which is the
// same decision YUY2 gets and for the same reason: image/jpeg hands back an
// *image.YCbCr whose Y plane IS the luminance, and headflow.Profile sums R+G+B
// down each column. Converting YCbCr to RGB would compute chrominance for the
// consumer to add up and throw away — three multiplies and two clamps a pixel,
// at 30 frames a second, for a number nothing reads.
//
// ⛔ AND THE SIZE IS THE IMAGE'S, CHECKED. A camera that committed to 640x480
// and sends a 320x240 JPEG is within its rights to be wrong, and indexing the
// destination by the size we ASKED for would read past the decoded planes.
func (a *FrameAssembler) decodeJPEG(dst *image.RGBA, frame []byte) (*image.RGBA, error) {
	im, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		a.badJPEG++
		return nil, fmt.Errorf("%w: a %d-byte MJPEG frame would not decode: %w",
			ErrBadPayload, len(frame), err)
	}
	b := im.Bounds()
	if b.Dx() != a.w || b.Dy() != a.h {
		a.badJPEG++
		return nil, fmt.Errorf("%w: the camera committed to %dx%d and sent a %dx%d JPEG",
			ErrBadPayload, a.w, a.h, b.Dx(), b.Dy())
	}
	if dst == nil || dst.Rect.Dx() != a.w || dst.Rect.Dy() != a.h {
		dst = image.NewRGBA(image.Rect(0, 0, a.w, a.h))
	}
	yc, ok := im.(*image.YCbCr)
	if !ok {
		// A grayscale JPEG decodes to *image.Gray, and a camera may send one.
		// Anything else goes through the general path rather than being refused:
		// it is a picture, and this is not the place to be clever about formats.
		return a.drawAny(dst, im), nil
	}
	for y := 0; y < a.h; y++ {
		src := yc.Y[y*yc.YStride:]
		row := dst.Pix[y*dst.Stride:]
		for x := 0; x < a.w; x++ {
			l := src[x]
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = l, l, l, 0xFF
		}
	}
	return dst, nil
}

// drawAny copies any decoded image into dst as grey, for the formats image/jpeg
// may return that are not YCbCr.
func (a *FrameAssembler) drawAny(dst *image.RGBA, im image.Image) *image.RGBA {
	b := im.Bounds()
	for y := 0; y < a.h; y++ {
		row := dst.Pix[y*dst.Stride:]
		for x := 0; x < a.w; x++ {
			// The Y of YCbCr and the grey of Gray both come back as equal
			// R, G and B from At, so one channel is the luminance.
			r, _, _, _ := im.At(b.Min.X+x, b.Min.Y+y).RGBA()
			l := byte(r >> 8)
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = l, l, l, 0xFF
		}
	}
	return dst
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
	// Decoding is how long the decodes have taken in all.
	//
	// ⛔⛔ IT IS MEASURED ON THE DEVICE BECAUSE A BENCHMARK ELSEWHERE IS AN
	// OPTIMISTIC BOUND AND NOT A MEASUREMENT. Choosing between YUY2 and MJPEG
	// turns on whether the decode fits inside the frame interval, and the
	// processor that has to fit it is the phone's. On a workstation MJPEG costs
	// 3 % of a 30 fps interval at 320x240, 15-18 % at 640x480 and 69-84 % at
	// 1920x1080 — figures that say the shape of the answer and not the answer.
	Decoding time.Duration
	// BadJPEG is frames that arrived whole, both markers present, and still
	// would not decode. See [FrameAssembler.BadJPEG] for why it is apart from
	// Dropped.
	BadJPEG int
}

// String renders the counters for logs.
func (s UVCStreamStats) String() string {
	out := fmt.Sprintf("%d frames, %d dropped, %d late, %d bad payloads; "+
		"%d packets (%d empty), %d bytes",
		s.Frames, s.Dropped, s.Late, s.Bad, s.Packets, s.Empty, s.Bytes)
	if s.Frames > 0 && s.Decoding > 0 {
		out += fmt.Sprintf("; %s decoding, %s a frame",
			s.Decoding.Round(time.Millisecond),
			(s.Decoding / time.Duration(s.Frames)).Round(10*time.Microsecond))
	}
	// Only when there are any: a counter that is always zero in every log line
	// teaches readers to skip the line.
	if s.Foreign > 0 {
		out += fmt.Sprintf("; %d foreign completion(s)", s.Foreign)
	}
	if s.BadJPEG > 0 {
		out += fmt.Sprintf("; %d JPEG(s) that arrived whole and would not decode",
			s.BadJPEG)
	}
	return out
}
