// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// StreamURBs is how many isochronous requests are kept in flight.
//
// ⛔ ONE IS NOT ENOUGH, AND THAT IS NOT A TUNING CHOICE. An isochronous
// endpoint is scheduled every microframe whether anybody is listening: between
// reaping one request and submitting the next, the camera's packets go nowhere.
//
// ⛔⛔ AND SIXTEEN IS MEASURED, AGAINST FOUR WHICH HALVED THE FRAME RATE. This
// was 4 for five releases on the strength of an experiment with a CHEAP consumer
// where deepening the ring changed nothing — an experiment that was right about
// its own conditions and wrong as a general conclusion. With a consumer costing
// more than the ring covers, the ring is dry while it works and the packets that
// arrive then are gone. Measured on a Pixel 11 Pro Fold with cmd/xrhead, which
// spends 4.8 ms a frame decoding and tracking:
//
//	requests   fps     dropped
//	       4   10.5    79
//	       8   21.5    14
//	      16   23.8     0      <- this
//	      32    0       the camera stops sending
//
// 23.8 fps against the 25 the camera advertises, and nothing dropped. Thirty-two
// fails the same way thirty-two PACKETS a request does; both axes have a ceiling
// of their own and neither is their product, since 4x32 and 16x8 are both 128
// packets and only one of them works.
//
// ⛔ WHAT THIS CORRECTS IS A PUBLISHED CONCLUSION. This repository said "~10 fps
// is the camera's ceiling, flat across a fourfold change in pixel count", and
// the byte budget behind it said the camera filled 34.7 % of each interval. The
// budget closed; the causal reading was backwards. The camera under-filled
// because NOTHING WAS ASKING — at sixteen requests the same camera fills 77 %.
const StreamURBs = 16

// MaxPendingFrames is how many assembled frames may wait for the caller.
//
// ⛔ IT USED TO BE [StreamURBs], WHICH WAS A COINCIDENCE RATHER THAN A REASON.
// This ceiling answers "how stale may a frame be before it is worthless", which
// is a question about the CONSUMER — head tracking wants the freshest picture —
// and has nothing to do with how many requests are in flight. Borrowing the
// ring's depth meant that raising the ring silently made the shedding branch
// unreachable, which the coverage gate caught the moment the ring went to
// sixteen.
//
// Four, because a consumer four frames behind at 24 fps is already 170 ms late
// and a fifth frame would not help it.
const MaxPendingFrames = 4

// A UVCStream is a running camera.
//
// ⛔ IT IS NOT A Stream. The capture and wall paths deliver frames the HOST
// wrote into shared memory and hand them over borrowed; this one is built from
// isochronous packets this process reaped itself, assembled, and decoded. They
// share no machinery and the names are deliberately different.
//
// A UVCStream is NOT safe for concurrent use: the URB ring, the assembler and
// the decode buffer are all single-reader state, and there is no reason for two
// goroutines to read one camera.
type UVCStream struct {
	h    *USBHandle
	ep   USBEndpoint
	asm  *FrameAssembler
	neg  UVCNegotiation
	rgba *image.RGBA

	// ring holds every request this stream owns. A reaped URB is matched to its
	// slot by ADDRESS, compared as a uintptr — never converted back to a
	// pointer, which is what `go vet` calls a misuse and what nothing keeps
	// alive across the round trip.
	ring []*urbSlot

	// pending holds frames that completed and have not been handed out.
	//
	// ⛔⛔ ONE REQUEST CAN COMPLETE TWO FRAMES, AND RETURNING ONLY THE LAST
	// LOSES THE OTHER WHILE STILL COUNTING IT. A request carries
	// StreamPacketsPerURB packets; a frame small enough to fit in fewer than that
	// finishes twice inside one drain, and the first version of this code
	// overwrote `frame` — so Stats said two frames and the caller got one. It
	// does not happen at 640x480, where a frame is some two hundred packets,
	// which is exactly why it had to be found by a test rather than by looking.
	//
	// Frames are whole allocations once complete (see FrameAssembler.complete),
	// so holding them costs nothing but the memory and nothing dangles.
	pending [][]byte

	// frames carries assembled frames from the reader to the caller, and stop
	// ends the reader.
	//
	// ⛔ THE CHANNEL IS THE HANDOVER, AND ITS DEPTH IS [MaxPendingFrames]. A
	// reader that blocked on a full channel would be starving the ring again
	// through a different door, so it sheds the oldest instead and counts it.
	frames chan []byte
	stop   chan struct{}
	done   chan struct{}

	// late counts frames thrown away because the caller had not come back for
	// the ones before them.
	late int
	// readerErr is why the reader stopped, when it was not asked to.
	readerErr error
	// reading says the reader goroutine was launched; see started.
	reading bool
	// decoding is how long the decodes have taken in all, which is the only
	// honest measure of what a format costs on the processor that runs it.
	decoding time.Duration

	mu      sync.Mutex
	closed  bool
	packets int
	bytes   int
	empty   int
	foreign int
}

// urbSlot is one request and the memory it names.
type urbSlot struct {
	// raw is the URB and its packet descriptors in ONE allocation, because the
	// kernel reads them as one struct with a flexible array at the end.
	raw []byte
	// buf is where the packets land.
	buf []byte
	// addr is raw's address, kept so a reaped URB can be recognised without
	// turning the kernel's answer back into a pointer.
	addr uintptr
	// out says whether this slot is currently submitted.
	out bool
}

// Stream starts reading frames from a camera that has already been negotiated.
//
// ⛔ THE ORDER MATTERS AND NOTHING ENFORCES IT BUT THIS SENTENCE: the interface
// must be claimed, its alternate selected and the format committed before this
// is called. A stream started before the commit reaps packets of nothing, which
// looks exactly like a camera with the lens covered.
func (h *USBHandle) Stream(neg UVCNegotiation, ep USBEndpoint, w, h2 int) (*UVCStream, error) {
	// ⛔ THE FORMAT COMES FROM THE NEGOTIATION, which is the only place that
	// knows it: assembling a compressed frame by length and an uncompressed one
	// by markers are both wrong, and nothing else here can tell which is which.
	asm, err := NewFrameAssemblerFor(neg.Kind, w, h2)
	if err != nil {
		return nil, err
	}
	packet := ep.ISOPacketSize()
	if packet <= 0 {
		return nil, fmt.Errorf("%w: endpoint %#02x carries %d bytes an interval",
			ErrInvalidOption, ep.Address, packet)
	}
	s := &UVCStream{
		h: h, ep: ep, asm: asm, neg: neg,
		frames: make(chan []byte, MaxPendingFrames),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	for i := 0; i < StreamURBs; i++ {
		slot := newURBSlot(ep, packet)
		s.ring = append(s.ring, slot)
		// ⛔ PINNED BEFORE IT IS SUBMITTED, because after the submit the kernel
		// is already entitled to write into it. See USBHandle.pinned.
		h.pin(slot)
		if err := s.submit(slot); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	// ⛔ THE READER STARTS ONLY ONCE EVERY REQUEST IS QUEUED. Starting it inside
	// the loop would have it reaping while this one is still submitting, and the
	// ring is single-owner state — the reader's, from here on.
	s.mu.Lock()
	s.reading = true
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		s.read()
	}()
	return s, nil
}

// newURBSlot allocates one request.
func newURBSlot(ep USBEndpoint, packet int) *urbSlot {
	n := StreamPacketsPerURB
	s := &urbSlot{
		buf: make([]byte, packet*n),
		raw: make([]byte, urbSize+isoPacketSize*n),
	}
	s.addr = uintptr(unsafe.Pointer(&s.raw[0]))
	u := (*usbdevfsURB)(unsafe.Pointer(&s.raw[0]))
	u.Type = uint8(urbTypeFor(ep.Type()))
	u.Endpoint = uint8(ep.Address)
	u.Buffer = uintptr(unsafe.Pointer(&s.buf[0]))
	u.BufferLength = int32(len(s.buf))
	u.NumberOfPackets = int32(n)
	for i := 0; i < n; i++ {
		p := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&s.raw[urbSize+isoPacketSize*i]))
		p.Length = uint32(packet)
	}
	return s
}

// The three operations a stream performs on the kernel, as seams.
//
// ⛔⛔ THEY ARE AT THIS LEVEL, NOT AT THE IOCTL, AND FOR THE SAME REASON AS
// [controlTransfer]. A fake camera installed under the ioctl is handed
// unsafe.Pointer(&raw[0]) and nothing else: to put packets where the stream will
// read them it has to take the Buffer field — a uintptr — and turn it back into
// a pointer, which is the unsafe misuse `go vet` refuses and which already
// crashed this suite once. One step higher a fake is handed the *urbSlot, whose
// buf is an ordinary Go slice, and writes into it with no unsafe at all.
//
// The ioctl numbers themselves are still asserted against usbdevice_fs.h in
// usbfs_test.go, and [USBHandle.SubmitISO] still exercises the real path.
var (
	submitURB = func(fd int, slot *urbSlot) error {
		return ioctl(fd, usbdevfsSubmitURB, unsafe.Pointer(&slot.raw[0]))
	}
	reapURB = func(fd int) (uintptr, error) {
		var p uintptr
		err := ioctl(fd, usbdevfsReapURBNDelay, unsafe.Pointer(&p))
		return p, err
	}
	discardURB = func(fd int, slot *urbSlot) error {
		return ioctl(fd, usbdevfsDiscardURB, unsafe.Pointer(&slot.raw[0]))
	}
)

// submit queues one slot. The caller holds no lock; the ring is single-reader.
func (s *UVCStream) submit(slot *urbSlot) error {
	if err := submitURB(s.h.fd, slot); err != nil {
		return fmt.Errorf("android: submitting an isochronous request to %#02x of %s: %w",
			s.ep.Address, s.h.name, err)
	}
	slot.out = true
	runtime.KeepAlive(slot.buf)
	runtime.KeepAlive(slot.raw)
	return nil
}

// Frame reads until one whole frame has been assembled, and returns it decoded.
//
// The image is REUSED between calls, so a steady stream allocates nothing after
// the first frame. A caller that keeps one copies it.
//
// It reports the context's error if the wait is abandoned, and [ErrClosed]
// after [UVCStream.Close].
func (s *UVCStream) Frame(ctx context.Context) (*image.RGBA, error) {
	for {
		// ⛔ A CONTEXT ALREADY CANCELLED IS REPORTED BEFORE ANYTHING ELSE IS
		// LOOKED AT. The select below has two cases that can both be ready at
		// once — a closed handover and a cancelled caller — and select picks
		// among ready cases at RANDOM, so without this the same call answered
		// ErrClosed or context.Canceled depending on the coin. The caller's own
		// cancellation is the more specific answer, and a test asserts it.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var frame []byte
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case f, ok := <-s.frames:
			if !ok {
				// The reader stopped. Its reason, if it had one, is the
				// stream's; ErrClosed is what an ordinary Close looks like.
				s.mu.Lock()
				err := s.readerErr
				s.mu.Unlock()
				return nil, cmp.Or(err, error(ErrClosed))
			}
			frame = f
		}
		// ⛔⛔ THE DECODE HAPPENS HERE, ON THE CALLER'S GOROUTINE, AND THAT IS
		// THE WHOLE POINT OF THE READER. It used to happen between two reaps,
		// so its cost was time with nothing submitted — and an isochronous
		// endpoint with no request outstanding is a schedule with nowhere to put
		// the bytes. Measured on a Pixel 11 Pro Fold: MJPEG decodes in 19 ms
		// against a ring covering 16, and delivered 3.2 of its advertised
		// 30 fps. Deepening the ring could not fix that — thirty-two requests
		// make this camera stop sending — so the coupling had to go instead.
		//
		// It is still timed, because the choice between YUY2 and MJPEG turns on
		// what it costs on the processor that has to run it, and a benchmark on
		// a workstation is an optimistic bound rather than a measurement.
		t0 := time.Now()
		img, err := s.decode(frame)
		s.mu.Lock()
		s.decoding += time.Since(t0)
		s.mu.Unlock()
		if err != nil {
			// A frame that arrived whole and will not decode is the camera's
			// fault, not the end of the stream: it is counted and the next
			// frame is awaited. Ending here would turn one bad JPEG into a
			// dead camera.
			continue
		}
		s.rgba = img
		return img, nil
	}
}

// read is the reader: it reaps, assembles and resubmits, and does nothing else.
//
// ⛔⛔ IT EXISTS SO THAT NOTHING THE CALLER DOES CAN STARVE THE RING. Every
// millisecond spent here is a millisecond a request is outstanding; every
// millisecond spent decoding, tracking or drawing is the caller's and happens
// elsewhere. That separation is what the ring depth was standing in for — 4 to
// 16 bought a doubling, and 32 is refused by the camera, so there was no more
// room on that axis.
//
// It owns the ring and the pending queue outright: no other goroutine touches
// them, which is why they need no lock while the counters do.
func (s *UVCStream) read() {
	defer close(s.frames)
	for {
		select {
		case <-s.stop:
			return
		default:
		}

		slot, err := s.reap()
		if err != nil {
			s.mu.Lock()
			s.readerErr = err
			s.mu.Unlock()
			return
		}
		if slot == nil {
			// Nothing has completed. ⚠ THE SLEEP IS THE SCHEDULE, not a guess:
			// at StreamPacketsPerURB packets a request, each covers four
			// milliseconds, so there is nothing to collect sooner and a busy
			// loop would burn a core to learn that.
			select {
			case <-s.stop:
				return
			case <-time.After(time.Millisecond):
			}
			continue
		}

		// ⛔ DRAINED AND RESUBMITTED BEFORE ANYTHING IS HANDED OVER. The slot
		// goes back to the kernel immediately; the frames it completed are then
		// offered to the caller, and an offer that nobody is waiting for must
		// not hold this loop.
		s.drain(slot)
		if err := s.submit(slot); err != nil {
			s.mu.Lock()
			s.readerErr = err
			s.mu.Unlock()
			return
		}
		// ⛔ THIS LOOP HAS NO CANCELLATION CASE AND MUST NOT GROW ONE. Every
		// select in it carries a default, so not one of them can block: each
		// turn either hands a frame over or makes room for one, so the loop
		// ends within twice the length of the queue whatever the caller does.
		// Watching s.stop here would add a branch no test can reach — a race
		// between a ready send and a closed channel is decided by a coin — and
		// the gate would be measuring luck. The outer loop reads s.stop, which
		// is where a stream that has been closed belongs.
		for len(s.pending) > 0 {
			select {
			case s.frames <- s.pending[0]:
				s.pending = s.pending[1:]
			default:
				// ⛔⛔ THE CALLER IS BEHIND AND THE RING MAY NOT WAIT FOR IT.
				// Blocking here would be the starvation this reader exists to
				// remove, wearing a different hat.
				//
				// ⛔⛔ AND IT IS THE OLDEST *IN THE CHANNEL* THAT GOES, not the
				// one being offered. A channel is a queue: dropping the new
				// frame would leave the caller reading the four stalest, which
				// is the opposite of what head tracking wants and is exactly
				// what the first version of this reader did — the test caught
				// it by asserting that the frame handed over is not the oldest.
				// Making room at the front and pushing at the back keeps the
				// freshest, which is the policy the queue had before the
				// channel replaced it.
				select {
				case <-s.frames:
					s.mu.Lock()
					s.late++
					s.mu.Unlock()
				default:
					// Drained by the caller between the two selects: there is
					// room now, and the next turn of the loop will use it.
				}
			}
		}
	}
}

// decode turns one assembled frame into the reused image.
//
// ⛔ THE TWO FORMATS DIFFER IN WHETHER THIS CAN FAIL, and that is why there is
// no single call. A queued YUY2 frame is exactly a frame's worth by
// construction, so its length check is dead on this path and the coverage gate
// says so — see FrameAssembler.decodeInto. An MJPEG frame passed only a MARKER
// test to get here, so it genuinely may not be a picture.
func (s *UVCStream) decode(frame []byte) (*image.RGBA, error) {
	if s.asm.Format() == FormatMJPEG {
		return s.asm.Decode(s.rgba, frame)
	}
	return s.asm.decodeInto(s.rgba, frame), nil
}

// reap collects one completed request, or nil when none has.
func (s *UVCStream) reap() (*urbSlot, error) {
	p, err := reapURB(s.h.fd)
	if err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			return nil, nil // nothing finished yet
		}
		return nil, fmt.Errorf("android: reaping a request from %s: %w", s.h.name, err)
	}
	// ⛔ MATCHED BY ADDRESS, COMPARED AS A NUMBER. The kernel hands back the
	// pointer it was given; converting it back to a *usbdevfsURB would be the
	// unsafe misuse `go vet` refuses, and the slot already knows its own
	// memory.
	for _, slot := range s.ring {
		if slot.addr == p {
			slot.out = false
			return slot, nil
		}
	}
	// ⛔⛔ A COMPLETION THIS STREAM DOES NOT OWN IS SKIPPED, NOT FATAL. It was
	// fatal, and that killed the first on-device run on its FIRST call: the
	// probe's own SubmitISO had cancelled a test request without reaping it, and
	// a cancelled request still completes and still waits in the queue. That
	// particular litter is cleaned up now — see reapOwn — but the lesson is
	// structural: the completion queue belongs to the DESCRIPTOR, so anything
	// else reading the same device lands here, and one stray request must not
	// end a working camera.
	//
	// It is COUNTED, because the alternative to fatal is not silence: a stream
	// quietly eating foreign completions would look like a slow camera.
	s.mu.Lock()
	s.foreign++
	s.mu.Unlock()
	return nil, nil
}

// drain reads one completed request's packets into the assembler, queueing
// every frame that completed.
func (s *UVCStream) drain(slot *urbSlot) {
	off := 0
	packet := s.ep.ISOPacketSize()
	for i := 0; i < StreamPacketsPerURB; i++ {
		d := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&slot.raw[urbSize+isoPacketSize*i]))
		n := int(d.ActualLength)
		pkt := slot.buf[off : off+min(n, packet)]
		off += packet

		s.mu.Lock()
		s.packets++
		s.bytes += n
		if n == 0 {
			// ⚠ AN EMPTY PACKET IS NORMAL AND IS COUNTED. An isochronous
			// endpoint is scheduled whether the camera has anything to send, so
			// most packets are empty between frames; a stream reporting only
			// bytes would look stalled when it is merely idle.
			s.empty++
		}
		s.mu.Unlock()
		if n == 0 {
			continue
		}

		pl, err := ParsePayload(pkt)
		if err != nil {
			// A packet this process cannot read is not a frame boundary and
			// not pixels. Counting it as bad would blame the camera for our
			// parse; it is simply skipped, and the frame it belonged to will
			// come out short and be dropped.
			continue
		}
		if f, ok := s.asm.Feed(pl); ok {
			s.pending = append(s.pending, f)
		}
	}
	// ⚠ NOTHING IS SHED HERE ANY MORE. The ceiling moved to the handover in
	// read, where it belongs: the queue this builds lives for one request, and
	// the one that can grow behind a slow caller is the channel. Shedding in
	// both places would have counted the same loss twice.
}

// Stats reports what the stream has seen.
func (s *UVCStream) Stats() UVCStreamStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, d, b := s.asm.Stats()
	return UVCStreamStats{
		Packets: s.packets, Empty: s.empty, Bytes: s.bytes,
		Frames: f, Dropped: d, Bad: b, Late: s.late, Foreign: s.foreign,
		Decoding: s.decoding, BadJPEG: s.asm.BadJPEG(),
	}
}

// Negotiation is what the camera agreed to for this stream.
func (s *UVCStream) Negotiation() UVCNegotiation { return s.neg }

// Assembler is the frame assembler this stream feeds, for the counters that
// describe HOW frames ended rather than how many did.
func (s *UVCStream) Assembler() *FrameAssembler { return s.asm }

// Close cancels every outstanding request. It is idempotent.
//
// ⛔ IT DISCARDS WHAT IS STILL IN FLIGHT. A request left outstanding has the
// kernel writing into this process's memory on the camera's schedule, and the
// Go heap is entitled to move on without it.
func (s *UVCStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// ⛔⛔ THE READER IS STOPPED AND WAITED FOR BEFORE A SINGLE REQUEST IS
	// DISCARDED. It owns the ring; discarding underneath it would be two
	// goroutines on the same slots, and the one that lost would be the kernel's.
	// If Stream failed while submitting, there is no reader — the channels are
	// made before the loop and done is closed by nobody, so the wait is on a
	// nil-safe guard rather than on a goroutine that was never started.
	close(s.stop)
	if s.started() {
		<-s.done
	}
	for _, slot := range s.ring {
		if slot.out {
			_ = discardURB(s.h.fd, slot)
			slot.out = false
		}
	}
	return nil
}

// started reports whether the reader goroutine was ever launched.
//
// ⚠ Stream calls Close on a failed submit, BEFORE the reader exists, and
// waiting on done then would wait forever. This is that case, named.
func (s *UVCStream) started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reading
}
