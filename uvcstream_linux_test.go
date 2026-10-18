// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// isoEndpoint is a high-speed isochronous IN endpoint carrying `per` bytes an
// interval, written the way a camera writes it: a size in bits 10:0 and the
// additional transactions in bits 12:11.
func isoEndpoint(per int) USBEndpoint {
	size, extra := per, 0
	for size > 1024 {
		size = per / (extra + 2)
		extra++
	}
	return USBEndpoint{
		Address:       0x81,
		Attributes:    int(USBTransferIsochronous),
		MaxPacketSize: size | extra<<11,
		Interval:      1,
	}
}

// A fakeStream answers the three stream seams without a kernel.
//
// ⛔ IT IS INSTALLED AT submitURB/reapURB/discardURB AND NOT AT THE IOCTL. Under
// the ioctl it would be handed one unsafe.Pointer and would have to turn the
// URB's Buffer field — a uintptr — back into a pointer to put packets where the
// stream reads them; that is the misuse `go vet` refuses and it crashed this
// suite once, in the probe/commit tests, which is why controlTransfer is a seam
// at the same height. Here it is handed the *urbSlot and writes into an
// ordinary Go slice.
type fakeStream struct {
	t      *testing.T
	packet int

	// queue is the slots submitted and not yet reaped, oldest first. A real
	// kernel completes them in order and so does this.
	queue []*urbSlot

	// packets is what the camera has left to send, one entry per isochronous
	// packet. A nil entry is an empty packet, which is what an idle endpoint
	// sends and which must not be mistaken for the end of anything.
	packets [][]byte

	submits, reaps, discards int

	// submitErrAfter fails the Nth submit, counting from one; zero never fails.
	submitErrAfter int
	// reapErr, when set, is what reapURB answers.
	reapErr error
	// reapUnknown makes the next reap name an address no slot owns.
	reapUnknown bool
}

// install swaps the seams for the length of one test.
func (f *fakeStream) install() {
	oldSubmit, oldReap, oldDiscard := submitURB, reapURB, discardURB
	f.t.Cleanup(func() { submitURB, reapURB, discardURB = oldSubmit, oldReap, oldDiscard })

	submitURB = func(_ int, slot *urbSlot) error {
		f.submits++
		if f.submitErrAfter != 0 && f.submits >= f.submitErrAfter {
			return syscall.ENODEV
		}
		f.queue = append(f.queue, slot)
		return nil
	}
	reapURB = func(int) (uintptr, error) {
		f.reaps++
		if f.reapErr != nil {
			return 0, f.reapErr
		}
		if f.reapUnknown {
			f.reapUnknown = false
			// An address no slot owns. It is this fake's OWN memory rather than
			// a made-up number, so nothing here depends on a value the
			// allocator chose.
			return uintptr(unsafe.Pointer(f)), nil
		}
		if len(f.queue) == 0 || len(f.packets) == 0 {
			return 0, syscall.EAGAIN
		}
		slot := f.queue[0]
		f.queue = f.queue[1:]
		f.fill(slot)
		return slot.addr, nil
	}
	discardURB = func(_ int, slot *urbSlot) error {
		f.discards++
		for i, q := range f.queue {
			if q == slot {
				f.queue = append(f.queue[:i], f.queue[i+1:]...)
				break
			}
		}
		return nil
	}
}

// fill writes the next request's worth of packets into one slot.
func (f *fakeStream) fill(slot *urbSlot) {
	for i := 0; i < StreamPacketsPerURB; i++ {
		d := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&slot.raw[urbSize+isoPacketSize*i]))
		if len(f.packets) == 0 {
			d.ActualLength = 0
			continue
		}
		pkt := f.packets[0]
		f.packets = f.packets[1:]
		if len(pkt) > f.packet {
			f.t.Fatalf("this fake was asked to send a %d-byte packet on a %d-byte endpoint",
				len(pkt), f.packet)
		}
		copy(slot.buf[i*f.packet:], pkt)
		d.ActualLength = uint32(len(pkt))
	}
}

// sendFrame queues one whole frame's packets, with the given frame identifier.
func (f *fakeStream) sendFrame(fid bool, v byte, bytes_, per int) {
	var bits byte
	if fid {
		bits = payloadFID
	}
	for sent := 0; sent < bytes_; sent += per {
		n := min(per, bytes_-sent)
		data := make([]byte, n)
		for i := range data {
			data[i] = v
		}
		f.packets = append(f.packets, payload(2, bits, data...))
	}
}

// sendJPEG queues one whole JPEG's packets, ending the last one with EOF the
// way a camera marks a compressed frame it has finished.
func (f *fakeStream) sendJPEG(frame []byte, fid bool) {
	var bits byte
	if fid {
		bits = payloadFID
	}
	const per = 1024
	for off := 0; off < len(frame); off += per {
		end := min(off+per, len(frame))
		b := bits
		if end == len(frame) {
			b |= payloadEOF
		}
		f.packets = append(f.packets, payload(2, b, frame[off:end]...))
	}
}

// sendEmpty queues n packets carrying nothing, which is what the endpoint
// delivers between frames.
func (f *fakeStream) sendEmpty(n int) {
	for i := 0; i < n; i++ {
		f.packets = append(f.packets, nil)
	}
}

func fakeHandle() *USBHandle { return &USBHandle{name: "001/002", fd: -1} }

func streamCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAnEndpointSizeIsDecodedBeforeItIsUsed(t *testing.T) {
	// ⛔⛔ THE DEFECT THIS GUARDS COST A CYCLE AND A WRONG CONCLUSION: the raw
	// wMaxPacketSize was passed to the kernel, which answered EMSGSIZE, and the
	// probe reported it as "the kernel refused the camera". 0x1400 is 1024 with
	// two ADDITIONAL transactions, so 3072 bytes an interval.
	e := USBEndpoint{Address: 0x81, Attributes: 1, MaxPacketSize: 0x1400}
	if got := e.ISOPacketSize(); got != 3072 {
		t.Errorf("ISOPacketSize on 0x1400 = %d, want 3072", got)
	}
	if got := e.ISOPacketSize(); got == e.MaxPacketSize {
		t.Errorf("ISOPacketSize returned the raw field (%d); that field is not a size", got)
	}
}

func TestAStreamRefusesAnEndpointThatCarriesNothing(t *testing.T) {
	h := fakeHandle()
	// bits 10:0 zero: a size of nothing, whatever the multiplier says.
	ep := USBEndpoint{Address: 0x81, Attributes: 1, MaxPacketSize: 0x1800}
	if _, err := h.Stream(UVCNegotiation{Kind: FormatYUY2}, ep, 320, 240); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("Stream on a zero-size endpoint = %v, want ErrInvalidOption", err)
	}
	// And a frame size it cannot track is refused before anything is submitted.
	if _, err := h.Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 16, 16); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("Stream on a 16x16 frame = %v, want ErrInvalidOption", err)
	}
}

func TestAStreamFillsTheRingBeforeItReadsAnything(t *testing.T) {
	// ⛔ ONE REQUEST IN FLIGHT IS NOT ENOUGH AND IT IS NOT A TUNING CHOICE: an
	// isochronous endpoint is scheduled every microframe whether anybody is
	// listening, so between reaping one request and submitting the next the
	// camera's packets go nowhere. The ring is full before Frame is ever
	// called.
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	if f.submits != StreamURBs {
		t.Errorf("Stream submitted %d requests, want %d before reading anything",
			f.submits, StreamURBs)
	}
	if len(s.ring) != StreamURBs {
		t.Errorf("the ring holds %d slots, want %d", len(s.ring), StreamURBs)
	}
}

func TestAStreamDeliversAFrameAndKeepsTheRingFull(t *testing.T) {
	const w, h = 320, 4
	whole := w * h * BytesPerPixelYUY2

	f := &fakeStream{t: t, packet: 3072}
	f.install()
	// Idle first, because a stream that only worked on a camera already talking
	// would hang on a real one; then a frame, then the next frame's first
	// packet, which is what ends it.
	f.sendEmpty(StreamPacketsPerURB)
	f.sendFrame(false, 0x5A, whole, 1024)
	f.sendFrame(true, 0xA5, 1024, 1024)

	s, err := fakeHandle().Stream(UVCNegotiation{Format: 1, Frame: 2, Kind: FormatYUY2},
		isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	img, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if img.Rect.Dx() != w || img.Rect.Dy() != h {
		t.Fatalf("Frame gave a %v image, want %dx%d", img.Rect, w, h)
	}
	// ⛔⛔ AND IT CHECKS THE PIXELS, BECAUSE A BLACK FRAME LOOKS EXACTLY LIKE A
	// WORKING STREAM TO A COUNTER. That mistake is this package's oldest, and
	// the frame was sent as 0x5A for the whole of it.
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0x5A {
			t.Fatalf("pixel %d is %#02x, want %#02x — the stream counted a frame it did "+
				"not actually carry", i/4, img.Pix[i], 0x5A)
		}
	}

	st := s.Stats()
	if st.Frames != 1 {
		t.Errorf("Stats reports %d frames, want 1 (%s)", st.Frames, st)
	}
	if st.Empty < StreamPacketsPerURB {
		t.Errorf("Stats reports %d empty packets, want at least %d: an idle endpoint's "+
			"empty packets are normal and must be counted, not dropped",
			st.Empty, StreamPacketsPerURB)
	}
	if st.Bytes < whole {
		t.Errorf("Stats reports %d bytes for a %d-byte frame", st.Bytes, whole)
	}
	// Every slot the stream emptied was submitted again: the ring cannot run
	// down, or the camera's packets start going nowhere a request at a time.
	if f.submits <= StreamURBs {
		t.Errorf("%d submits in all; a reaped request was not resubmitted", f.submits)
	}
	if got := s.Negotiation(); got.Format != 1 || got.Frame != 2 {
		t.Errorf("Negotiation = %+v, want the one Stream was given", got)
	}
}

func TestTwoFramesInOneRequestAreBothDelivered(t *testing.T) {
	// ⛔⛔ THE FIRST VERSION OF drain OVERWROTE ITS RESULT, so a request that
	// completed two frames delivered the last and counted both: Stats said two,
	// the caller got one, and the second Frame call then hung until its
	// deadline. StreamPacketsPerURB packets a request means a frame small enough
	// to fit in fewer than that finishes twice inside one drain — which never
	// happens at 640x480, where a frame is some two hundred packets. THIS TEST
	// IS THE ONLY WAY THAT DEFECT WAS EVER GOING TO BE SEEN.
	const w, h = 320, 2
	whole := w * h * BytesPerPixelYUY2 // 1280 bytes: two packets a frame
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	f.sendFrame(false, 0x11, whole, 1024)
	f.sendFrame(true, 0x22, whole, 1024)
	f.sendFrame(false, 0x33, 1024, 1024) // ends the second

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Both frames fit in the first request's eight packets, and both must come
	// out — in order, and with their own pixels.
	for i, want := range []byte{0x11, 0x22} {
		img, err := s.Frame(streamCtx(t))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got := img.Pix[0]; got != want {
			t.Fatalf("frame %d carries %#02x, want %#02x: the frames came out in the "+
				"wrong order, or one was lost and the other delivered twice",
				i, got, want)
		}
	}
	if st := s.Stats(); st.Frames != 2 {
		t.Errorf("Stats reports %d frames and two were delivered (%s)", st.Frames, st)
	}
}

func TestACallerSlowerThanTheCameraLosesTheOldestFrames(t *testing.T) {
	// A queue without a ceiling grows without bound behind a slow caller. The
	// consumer here is head tracking, which wants the FRESHEST frame, so the
	// oldest go — and they are counted as Late rather than Dropped, because a
	// slow tracker is not a failing cable.
	// ⭐ ONE PACKET A FRAME, so that MORE than the ceiling completes inside a
	// single request: a 320x1 frame is 640 bytes and a request carries
	// StreamPacketsPerURB of them. A 320x2 frame takes two packets, four frames
	// fit, and four is not more than four — the first version of this test
	// never reached the shedding at all and passed anyway.
	//
	// ⛔⛔ AND THE CEILING IS MaxPendingFrames, WHICH USED TO BE StreamURBs. That
	// was a coincidence: raising the ring from 4 to 16 for a measured doubling of
	// the frame rate made this branch unreachable, and the gate caught it. The
	// two numbers answer different questions — how many requests are in flight,
	// and how stale a frame may be before a tracker stops wanting it.
	const w, h = 320, 1
	whole := w * h * BytesPerPixelYUY2
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	for i := 0; i < StreamPacketsPerURB; i++ {
		f.sendFrame(i%2 == 1, byte(0x10+i), whole, whole)
	}

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	img, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}

	st := s.Stats()
	if st.Late == 0 {
		t.Fatalf("%d frames completed in one request against a ceiling of %d and none "+
			"was shed; the queue has no ceiling (%s)", st.Frames, MaxPendingFrames, st)
	}
	// Every frame the assembler finished is either delivered, still queued, or
	// counted late. ⛔ A COUNTER THAT DOES NOT ADD UP IS THE DEFECT THIS WHOLE
	// FILE EXISTS FOR.
	if st.Late+len(s.pending)+1 != st.Frames {
		t.Errorf("%d frames assembled, 1 delivered, %d still queued, %d late; they must "+
			"add up (%s)", st.Frames, len(s.pending), st.Late, st)
	}
	// And the frame handed over is NOT the oldest: the oldest is what was shed,
	// because head tracking wants the freshest.
	if got := img.Pix[0]; got == 0x10 {
		t.Errorf("the frame delivered is the oldest one (%#02x); the oldest is what gets "+
			"shed, or a tracker runs on stale pixels", got)
	}
}

// TestAForeignCompletionIsSkippedAndCOUNTED.
//
// ⛔⛔ IT WAS FATAL, AND THAT ENDED THE FIRST ON-DEVICE RUN ON ITS FIRST CALL.
// The probe's own SubmitISO had cancelled a test request without reaping it; a
// cancelled request still completes and still waits in the queue, and the queue
// belongs to the DESCRIPTOR rather than to whoever submitted. The stream
// collected that orphan, said it did not own it, and stopped:
//
//	STEP 8 stream  0 frame(s) in 0s, 0 packets
//	STOPPED BY: … completed a request at 0x79f466a000 that this stream does not own
//
// That litter is cleaned up at the source now (see reapOwn), but one stray
// request from anything else reading the same device must not end a working
// camera. It is counted, because the alternative to fatal is not silence.
func TestAForeignCompletionIsSkippedAndCounted(t *testing.T) {
	const w, h = 320, 2
	whole := w * h * BytesPerPixelYUY2
	f := &fakeStream{t: t, packet: 3072, reapUnknown: true}
	f.install()
	f.sendFrame(false, 0x5E, whole, 1024)
	f.sendFrame(true, 0x5F, 1024, 1024)

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The foreign completion comes first, and the frame still arrives.
	img, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("Frame = %v; a stray completion must not end the stream", err)
	}
	if got := img.Pix[0]; got != 0x5E {
		t.Errorf("the frame carries %#02x, want %#02x", got, 0x5E)
	}
	st := s.Stats()
	if st.Foreign != 1 {
		t.Errorf("Stats reports %d foreign completion(s), want 1: skipping one "+
			"silently would make a stream eating other people's requests look "+
			"merely slow (%s)", st.Foreign, st)
	}
	if s.Assembler() == nil {
		t.Error("Assembler() is nil on a running stream")
	}
}

// TestSubmitISOLeavesNothingInTheQueue.
//
// ⛔⛔ DISCARDING A REQUEST DOES NOT REMOVE IT FROM THE COMPLETION QUEUE, and
// SubmitISO discarded without reaping for as long as nothing else read the same
// descriptor. The first thing that did — the stream — got the orphan.
func TestSubmitISOLeavesNothingInTheQueue(t *testing.T) {
	ep := isoEndpoint(3072)
	// A fake kernel that remembers what was submitted, discarded and reaped.
	var submitted, discarded []uintptr
	var reaps int
	queue := []uintptr{}
	oldSubmit, oldReap, oldDiscard, oldIoctl := submitURB, reapURB, discardURB, ioctl
	t.Cleanup(func() {
		submitURB, reapURB, discardURB, ioctl = oldSubmit, oldReap, oldDiscard, oldIoctl
	})
	// SubmitISO goes through the raw ioctl, not through the slot seams, so the
	// submit is intercepted there and the ADDRESS is what the kernel would be
	// handed.
	ioctl = func(_ int, req uintptr, p unsafe.Pointer) error {
		switch req {
		case usbdevfsSubmitURB:
			a := uintptr(p)
			submitted = append(submitted, a)
			queue = append(queue, a)
			return nil
		case usbdevfsDiscardURB:
			discarded = append(discarded, uintptr(p))
			return nil
		}
		return nil
	}
	reapURB = func(int) (uintptr, error) {
		reaps++
		if len(queue) == 0 {
			return 0, syscall.EAGAIN
		}
		got := queue[0]
		queue = queue[1:]
		return got, nil
	}

	if err := (&USBHandle{name: "001/002", fd: -1}).SubmitISO(ep); err != nil {
		t.Fatalf("SubmitISO: %v", err)
	}
	if len(submitted) != 1 || len(discarded) != 1 {
		t.Fatalf("submitted %d, discarded %d; want one of each",
			len(submitted), len(discarded))
	}
	if len(queue) != 0 {
		t.Errorf("%d request(s) left in the completion queue; the next reader gets "+
			"them and cannot tell them from its own", len(queue))
	}
	if reaps == 0 {
		t.Error("SubmitISO never reaped, so the request it cancelled is still queued")
	}
}

// reapOwn must not sit forever on a request that never comes back, and must not
// keep taking other readers' completions while it looks.
func TestReapOwnIsBoundedAndDoesNotEatOthers(t *testing.T) {
	oldReap := reapURB
	t.Cleanup(func() { reapURB = oldReap })

	t.Run("a request that never completes", func(t *testing.T) {
		n := 0
		reapURB = func(int) (uintptr, error) { n++; return 0, syscall.EAGAIN }
		reapOwn(-1, 0xdeadbeef)
		if n != ReapTries {
			t.Errorf("tried %d times, want the bound of %d: waiting forever would "+
				"hang a probe on a question it has already answered", n, ReapTries)
		}
	})

	t.Run("somebody else's completion stops the search", func(t *testing.T) {
		n := 0
		reapURB = func(int) (uintptr, error) { n++; return 0x1234, nil }
		reapOwn(-1, 0xdeadbeef)
		if n != 1 {
			t.Errorf("took %d completions; one that is not ours has already left the "+
				"queue and cannot be put back, so the search must stop rather than "+
				"keep taking", n)
		}
	})

	t.Run("a descriptor in no state to be asked", func(t *testing.T) {
		n := 0
		reapURB = func(int) (uintptr, error) { n++; return 0, syscall.ENODEV }
		reapOwn(-1, 0xdeadbeef)
		if n != 1 {
			t.Errorf("asked %d times after ENODEV; the descriptor will not answer "+
				"again and retrying only delays the caller", n)
		}
	})
}

func TestTheKernelsMemoryIsHeldByTheDescriptorNotTheStream(t *testing.T) {
	// ⛔⛔ A SUBMITTED REQUEST HAS THE KERNEL WRITING INTO THIS PROCESS'S HEAP
	// ON THE CAMERA'S SCHEDULE, and it keeps doing so until the request is
	// discarded or the DESCRIPTOR is closed. A UVCStream that became
	// unreachable without Close would let its slots be collected while the
	// kernel still held their addresses — a use-after-free performed by the
	// kernel, landing in whatever the allocator handed out next, and reported
	// by nothing. So the memory is owned by the fd, whose closing is what
	// actually stops the writes.
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	h := fakeHandle()
	s, err := h.Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.pinned) != StreamURBs {
		t.Fatalf("the handle pins %d of %d requests; the rest are collectable while the "+
			"kernel holds their addresses", len(h.pinned), StreamURBs)
	}
	// Every slot in the ring is one the handle is holding.
	for i, slot := range s.ring {
		if !slices.Contains(h.pinned, slot) {
			t.Errorf("ring slot %d is not pinned by the handle", i)
		}
	}
	// ⛔ AND CLOSING THE STREAM DOES NOT RELEASE IT. The stream discards its
	// requests, but the fd is still open and the kernel is still entitled to
	// the addresses it was given until that fd closes.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(h.pinned) != StreamURBs {
		t.Errorf("closing the stream let go of %d pinned requests while the descriptor "+
			"is still open", StreamURBs-len(h.pinned))
	}
}

// ⛔⛔ A SEAM TESTED ONLY THROUGH ITS REPLACEMENT IS A SEAM NOBODY HAS RUN. Every
// other test in this file swaps these three out, so the production bodies —
// where the ioctl number and the pointer meet — would be covered by nothing. A
// memfd answers ENOTTY, which is the kernel's own refusal for this descriptor
// and exactly the shape of answer the probe must report rather than swallow.
func TestTheStreamSeamsRunAgainstARealDescriptor(t *testing.T) {
	_, h := openCamera(t)
	ep := isoEndpoint(3072)
	slot := newURBSlot(ep, ep.ISOPacketSize())

	if err := submitURB(h.fd, slot); !refusedByTheKernel(err) {
		t.Errorf("submitURB on a memfd = %v, want a refusal", err)
	}
	if _, err := reapURB(h.fd); !refusedByTheKernel(err) {
		t.Errorf("reapURB on a memfd = %v, want a refusal", err)
	}
	if err := discardURB(h.fd, slot); !refusedByTheKernel(err) {
		t.Errorf("discardURB on a memfd = %v, want a refusal", err)
	}
}

// refusedByTheKernel reports whether err is a refusal that came from a real
// syscall, which is the whole of what a seam test can assert here.
//
// ⛔⛔ IT DOES NOT DEMAND ENOTTY, AND THE REASON IS MEASURED RATHER THAN
// GUESSED. USBDEVFS_DISCARDURB is _IO('U', 11) = 0x550b, and qemu-user has that
// number in its own ioctl table: it unmarshals a usbdevfs_urb from the guest
// pointer before the kernel is ever reached, and answers EFAULT. Four
// architectures, the same four calls, /dev/zero:
//
//	arch              0x550b     0x5563, 0x5a01, 0x8004550f
//	amd64 (native)    ENOTTY     ENOTTY
//	ppc64le (qemu)    ENOTTY     ENOTTY
//	s390x (qemu)      EFAULT     ENOTTY
//	riscv64 (qemu)    EFAULT     ENOTTY
//
// So it is THAT ONE NUMBER and not "an ioctl qemu does not know" — the three
// unknown ones fall through to the kernel on every lane. ppc64le escapes
// because PowerPC encodes _IOC differently (_IOC_NONE is 1, not 0), so 0x550b
// is not that command there and qemu's table does not match it.
//
// ⚠ AND THAT IS WORTH KNOWING FOR A SECOND REASON: this package computes
// asm-generic _IOC encodings unconditionally, which is right for every
// architecture Android runs on and wrong for PowerPC, MIPS and SPARC. Those are
// cross-build lanes here and never hosts, so nothing is broken — but a usbfs
// ioctl number is not portable arithmetic, and the ppc64le column above is what
// that looks like from the outside.
//
// What the assertion keeps is the thing the seam exists to prove: the body ran
// a syscall and SURFACED the errno rather than swallowing it. A bare
// `err != nil` would not — it cannot tell a refusal from a nil-pointer panic
// recovered somewhere, so the error must be an errno, and a non-zero one.
func refusedByTheKernel(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno != 0
}

// TestAnMJPEGStreamDeliversPicturesAndTimesItsOwnDecode.
//
// ⛔⛔ THE WHOLE RING HAD TO LEARN THE FORMAT, not just the assembler: a
// compressed frame is whole by its MARKERS and an uncompressed one by its
// LENGTH, and this path is where the two meet. It also measures what the decode
// costs, which is the number the choice between the formats turns on and which a
// benchmark on a workstation can only bound.
func TestAnMJPEGStreamDeliversPicturesAndTimesItsOwnDecode(t *testing.T) {
	const w, h = 320, 240
	frame := jpegFrame(t, w, h, 80)
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	// Two whole JPEGs, each ended by its own EOF, as a camera sends them.
	f.sendJPEG(frame, false)
	f.sendJPEG(frame, true)

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatMJPEG}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	img, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	// ⛔ THE BARS, NOT THE COUNT. A uniformly grey image is what a broken decode
	// looks like to a counter, exactly as a black frame is.
	var dark, light int
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] < 0x60 {
			dark++
		} else {
			light++
		}
	}
	if dark == 0 || light == 0 {
		t.Fatalf("%d dark and %d light pixels: the encoded bars did not survive the "+
			"stream", dark, light)
	}

	st := s.Stats()
	// ⚠ NOT "ONE FRAME". Both JPEGs are small enough to complete inside a single
	// request — the very case TestTwoFramesInOneRequestAreBothDelivered exists
	// for — so the honest assertion is the one that ADDS UP: every frame the
	// assembler finished is delivered, queued, or shed.
	if st.Frames < 1 {
		t.Fatalf("no frame was assembled (%s)", st)
	}
	if st.Late+len(s.pending)+1 != st.Frames {
		t.Errorf("%d frames assembled, 1 delivered, %d queued, %d late; they must add "+
			"up (%s)", st.Frames, len(s.pending), st.Late, st)
	}
	if st.Decoding <= 0 {
		t.Errorf("Stats reports no decoding time; that number is what the choice "+
			"between YUY2 and MJPEG turns on and nothing else can measure it on "+
			"the processor that has to run it (%s)", st)
	}
	// ⚠ NO UPPER BOUND IS ASSERTED. How long a decode takes is a property of the
	// machine, and a test that failed on a slow one would be measuring the
	// runner. What must hold is that it was measured at all.
	if st.BadJPEG != 0 {
		t.Errorf("Stats reports %d bad JPEG(s) for two good ones (%s)", st.BadJPEG, st)
	}
}

// TestAnMJPEGFrameThatWillNotDecodeDoesNotENDTheStream.
//
// ⛔ ONE BAD JPEG MUST NOT BE A DEAD CAMERA. The frame arrived whole — both
// markers — and is still not a picture, which is the camera's compressor or a
// corrupted middle. Counting it and waiting for the next frame is the only
// answer that keeps a stream alive through one; returning the error would end it.
func TestAnMJPEGFrameThatWillNotDecodeDoesNotEndTheStream(t *testing.T) {
	const w, h = 320, 240
	good := jpegFrame(t, w, h, 80)
	ruined := jpegFrame(t, w, h, 80)
	for i := len(jpegSOI) + 4; i < len(ruined)-len(jpegEOI)-4; i++ {
		ruined[i] = 0x00
	}
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	f.sendJPEG(ruined, false)
	f.sendJPEG(good, true)
	f.sendJPEG(good, false)

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatMJPEG}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Frame(streamCtx(t)); err != nil {
		t.Fatalf("Frame = %v; one undecodable JPEG ended the stream", err)
	}
	if st := s.Stats(); st.BadJPEG != 1 {
		t.Errorf("Stats reports %d bad JPEG(s), want 1 — and counted apart from "+
			"Dropped, because a frame that never arrived and one that arrived and "+
			"is not a picture are different faults (%s)", st.BadJPEG, st)
	}
}

func TestAStreamReusesOneImage(t *testing.T) {
	const w, h = 320, 2
	whole := w * h * BytesPerPixelYUY2
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	for i := 0; i < 3; i++ {
		f.sendFrame(i%2 == 1, byte(0x10*i+0x10), whole, 1024)
	}
	f.sendFrame(false, 0xFF, 1024, 1024)

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	first, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("first Frame: %v", err)
	}
	second, err := s.Frame(streamCtx(t))
	if err != nil {
		t.Fatalf("second Frame: %v", err)
	}
	if first != second {
		t.Error("Frame allocated a second image; a steady stream must allocate nothing")
	}
}

func TestAStreamWaitsRatherThanSpinning(t *testing.T) {
	// A camera with nothing to say must not burn a core. The fake answers
	// EAGAIN until the context expires, and the stream reports the context
	// rather than inventing a frame.
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Frame(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Frame on a silent camera = %v, want the context's error", err)
	}
	// ⚠ THE BOUND IS GENEROUS ON PURPOSE. This asserts that the loop SLEEPS,
	// not how long: at one millisecond a turn, thirty milliseconds is about
	// thirty reaps, and a spinning loop would be in the thousands. A tight
	// bound here would fail under load, and this fleet has a rule about
	// measuring under load.
	if f.reaps > 500 {
		t.Errorf("%d reap attempts in 30ms: the wait is a busy loop", f.reaps)
	}
}

func TestAStreamStopsWhenItIsClosed(t *testing.T) {
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// ⛔ EVERY OUTSTANDING REQUEST IS DISCARDED. One left in flight has the
	// kernel writing into this process's memory on the camera's schedule, and
	// the Go heap is entitled to move on without it.
	if f.discards != StreamURBs {
		t.Errorf("Close discarded %d of %d outstanding requests", f.discards, StreamURBs)
	}
	if _, err := s.Frame(streamCtx(t)); !errors.Is(err, ErrClosed) {
		t.Errorf("Frame after Close = %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("the second Close = %v, want it idempotent", err)
	}
	if f.discards != StreamURBs {
		t.Errorf("the second Close discarded again (%d in all)", f.discards)
	}
	// A context already cancelled is reported before anything else is touched.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Frame(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Frame with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestAStreamReportsWhatTheKernelRefuses(t *testing.T) {
	t.Run("a submit that fails while the ring fills", func(t *testing.T) {
		f := &fakeStream{t: t, packet: 3072, submitErrAfter: 2}
		f.install()
		_, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
		if !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("Stream = %v, want the kernel's ENODEV", err)
		}
		// ⛔ AND IT CLEANS UP WHAT IT DID QUEUE. A failed Stream that left one
		// request outstanding would leave the kernel writing into memory this
		// process has forgotten.
		if f.discards == 0 {
			t.Error("Stream failed without discarding the request it had already queued")
		}
	})

	t.Run("a reap that fails", func(t *testing.T) {
		f := &fakeStream{t: t, packet: 3072, reapErr: syscall.ENODEV}
		f.install()
		s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), 320, 240)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.Frame(streamCtx(t)); !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("Frame = %v, want the kernel's ENODEV", err)
		}
	})

	t.Run("a resubmit that fails after a frame arrived", func(t *testing.T) {
		const w, h = 320, 2
		whole := w * h * BytesPerPixelYUY2
		f := &fakeStream{t: t, packet: 3072}
		f.install()
		f.sendFrame(false, 0x7, whole, 1024)
		f.sendFrame(true, 0x8, 1024, 1024)
		s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		f.submitErrAfter = f.submits + 1
		if _, err := s.Frame(streamCtx(t)); !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("Frame = %v, want the kernel's ENODEV from the resubmit", err)
		}
	})

	// ⛔ A COMPLETION THIS STREAM DOES NOT OWN IS NOT A KERNEL REFUSAL, so it is
	// not here. It used to be — fatal — and that ended the first on-device run;
	// see TestAForeignCompletionIsSkippedAndCounted for what it does now and
	// why.
}

func TestAPacketThisProcessCannotReadIsSkippedRatherThanBlamedOnTheCamera(t *testing.T) {
	// A packet whose header does not parse is not pixels and not a boundary.
	// Counting it as "bad" would blame the camera for our own parse; the frame
	// it belonged to comes out short and is dropped, which is visible in the
	// counters.
	const w, h = 320, 2
	whole := w * h * BytesPerPixelYUY2
	f := &fakeStream{t: t, packet: 3072}
	f.install()
	f.sendFrame(false, 0x11, whole-1024, 1024)
	f.packets = append(f.packets, []byte{0xFF, 0x00, 0x01}) // a 255-byte header in 3 bytes
	f.sendFrame(true, 0x22, whole, 1024)
	f.sendFrame(false, 0x33, 1024, 1024)

	s, err := fakeHandle().Stream(UVCNegotiation{Kind: FormatYUY2}, isoEndpoint(3072), w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Frame(streamCtx(t)); err != nil {
		t.Fatalf("Frame: %v", err)
	}
	st := s.Stats()
	if st.Frames != 1 {
		t.Errorf("Stats reports %d frames, want the one whole frame (%s)", st.Frames, st)
	}
	if st.Dropped != 1 {
		t.Errorf("Stats reports %d dropped, want the one frame the unreadable packet "+
			"ruined (%s)", st.Dropped, st)
	}
	if st.Bad != 0 {
		t.Errorf("Stats blames the camera for %d bad payloads; the camera marked none, "+
			"this process failed to read one (%s)", st.Bad, st)
	}
}

func TestANewURBSlotDescribesWhatTheKernelExpects(t *testing.T) {
	// The fields the kernel reads, checked where they are written rather than
	// through a round trip: a wrong Type is the difference between isochronous
	// and interrupt, and bmAttributes 1 means ISOCHRONOUS while URB type 1
	// means INTERRUPT.
	ep := isoEndpoint(3072)
	slot := newURBSlot(ep, ep.ISOPacketSize())
	u := (*usbdevfsURB)(unsafe.Pointer(&slot.raw[0]))
	if int(u.Type) != urbTypeFor(USBTransferIsochronous) {
		t.Errorf("URB type %d, want %d: bmAttributes 1 is isochronous but URB type 1 is "+
			"INTERRUPT, and the two tables are not the same table",
			u.Type, urbTypeFor(USBTransferIsochronous))
	}
	if int(u.Endpoint) != ep.Address {
		t.Errorf("URB endpoint %#02x, want %#02x", u.Endpoint, ep.Address)
	}
	if int(u.NumberOfPackets) != StreamPacketsPerURB {
		t.Errorf("URB carries %d packets, want %d", u.NumberOfPackets, StreamPacketsPerURB)
	}
	if int(u.BufferLength) != ep.ISOPacketSize()*StreamPacketsPerURB {
		t.Errorf("URB buffer is %d bytes, want %d packets of %d",
			u.BufferLength, StreamPacketsPerURB, ep.ISOPacketSize())
	}
	if slot.addr != uintptr(unsafe.Pointer(&slot.raw[0])) {
		t.Error("the slot's remembered address is not its own memory, so no completion " +
			"will ever match it")
	}
	for i := 0; i < StreamPacketsPerURB; i++ {
		d := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&slot.raw[urbSize+isoPacketSize*i]))
		if int(d.Length) != ep.ISOPacketSize() {
			t.Fatalf("packet %d asks for %d bytes, want %d", i, d.Length, ep.ISOPacketSize())
		}
	}
}
