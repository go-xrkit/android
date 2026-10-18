// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"image"
	"strings"
	"testing"
	"time"
)

// payload builds one isochronous packet the way a camera does: a header whose
// FIRST byte is its own length, then the pixels.
//
// ⭐ IT TAKES THE HEADER LENGTH AS AN ARGUMENT because that length is the thing
// most likely to be wrong in the code under test, and a helper that hard-coded
// two would make the shearing bug untestable.
func payload(headerLen int, bits byte, data ...byte) []byte {
	b := make([]byte, headerLen, headerLen+len(data))
	b[0] = byte(headerLen)
	if headerLen > 1 {
		b[1] = bits
	}
	return append(b, data...)
}

func TestAPayloadHeaderIsAsLongAsItSaysItIs(t *testing.T) {
	// ⛔ THE REGRESSION THIS GUARDS: a fixed two-byte header on a camera that
	// sends twelve prepends ten bytes of timestamp to every frame, which shears
	// the image by five YUY2 pixels and looks like a crooked lens.
	for _, n := range []int{2, 8, 12, 32} {
		p, err := ParsePayload(payload(n, 0, 1, 2, 3, 4))
		if err != nil {
			t.Fatalf("ParsePayload with a %d-byte header: %v", n, err)
		}
		if got := string(p.Data); got != "\x01\x02\x03\x04" {
			t.Errorf("a %d-byte header left %q as the pixels, want the four data bytes",
				n, got)
		}
	}
}

func TestAPayloadThatCannotBeTrustedIsRefused(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want error
	}{
		{"nothing at all", nil, ErrShortPayload},
		{"one byte", []byte{2}, ErrShortPayload},
		{"a header shorter than a header", payload(2, 0)[:2], nil},
		{"a header claiming one byte", []byte{1, 0}, ErrBadPayload},
		{"a header claiming zero", []byte{0, 0}, ErrBadPayload},
		// ⛔ A HEADER LONGER THAN ITS PACKET IS REFUSED RATHER THAN CLAMPED:
		// clamping would hand the assembler a slice from beyond the packet, and
		// the packet is not the one the camera meant to send either way.
		{"a 40-byte header in a 6-byte packet", []byte{40, 0, 1, 2, 3, 4}, ErrShortPayload},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePayload(c.in)
			switch {
			case c.want == nil && err != nil:
				t.Fatalf("ParsePayload = %v, want it accepted", err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Fatalf("ParsePayload = %v, want %v", err, c.want)
			}
		})
	}
}

func TestThePayloadBitsAreTheOnesTheSpecificationNames(t *testing.T) {
	// The three bits this package reads, at the positions UVC 1.5 section
	// 2.4.3.3 gives them. A table because getting one wrong is silent: reading
	// EOF where FID is means every frame ends on the first packet.
	for _, c := range []struct {
		bits          byte
		fid, eof, bad bool
	}{
		{0x00, false, false, false},
		{0x01, true, false, false},
		{0x02, false, true, false},
		{0x40, false, false, true},
		{0x43, true, true, true},
	} {
		p := UVCPayload{Header: c.bits}
		if p.FID() != c.fid || p.EOF() != c.eof || p.Bad() != c.bad {
			t.Errorf("header %#02x: FID=%v EOF=%v Bad=%v, want %v %v %v",
				c.bits, p.FID(), p.EOF(), p.Bad(), c.fid, c.eof, c.bad)
		}
	}
}

func TestAFrameAssemblerRefusesASizeItCannotTrack(t *testing.T) {
	for _, c := range []struct{ w, h int }{
		{0, 0}, {MinTrackingWidth - 1, 240}, {640, 0}, {640, -1},
	} {
		if _, err := NewFrameAssembler(c.w, c.h); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("NewFrameAssembler(%d, %d) = %v, want ErrInvalidOption", c.w, c.h, err)
		}
	}
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatalf("NewFrameAssembler(320, 2): %v", err)
	}
	if w, h := a.Size(); w != 320 || h != 2 {
		t.Errorf("Size = %dx%d, want 320x2", w, h)
	}
}

func TestAFrameSizeFromTheDeviceCannotChooseTheAllocation(t *testing.T) {
	// ⛔⛔ THE SIZE COMES FROM TWO 16-BIT USB DESCRIPTOR FIELDS, so a broken or
	// hostile camera on the other end of a cable can ask for 65535x65535 —
	// 8.6 GB, allocated twice over before anything else happens. The device is
	// not trusted with an allocation.
	for _, c := range []struct{ w, h int }{
		{65535, 65535},
		{4096, 8193}, // one row over the ceiling: 4096*8192 is exactly 64 MB
		{320, 1 << 40},
		// ⚠ AND THE OVERFLOW CASE ITSELF, which is why the check is a division:
		// w*h*2 wraps to a small positive number here, and a product-based
		// check would have accepted it and then indexed far past the buffer.
		{1 << 40, 1 << 40},
	} {
		if _, err := NewFrameAssembler(c.w, c.h); !errors.Is(err, ErrInvalidOption) {
			t.Errorf("NewFrameAssembler(%d, %d) = %v, want ErrInvalidOption",
				c.w, c.h, err)
		}
	}
	// And the largest frame the ceiling allows is still accepted, or the guard
	// would be refusing real cameras: the Beast's own largest is 1920x1080.
	if _, err := NewFrameAssembler(1920, 1080); err != nil {
		t.Errorf("NewFrameAssembler(1920, 1080) = %v, want it accepted", err)
	}
	if _, err := NewFrameAssembler(4096, 8192); err != nil {
		t.Errorf("NewFrameAssembler(4096, 8192) = %v; that is exactly %d bytes, the "+
			"ceiling, and a boundary refused is a boundary wrong by one",
			err, MaxFrameBytes)
	}
}

// feedFrame sends n bytes of value v in whole packets, and returns the frame that
// completed, if one did.
func feedFrame(t *testing.T, a *FrameAssembler, fid bool, v byte, n, per int) ([]byte, bool) {
	t.Helper()
	var bits byte
	if fid {
		bits = payloadFID
	}
	for sent := 0; sent < n; sent += per {
		data := make([]byte, min(per, n-sent))
		for i := range data {
			data[i] = v
		}
		p, err := ParsePayload(payload(2, bits, data...))
		if err != nil {
			t.Fatalf("ParsePayload on my own packet: %v", err)
		}
		if f, ok := a.Feed(p); ok {
			return f, true
		}
	}
	return nil, false
}

func TestAFrameEndsWhenTheFrameIdentifierToggles(t *testing.T) {
	// ⛔⛔ THE TOGGLE IS THE BOUNDARY AND NOT THE END-OF-FRAME BIT. Cameras that
	// never set EOF exist; on one of those, an assembler that waited for it
	// would run every frame into the next forever and deliver nothing. This
	// stream sets NO EOF at all.
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * 2 * BytesPerPixelYUY2

	if _, ok := feedFrame(t, a, false, 0x11, whole, 256); ok {
		t.Fatal("a frame came out before the identifier toggled")
	}
	// The first packet of the NEXT frame is what ends this one.
	frame, ok := feedFrame(t, a, true, 0x22, 256, 256)
	if !ok {
		t.Fatal("the identifier toggled and no frame came out")
	}
	if len(frame) != whole {
		t.Fatalf("the frame is %d bytes, want %d", len(frame), whole)
	}
	for i, b := range frame {
		if b != 0x11 {
			t.Fatalf("byte %d of the frame is %#02x, want the first frame's %#02x — the "+
				"next frame's first packet was appended to this one", i, b, 0x11)
		}
	}
	if f, d, b := a.Stats(); f != 1 || d != 0 || b != 0 {
		t.Errorf("Stats = %d frames, %d dropped, %d bad; want 1, 0, 0", f, d, b)
	}
}

func TestTheEndOfFrameBitEndsAFrameOnePacketEarlier(t *testing.T) {
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * 2 * BytesPerPixelYUY2

	// Everything but the last packet, then the last one WITH EOF set.
	if _, ok := feedFrame(t, a, false, 0x33, whole-128, 256); ok {
		t.Fatal("a frame came out early")
	}
	last := make([]byte, 128)
	for i := range last {
		last[i] = 0x33
	}
	p, err := ParsePayload(payload(2, payloadEOF, last...))
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := a.Feed(p)
	if !ok {
		t.Fatal("EOF did not end the frame; it is only honoured when it comes, but it " +
			"must be honoured")
	}
	if len(frame) != whole {
		t.Fatalf("the frame is %d bytes, want %d", len(frame), whole)
	}
}

func TestAShortFrameIsDroppedRatherThanPadded(t *testing.T) {
	// ⛔ PADDING A LOST PACKET WITH ZEROS PRODUCES A BLACK BAND, and a tracker
	// reads a black band as structure. Worse than no frame, so there is no
	// frame.
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * 2 * BytesPerPixelYUY2
	if _, ok := feedFrame(t, a, false, 0x44, whole-256, 256); ok {
		t.Fatal("a frame came out before it was whole")
	}
	if _, ok := feedFrame(t, a, true, 0x55, 256, 256); ok {
		t.Fatal("a frame 256 bytes short was delivered")
	}
	if f, d, _ := a.Stats(); f != 0 || d != 1 {
		t.Errorf("Stats = %d frames, %d dropped; want 0, 1", f, d)
	}
}

func TestMoreBytesThanAFrameHoldsAreTruncatedNotCarried(t *testing.T) {
	// A camera that sends more than the committed size and an assembler that
	// kept the overflow would offset every row of the following frame. The
	// frame that comes out is the right LENGTH, which is what the decoder
	// indexes by.
	a, err := NewFrameAssembler(320, 1)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * BytesPerPixelYUY2
	if _, ok := feedFrame(t, a, false, 0x66, whole+1024, 256); ok {
		t.Fatal("a frame came out before the identifier toggled")
	}
	frame, ok := feedFrame(t, a, true, 0x77, 256, 256)
	if !ok {
		t.Fatal("no frame came out")
	}
	if len(frame) != whole {
		t.Errorf("the frame is %d bytes, want exactly %d", len(frame), whole)
	}
}

func TestAPayloadTheCameraMarksBadAbandonsItsFrame(t *testing.T) {
	a, err := NewFrameAssembler(320, 1)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * BytesPerPixelYUY2
	if _, ok := feedFrame(t, a, false, 0x88, whole-256, 256); ok {
		t.Fatal("a frame came out early")
	}
	bad, err := ParsePayload(payload(2, payloadError))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Feed(bad); ok {
		t.Fatal("a bad payload delivered a frame")
	}
	// And the frame it ruined is gone rather than finished by the next packets:
	// the remaining 256 bytes must NOT complete it.
	if _, ok := feedFrame(t, a, false, 0x99, 256, 256); ok {
		t.Fatal("the abandoned frame was completed by the packets that followed, which " +
			"means it is short by whatever the bad payload carried")
	}
	if _, _, b := a.Stats(); b != 1 {
		t.Errorf("Stats reports %d bad payloads, want 1", b)
	}
}

// TestTheCensusSaysWHICHBoundaryEndedEachFrame.
//
// ⛔⛔ THE TOTALS DID NOT CLOSE ON THE DEVICE AND THAT IS WHY THIS EXISTS.
// The first streaming run delivered 10 frames and dropped 10, with about
// thirty-one frames' worth of packets for twenty frames seen. "frames" and
// "dropped" cannot say which boundary fired, and 11 toggles with 11 EOFs fits
// two opposite stories. Splitting the losses by boundary settled it in one run:
// 12 losses on toggles, 2 on EOFs — the camera sends same-identifier packets
// after its end-of-frame bit.
func TestTheCensusSaysWHICHBoundaryEndedEachFrame(t *testing.T) {
	const w, h = 320, 2
	a, err := NewFrameAssembler(w, h)
	if err != nil {
		t.Fatal(err)
	}
	whole := w * h * BytesPerPixelYUY2

	// A whole frame ended by EOF: delivered, and counted as an EOF boundary.
	feedFrame(t, a, false, 0x11, whole-128, 256)
	last := make([]byte, 128)
	p, err := ParsePayload(payload(2, payloadEOF, last...))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Feed(p); !ok {
		t.Fatal("a whole frame ended by EOF was not delivered")
	}

	// Then the device's own misbehaviour: more packets with the SAME
	// identifier, which open a frame that only the toggle can end.
	feedFrame(t, a, false, 0x22, 256, 256)
	feedFrame(t, a, true, 0x33, 256, 256) // the toggle, ending a 256-byte fragment

	tg, eo, short := a.Boundaries()
	if tg != 1 || eo != 1 {
		t.Errorf("Boundaries = %d toggle(s), %d EOF(s); want one of each", tg, eo)
	}
	if want := whole - 256; short != want {
		t.Errorf("the shortfall is %d bytes, want %d — the fragment held 256 of %d",
			short, want, whole)
	}
	onT, onE := a.Losses()
	if onT != 1 || onE != 0 {
		t.Errorf("Losses = %d on toggle, %d on EOF; want the loss attributed to the "+
			"TOGGLE, which is the boundary firing where it should not", onT, onE)
	}
	if f, d, _ := a.Stats(); f != 1 || d != 1 {
		t.Errorf("Stats = %d frames, %d dropped; want 1 and 1", f, d)
	}
}

// And the mirror image, so the split is not merely always-toggle.
func TestALossOnAnEndOfFrameBitIsAttributedToIt(t *testing.T) {
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Half a frame, then EOF: the camera marked the end early.
	feedFrame(t, a, false, 0x44, 256, 256)
	p, err := ParsePayload(payload(2, payloadEOF))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Feed(p); ok {
		t.Fatal("a frame a quarter full was delivered")
	}
	onT, onE := a.Losses()
	if onT != 0 || onE != 1 {
		t.Errorf("Losses = %d on toggle, %d on EOF; want it attributed to the EOF",
			onT, onE)
	}
}

// TestAPayloadWithNODATADoesNotBeginAFrame.
//
// ⛔⛔ IT USED TO, AND IT COST ONE PHANTOM DROPPED FRAME FOR EVERY REAL ONE. A
// header-only payload — a length, the bits, nothing after them — is how a camera
// keeps an isochronous stream alive between frames. It is NOT an empty packet,
// so the stream's zero-length skip never sees it, and it parses perfectly.
// Opening a frame on it meant the next identifier toggle completed a buffer with
// nothing in it. Measured on a Pixel 11 Pro Fold at 320x240, 2026-10-18:
//
//	before   10 frames, 10 dropped, 10 toggles, short by 153600 of 153600
//	after    10 frames,  0 dropped,  0 toggles
//
// Short by exactly a whole frame is a frame that carried NOTHING. The counters
// said the bus was losing half the frames; the bus was losing none, and the
// published diagnosis drawn from them — that the camera sent same-identifier
// packets after its end-of-frame bit — was this bug.
func TestAPayloadWithNoDataDoesNotBeginAFrame(t *testing.T) {
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * 2 * BytesPerPixelYUY2

	// A whole frame, ended by its own EOF.
	feedFrame(t, a, false, 0x11, whole-256, 256)
	last := make([]byte, 256)
	for i := range last {
		last[i] = 0x11
	}
	p, err := ParsePayload(payload(2, payloadEOF, last...))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Feed(p); !ok {
		t.Fatal("a whole frame was not delivered")
	}

	// Then the keep-alive the camera actually sends: header, no data. Several of
	// them, and then the next frame's identifier.
	for i := 0; i < 3; i++ {
		hdr, err := ParsePayload(payload(12, 0))
		if err != nil {
			t.Fatal(err)
		}
		if f, ok := a.Feed(hdr); ok {
			t.Fatalf("a header-only payload delivered a frame of %d bytes", len(f))
		}
	}
	// The toggle that used to complete a phantom.
	if _, ok := feedFrame(t, a, true, 0x22, 256, 256); ok {
		t.Fatal("the toggle after the keep-alives delivered a frame")
	}

	if f, d, _ := a.Stats(); f != 1 || d != 0 {
		t.Errorf("Stats = %d frames, %d dropped; want 1 and 0 — a dropped frame here "+
			"is one this assembler invented", f, d)
	}
	if tg, _, short := a.Boundaries(); tg != 0 || short != 0 {
		t.Errorf("Boundaries = %d toggle(s), %d short bytes; a keep-alive is not a "+
			"frame and must leave no trace", tg, short)
	}
}

// ⭐ AND A HEADER-ONLY PAYLOAD STILL ENDS A FRAME THAT IS OPEN, which is the
// legitimate UVC way to close one whose data has all arrived. Refusing to act on
// it would hold every frame until the next toggle — a whole frame-time of extra
// latency on the one method latency bounds.
func TestAHeaderOnlyPayloadStillEndsAnOpenFrame(t *testing.T) {
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	whole := 320 * 2 * BytesPerPixelYUY2
	if _, ok := feedFrame(t, a, false, 0x33, whole, 256); ok {
		t.Fatal("the frame completed before it was ended")
	}
	eof, err := ParsePayload(payload(12, payloadEOF))
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := a.Feed(eof)
	if !ok {
		t.Fatal("a header-only EOF did not end the open frame")
	}
	if len(frame) != whole {
		t.Errorf("the frame is %d bytes, want %d", len(frame), whole)
	}
}

func TestDecodeWritesLuminanceIntoEveryChannel(t *testing.T) {
	// ⭐ GREY IS NOT A SHORTCUT HERE: headflow.Profile sums R+G+B down each
	// column, so a grey image carries exactly what it reads and a full YUV
	// conversion would compute chrominance for it to add up and throw away.
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 320*2*BytesPerPixelYUY2)
	for i := range frame {
		if i%2 == 0 {
			frame[i] = byte(i / 2 % 256) // luminance
		} else {
			frame[i] = 0x80 // chrominance, which must not reach the image
		}
	}
	img, err := a.Decode(nil, frame)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if img.Rect.Dx() != 320 || img.Rect.Dy() != 2 {
		t.Fatalf("Decode gave a %v image, want 320x2", img.Rect)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 320; x++ {
			want := frame[(y*320+x)*BytesPerPixelYUY2]
			i := y*img.Stride + x*4
			r, g, b, al := img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]
			if r != want || g != want || b != want || al != 0xFF {
				t.Fatalf("pixel (%d,%d) = %d,%d,%d,%d; want %d in all three channels "+
					"and an opaque alpha", x, y, r, g, b, al, want)
			}
		}
	}
}

func TestDecodeReusesTheImageItIsGiven(t *testing.T) {
	// A steady stream must not allocate a frame's worth of image every frame.
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 320*2*BytesPerPixelYUY2)
	first, err := a.Decode(nil, frame)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Decode(first, frame)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("Decode allocated a second image for the same size")
	}
	// And an image of the WRONG size is replaced rather than written past.
	small := image.NewRGBA(image.Rect(0, 0, 16, 16))
	out, err := a.Decode(small, frame)
	if err != nil {
		t.Fatal(err)
	}
	if out == small {
		t.Error("Decode wrote a 320x2 frame into a 16x16 image")
	}
}

func TestDecodeRefusesAFrameTooShortToBeOne(t *testing.T) {
	a, err := NewFrameAssembler(320, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Decode(nil, make([]byte, 16)); !errors.Is(err, ErrShortPayload) {
		t.Errorf("Decode on a 16-byte frame = %v, want ErrShortPayload", err)
	}
}

func TestStreamStatsSayWhatHappened(t *testing.T) {
	s := UVCStreamStats{
		Packets: 900, Empty: 700, Bytes: 614400,
		Frames: 2, Dropped: 1, Bad: 3, Late: 4,
	}
	// ⛔ A COUNTER THAT IS ALWAYS ZERO IN EVERY LINE TEACHES READERS TO SKIP THE
	// LINE, so Foreign appears only when there is one — and both halves of that
	// are asserted.
	if got := s.String(); strings.Contains(got, "foreign") {
		t.Errorf("String() mentions foreign completions when there were none: %q", got)
	}
	if got := s.String(); strings.Contains(got, "decoding") {
		t.Errorf("String() reports a decoding time that was never measured: %q", got)
	}
	if got := s.String(); strings.Contains(got, "would not decode") {
		t.Errorf("String() mentions bad JPEGs when there were none: %q", got)
	}
	loud := s
	loud.Foreign = 7
	loud.BadJPEG = 3
	loud.Decoding = 250 * time.Millisecond
	for _, want := range []string{
		"7 foreign completion(s)",
		"3 JPEG(s) that arrived whole and would not decode",
		"250ms decoding",
		"125ms a frame", // 250ms over the two frames
	} {
		if got := loud.String(); !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
	got := s.String()
	// ⭐ Late AND Dropped ARE BOTH NAMED, because they are different problems:
	// a dropped frame says the link is lossy, a late one says we are slow, and
	// one counter for both would make a slow tracker look like a bad cable.
	for _, want := range []string{
		"2 frames", "1 dropped", "4 late", "3 bad", "900 packets", "700 empty",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
