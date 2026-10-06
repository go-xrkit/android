// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"
)

// ErrNotPresentable reports a display that cannot carry a Presentation.
//
// The built-in panel is the usual one: it is where the launcher and every other
// application live, and the platform will not let an ordinary app put a
// Presentation over them. [Display.Presentation] says in advance, which is why
// this is answered here without asking the host.
var ErrNotPresentable = errors.New("android: display will not take a presentation")

// ErrScreenClosed is reported by [Screen.Next] and [Screen.Present] after
// [Screen.Close].
var ErrScreenClosed = errors.New("android: screen is closed")

// Limits on a screen's frames.
const (
	// MinScreenEdge is the smallest frame a screen will carry. Smaller than
	// this is a mistake rather than a frugal choice.
	MinScreenEdge = 16

	// MaxScreenPixels bounds one frame, and with it the shared buffer.
	//
	// The WHOLE buffer has to fit an int32, which is what Android's
	// SharedMemory.create takes, and it is MaxQueueDepth slots of four bytes a
	// pixel: 32 megapixels is the largest frame that leaves the product below
	// two gigabytes at every accepted queue depth. It is eight 4K panels' worth,
	// so it constrains nothing anybody would ask for and still refuses a width
	// times a height that overflowed somewhere upstream.
	MaxScreenPixels = 32 << 20
)

// ScreenOptions says what frames an application will put on a screen.
//
// The zero value is usable and means: frames the size of the display, with
// [DefaultQueueDepth] slots.
type ScreenOptions struct {
	// Width and Height are the size of the frames the application will draw,
	// in pixels. Zero means the display's own size.
	//
	// They need not match the display. The host scales the frame to fill it,
	// so a ribbon composited once at one resolution reaches panels of another —
	// and a screen rendered deliberately small is how a weak device keeps its
	// frame rate.
	Width, Height int

	// QueueDepth is how many frame slots the shared buffer holds. Zero means
	// [DefaultQueueDepth], and it is bounded by [MinQueueDepth] and
	// [MaxQueueDepth] exactly as a capture's is: the floor leaves room for the
	// frame being drawn, the one in flight and the one being blitted.
	QueueDepth int
}

// resolve fills the zero fields in against the display and validates the
// result, returning the options the host is actually asked for.
func (o ScreenOptions) resolve(d Display) (ScreenOptions, error) {
	if o.Width == 0 {
		o.Width = d.Width
	}
	if o.Height == 0 {
		o.Height = d.Height
	}
	if o.QueueDepth == 0 {
		o.QueueDepth = DefaultQueueDepth
	}
	if err := o.Validate(); err != nil {
		return ScreenOptions{}, err
	}
	return o, nil
}

// Validate reports whether the options describe frames a host could carry,
// wrapping [ErrInvalidOption]. It does not apply defaults: a zero Width or
// QueueDepth is refused here and filled in by [ShowOn] before this is reached.
func (o ScreenOptions) Validate() error {
	switch {
	case o.Width < MinScreenEdge || o.Height < MinScreenEdge:
		return fmt.Errorf("%w: a %dx%d frame is too small to be a screen; both edges "+
			"must be at least %d", ErrInvalidOption, o.Width, o.Height, MinScreenEdge)
	case o.Width > MaxDimension || o.Height > MaxDimension:
		return fmt.Errorf("%w: %dx%d is beyond the %d-pixel limit",
			ErrInvalidOption, o.Width, o.Height, MaxDimension)
	case int64(o.Width)*int64(o.Height) > MaxScreenPixels:
		return fmt.Errorf("%w: %dx%d is %d pixels, beyond the %d this package will carry",
			ErrInvalidOption, o.Width, o.Height, int64(o.Width)*int64(o.Height), MaxScreenPixels)
	case o.QueueDepth < MinQueueDepth || o.QueueDepth > MaxQueueDepth:
		return fmt.Errorf("%w: %d frame slots, want %d..%d",
			ErrInvalidOption, o.QueueDepth, MinQueueDepth, MaxQueueDepth)
	}
	return nil
}

// String renders the options for logs.
func (o ScreenOptions) String() string {
	return fmt.Sprintf("%dx%d, %d slots", o.Width, o.Height, o.QueueDepth)
}

// A Canvas is one frame slot of a screen's shared buffer, lent to the
// application to draw into.
//
// The bytes are BORROWED and belong to the host: they are valid from the
// [Screen.Next] that returned them until the [Screen.Present] that hands them
// back, and touching them afterwards races the blit. A Canvas is not safe for
// concurrent use.
//
// Pix is RGBA, eight bits a channel — the same memory layout as [image.RGBA],
// which is what [Canvas.RGBA] hands back so the whole of image/draw can paint a
// frame.
type Canvas struct {
	// Pix is the frame's pixels, Stride bytes a row, Height rows.
	Pix []byte
	// Stride is the distance in bytes between two rows. It is at least
	// Width*4 and is usually more: the host announces the alignment its
	// surface wants, and writing Width*4 instead would shear the image.
	Stride int
	// Width and Height are the frame's size in pixels.
	Width, Height int

	// slot is which slot of the shared buffer this is, and part of the token
	// Present checks: the number reaches the host as an offset into shared
	// memory, so a Canvas this screen did not lend is refused rather than
	// believed.
	slot int
	// token numbers the lending. A Canvas presented twice would free a slot
	// that is in use and let the application draw over a frame being blitted,
	// which is a tear nobody could trace back here.
	token int64
}

// RGBA returns the canvas as an [image.RGBA] sharing the same memory, so a
// frame can be painted with image/draw, a font renderer, or anything else that
// takes a draw.Image.
//
// It allocates only the header: the pixels are the host's, and drawing through
// this writes straight into the shared buffer.
func (c Canvas) RGBA() *image.RGBA {
	return &image.RGBA{
		Pix:    c.Pix,
		Stride: c.Stride,
		Rect:   image.Rect(0, 0, c.Width, c.Height),
	}
}

// Row returns the bytes of one row, for a compositor that writes spans rather
// than going through image/draw. It reports nil for a row outside the frame.
//
// The slice stops at Width*4 and cannot be appended past the row: the padding a
// stride leaves is the host's, and a span that ran into it would corrupt the
// next row rather than fail.
func (c Canvas) Row(y int) []byte {
	if y < 0 || y >= c.Height || c.Stride < c.Width*4 {
		return nil
	}
	off := y * c.Stride
	return c.Pix[off : off+c.Width*4 : off+c.Width*4]
}

// String renders the canvas for logs.
func (c Canvas) String() string {
	return fmt.Sprintf("canvas slot %d, %dx%d, %d-byte rows", c.slot, c.Width, c.Height, c.Stride)
}

// ScreenStats counts what a screen has done.
type ScreenStats struct {
	// Presented is how many frames the application handed over.
	Presented int64
	// Acknowledged is how many of those the host reported on the screen. The
	// difference is what is in flight.
	Acknowledged int64
	// Waited is how many times [Screen.Next] had to block because every slot
	// was in flight. A large number against a small QueueDepth means the
	// application is drawing faster than the headset can take it.
	Waited int64
}

// String renders the counters for logs.
func (s ScreenStats) String() string {
	return fmt.Sprintf("%d presented, %d acknowledged, %d waits",
		s.Presented, s.Acknowledged, s.Waited)
}

// afterTakingASlot is a seam: it runs inside [Screen.Next], between the slot
// arriving and the lock that commits it, and does nothing in production.
//
// ⛔ IT IS A SEAM BECAUSE THE WINDOW IS REAL AND THE TIMING IS NOT MINE. A
// Close landing exactly there must not hand out a canvas over memory that is
// about to be unmapped — and the only other way to test that is a goroutine
// racing a sleep, which passes on a fast machine whatever the code does. The
// same seam shape as mmap and getwd elsewhere in this package.
var afterTakingASlot = func() {}

// screenFeed is what the transport hands back for one screen: the mapped
// buffer, its layout, how to present a slot, and where the host's
// acknowledgements and failures arrive.
//
// It is a struct of functions and channels rather than an interface so the
// suite can drive every branch of [Screen] on a platform with no host to dial —
// which is most of this package's behaviour, and all of the part that could
// hand the host an offset into shared memory.
type screenFeed struct {
	buf      []byte
	stride   int
	slotSize int64
	slots    int

	width, height int
	displayID     int

	present func(PresentMessage) error
	acks    <-chan PresentedMessage
	fail    <-chan error
	close   func() error
}

// A Screen is a Presentation on a display that ALREADY EXISTS — the glasses —
// carrying pixels this process painted.
//
// # Why this is a different thing from a Wall
//
// A [Wall] creates displays and reads what Android renders on them. A Screen
// does neither: the display is the headset, the platform made it when the cable
// was plugged in, and the pixels travel the other way. The two share a host, a
// socket and a wire, and nothing else — a Wall is an INPUT, a Screen is the
// OUTPUT, and an XR application wants both at once.
//
// # Why an application may have one at all
//
// An ordinary APK may not start an activity on a display it does not own: that
// is refused with a SecurityException, and the flag that would lift it needs a
// signature permission. A Presentation is not an activity start, so it is not
// what that check refuses, and it is the only route there is. Whether a given
// display will take one is the platform's own answer, carried in
// [Display.Presentation] — measured on a Pixel 11 Pro Fold with VITURE Beast
// glasses on the USB-C port:
//
//	APPEARED id 12 "VITURE Beast" 1920x1080 @110dpi 60.0Hz flags 0x8088 presentation true
//
// No permission, no consent dialog and no MediaProjection are involved, exactly
// as for a Wall.
//
// # The back-pressure is the point
//
// The host lends a shared buffer of [ScreenOptions.QueueDepth] slots.
// [Screen.Next] hands out a free one, the application draws into it,
// [Screen.Present] gives it to the host, and the slot comes back only once the
// host says the frame reached the view. Nothing else stops the application
// overwriting a slot mid-blit: the pixels go the other way here, so there is no
// stream of host frames to pace against, and a tear in a headset is not a
// cosmetic defect.
//
// A Screen is safe for concurrent use. A [Canvas] is not: it is a window onto
// shared memory, and two goroutines drawing into one race.
type Screen struct {
	feed screenFeed
	opts ScreenOptions

	// free carries the slots nobody is drawing into or presenting. It is
	// buffered to exactly the slot count, so returning a slot never blocks the
	// goroutine that reads the host.
	free chan int
	// stop ends the ack loop, and closing it is what wakes a blocked Next.
	stop chan struct{}
	once sync.Once

	mu       sync.Mutex
	inflight map[int]int64 // slot -> the token it was lent under
	token    int64
	seq      int64
	stats    ScreenStats
	err      error
	closed   bool
}

// ShowOn puts a Presentation on an existing display and returns the screen the
// application draws into.
//
// It refuses a display the platform will not take a Presentation on with
// [ErrNotPresentable], BEFORE dialling anything: the answer is already in the
// [Display] the host handed over, and a round trip would come back saying less.
// It wraps [ErrInvalidOption] for options that cannot describe a frame, and
// reports [ErrUnsupported] where there is no host.
//
// Close the screen when done; it holds a socket, a mapping and a window.
func ShowOn(ctx context.Context, d Display, o ScreenOptions) (*Screen, error) {
	if err := checkPresentable(d); err != nil {
		return nil, err
	}
	opts, err := o.resolve(d)
	if err != nil {
		return nil, err
	}
	f, err := openScreen(ctx, d, opts)
	if err != nil {
		return nil, err
	}
	return newScreen(f, opts), nil
}

// presentable keeps the displays an ordinary application may put a
// Presentation on: the ones the platform flagged, minus the built-in panel.
//
// The platform sets FLAG_PRESENTATION on the default display of some devices,
// so the flag alone is not the answer — and an application that believed it
// would ask for a window over the launcher and be refused by a check it had
// already passed.
func presentable(ds []Display) []Display {
	out := make([]Display, 0, len(ds))
	for _, d := range ds {
		if d.Presentation() && !d.Default() {
			out = append(out, d)
		}
	}
	return out
}

// checkPresentable reports whether a display can carry a Presentation at all.
func checkPresentable(d Display) error {
	if d.Presentation() {
		return nil
	}
	if d.Default() {
		return fmt.Errorf("%w: %s is the built-in panel, where the launcher and every "+
			"other application live; an ordinary app may not put a presentation over "+
			"them. Attach a headset over USB-C and present on that", ErrNotPresentable, d)
	}
	return fmt.Errorf("%w: %s has no FLAG_PRESENTATION, so the platform will not take a "+
		"presentation on it", ErrNotPresentable, d)
}

// newScreen wires a screen onto a feed and starts reading the host.
func newScreen(f screenFeed, opts ScreenOptions) *Screen {
	s := &Screen{
		feed:     f,
		opts:     opts,
		free:     make(chan int, f.slots),
		stop:     make(chan struct{}),
		inflight: make(map[int]int64, f.slots),
	}
	for i := 0; i < f.slots; i++ {
		s.free <- i
	}
	go s.collect()
	return s
}

// collect returns acknowledged slots to the free list and records a host that
// went away. It is the only goroutine this package starts for a screen.
func (s *Screen) collect() {
	for {
		select {
		case <-s.stop:
			return
		case err := <-s.feed.fail:
			s.fail(err)
			return
		case a, ok := <-s.feed.acks:
			if !ok {
				s.fail(fmt.Errorf("%w: the host stopped acknowledging frames", ErrClosed))
				return
			}
			s.acknowledge(a)
		}
	}
}

// acknowledge frees the slot one acknowledgement names and counts it.
func (s *Screen) acknowledge(a PresentedMessage) {
	if s.release(a.Slot) {
		s.mu.Lock()
		s.stats.Acknowledged++
		s.mu.Unlock()
	}
}

// release puts a slot back on the free list, reporting whether it did.
//
// A slot the host names twice, or names without having been given it, is
// IGNORED rather than freed: putting it back would let the application draw
// into a slot another frame still occupies, and the host is the side this
// process cannot verify. The send cannot block — the channel is sized to the
// slot count, and a slot is either in flight or on the free list, never both.
func (s *Screen) release(slot int) bool {
	s.mu.Lock()
	if _, held := s.inflight[slot]; !held || s.closed {
		s.mu.Unlock()
		return false
	}
	delete(s.inflight, slot)
	s.mu.Unlock()
	s.free <- slot
	return true
}

// fail records the first failure and wakes anything waiting on a slot.
func (s *Screen) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.once.Do(func() { close(s.stop) })
}

// Next returns a canvas to draw the next frame into, waiting for a slot if
// every one is still in flight.
//
// The pixels it hands back are BORROWED from the shared buffer and are whatever
// the frame that last used that slot left there, not blank: a compositor that
// paints the whole frame wants that, and one that does not must clear it.
//
// It reports the context's error if the wait is abandoned, [ErrScreenClosed]
// after [Screen.Close], and the host's failure if the host went away.
func (s *Screen) Next(ctx context.Context) (Canvas, error) {
	if err := s.state(); err != nil {
		return Canvas{}, err
	}
	var slot int
	select {
	case slot = <-s.free:
	default:
		// Every slot is in flight. Counting the wait before taking it is what
		// makes the counter mean "the application was faster than the headset"
		// rather than "a slot was taken".
		s.mu.Lock()
		s.stats.Waited++
		s.mu.Unlock()
		select {
		case slot = <-s.free:
		case <-ctx.Done():
			return Canvas{}, ctx.Err()
		case <-s.stop:
			return Canvas{}, s.state()
		}
	}

	afterTakingASlot()
	s.mu.Lock()
	if s.closed {
		// Close ran between the check at the top and the slot arriving. Lending
		// the canvas anyway would hand out a window onto memory Close is about
		// to unmap, and the fault is a SIGSEGV rather than an error.
		s.mu.Unlock()
		return Canvas{}, ErrScreenClosed
	}
	s.token++
	tok := s.token
	s.inflight[slot] = tok
	s.mu.Unlock()

	off := int64(slot) * s.feed.slotSize
	n := int64(s.feed.stride) * int64(s.feed.height)
	return Canvas{
		Pix:    s.feed.buf[off : off+n : off+n],
		Stride: s.feed.stride,
		Width:  s.feed.width,
		Height: s.feed.height,
		slot:   slot,
		token:  tok,
	}, nil
}

// Present hands a drawn canvas to the host to put on the screen. The canvas is
// invalid afterwards whether this succeeded or not.
//
// It refuses a canvas this screen did not lend, and one presented twice, with
// [ErrInvalidOption]: the slot number reaches the host as an offset into shared
// memory, and freeing a slot that is in use would let the next frame be drawn
// over one being blitted.
func (s *Screen) Present(ctx context.Context, c Canvas) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrScreenClosed
	case s.err != nil:
		err := s.err
		s.mu.Unlock()
		return err
	}
	if tok, held := s.inflight[c.slot]; !held || tok != c.token {
		s.mu.Unlock()
		return fmt.Errorf("%w: this canvas is not one this screen is lending; a canvas is "+
			"valid from the Next that returned it until the Present that hands it back",
			ErrInvalidOption)
	}
	// ⛔ THE TOKEN DIES HERE, NOT AT THE ACKNOWLEDGEMENT. The slot stays in
	// flight until the host answers, so matching on it alone would accept the
	// SAME canvas twice in the window before the answer arrives — which is most
	// of the time, and is exactly when a second Present would be made by
	// mistake. Tokens start at 1, so zero matches no canvas.
	s.inflight[c.slot] = 0
	s.seq++
	m := PresentMessage{
		Seq:    s.seq,
		Slot:   c.slot,
		Width:  s.feed.width,
		Height: s.feed.height,
		Stride: s.feed.stride,
	}
	s.stats.Presented++
	s.mu.Unlock()

	if err := s.feed.present(m); err != nil {
		// The host never got the frame, so no acknowledgement is coming and the
		// slot would be lost. Giving it back here is what keeps a screen usable
		// across a failed write — and it is release rather than acknowledge,
		// because nothing reached the screen and Acknowledged must not say it
		// did.
		s.release(m.Slot)
		return fmt.Errorf("android: presenting frame %d: %w", m.Seq, err)
	}
	return nil
}

// state reports why the screen cannot be used, or nil.
func (s *Screen) state() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return ErrScreenClosed
	case s.err != nil:
		return s.err
	}
	return nil
}

// Err reports the failure that ended the screen, or nil while it is live.
func (s *Screen) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// DisplayID is the android.view.Display id this screen presents on.
func (s *Screen) DisplayID() int { return s.feed.displayID }

// Size is the frame size in pixels, which is what [Screen.Next] hands back.
// The display may be a different size; the host scales to fill it.
func (s *Screen) Size() (int, int) { return s.feed.width, s.feed.height }

// Stride is the distance in bytes between two rows of a frame.
func (s *Screen) Stride() int { return s.feed.stride }

// Options are the options this screen was opened with, with the zero fields
// filled in.
func (s *Screen) Options() ScreenOptions { return s.opts }

// Stats reports what the screen has done.
func (s *Screen) Stats() ScreenStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// String renders the screen for logs.
func (s *Screen) String() string {
	return fmt.Sprintf("screen on display %d, %dx%d, %d-byte rows, %s",
		s.feed.displayID, s.feed.width, s.feed.height, s.feed.stride, s.Stats())
}

// Close takes the Presentation down, unmaps the buffer and releases the socket.
// It is idempotent.
//
// Any [Canvas] still out is invalid afterwards: the memory under it is gone.
func (s *Screen) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.stop) })
	return s.feed.close()
}
