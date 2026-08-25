// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// A Wall is a set of displays this application created and owns, and the limit
// on how many of them may exist at once.
//
// # Why an application may have these at all
//
// An ordinary APK may not manufacture desktops for OTHER people's applications:
// launching an activity onto a display the app created fails with a
// SecurityException even for the app's own activity, and the flag that would
// lift it, VIRTUAL_DISPLAY_FLAG_TRUSTED, needs a signature permission. That
// half of the refusal is real and is not going anywhere.
//
// It may, however, create a virtual display and show a [Presentation] on it —
// a Dialog attached to a Display, which is not an activity start and so does
// not meet the check that refuses the other thing. Measured on Android 15
// (API 35, arm64):
//
//	Q1 ANSWER: Presentation.show() SUCCEEDED on the app's OWN virtual display 3
//	Q2 frame 640x480 ... black=0/307200 topLeft=#FF00FF00 bottomRight=#FF0000FF
//
// No permission, no consent dialog and no MediaProjection are involved, which
// is why a Wall needs none of [RequestAuthorization]'s machinery. So an XR
// ribbon on Android carries the mirrored phone — see [CaptureDisplay] — AND as
// many screens of Android-rendered content as the application asks for.
//
// # THE LIMIT IS THE SAFETY, AND IT IS NOT A FORMALITY
//
// On the emulator this package was measured against, creating the 304th virtual
// display did not fail: it killed system_server with
// android.view.Surface$OutOfResourcesException: NO_MEMORY out of
// SurfaceControl.nativeCreate, and the device soft-rebooted. An unprivileged
// APK with no permissions at all did that to the machine it was running on.
//
// READ THIS BEFORE RAISING THE LIMIT: the ceiling was the SAME COUNT at 640x480
// and at 1920x1080 — nine times the pixels, same number, with the app's Java
// heap at 14 MiB of 192. It is SurfaceControl handles, not graphics memory. So
// MAKING THE SCREENS SMALLER DOES NOT HELP, which is exactly the mitigation
// somebody reaches for. 304 is one emulator's measurement and is deliberately
// not a constant here; a real device's number is unknown and could be lower.
//
// A Wall therefore refuses past its limit with [ErrTooManyDisplays] instead of
// asking the platform and hoping. [DefaultMaxDisplays] is 8, which is more than
// a ribbon uses; [MaxDisplays] is 32, which is the most this package will let a
// caller ask for however explicit they are about wanting it. The Java host
// enforces the same 32 independently, so a second process cannot get past it
// either.
//
// A Wall is safe for concurrent use.
type Wall struct {
	// opener is the transport seam, per-wall rather than package-global so the
	// suite can drive the bookkeeping on a platform that has no host to dial
	// without any shared mutable state for two tests to race over.
	opener func(context.Context, DisplaySpec) (feed, error)

	mu     sync.Mutex
	max    int
	open   map[*OwnedDisplay]struct{}
	closed bool
}

// Limits on how many displays one application may create.
const (
	// DefaultMaxDisplays is the limit [NewWall] applies when it is given 0. A
	// ribbon uses six to eight panels, so this is already generous.
	DefaultMaxDisplays = 8

	// MaxDisplays is the largest limit [NewWall] accepts, however explicitly a
	// caller asks. It is nowhere near the count that killed system_server, and
	// the Java host refuses past it too.
	MaxDisplays = 32

	// DefaultDensityDPI is the density given to an owned display whose spec
	// leaves DensityDPI at zero. It is Android's DENSITY_XHIGH, the density a
	// 1920x1080 panel of the size XR glasses present usually reports.
	DefaultDensityDPI = 320

	// MinDisplayEdge is the smallest edge an owned display may have. A display
	// smaller than this cannot lay a view hierarchy out at all.
	MinDisplayEdge = 16
)

// ErrTooManyDisplays is reported by [NewWall] when the requested limit exceeds
// [MaxDisplays], and by [Wall.Open] when the wall is already at its limit.
// Nothing is created in either case.
var ErrTooManyDisplays = errors.New("android: too many virtual displays")

// ErrWallClosed is reported by [Wall.Open] after [Wall.Close].
var ErrWallClosed = errors.New("android: wall is closed")

// NewWall returns a wall that will hold at most max displays at once. A max of
// 0 means [DefaultMaxDisplays]; a negative max, or one above [MaxDisplays], is
// refused with [ErrTooManyDisplays] and no wall is returned.
//
// NewWall talks to nothing and can fail for no other reason, so a caller may
// build one before knowing whether a host is there.
func NewWall(max int) (*Wall, error) {
	switch {
	case max == 0:
		max = DefaultMaxDisplays
	case max < 0:
		return nil, fmt.Errorf("%w: a limit of %d makes no sense", ErrTooManyDisplays, max)
	case max > MaxDisplays:
		return nil, fmt.Errorf("%w: asked for a limit of %d, and this package will not "+
			"go past %d — creating enough virtual displays kills system_server and "+
			"reboots the device, and the ceiling does not move if you make the "+
			"displays smaller", ErrTooManyDisplays, max, MaxDisplays)
	}
	return &Wall{
		opener: openOwnedDisplay,
		max:    max,
		open:   make(map[*OwnedDisplay]struct{}),
	}, nil
}

// Max is the limit this wall was built with.
func (w *Wall) Max() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.max
}

// Len is how many displays the wall holds open right now.
func (w *Wall) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.open)
}

// Open creates one display, shows a Presentation carrying spec.Content on it,
// and returns a feed of the pixels Android rendered there.
//
// It refuses with [ErrTooManyDisplays] when the wall is already full, and it
// refuses BEFORE asking the host, so a full wall costs no round trip and
// creates nothing. It refuses with [ErrWallClosed] after [Wall.Close], and
// wraps [ErrInvalidOption] for a spec that cannot describe a display.
func (w *Wall) Open(ctx context.Context, spec DisplaySpec) (*OwnedDisplay, error) {
	resolved, err := spec.resolve()
	if err != nil {
		return nil, err
	}

	// The limit is checked, and the slot reserved, before the host is asked.
	// Asking first and refusing afterwards would mean the dangerous thing had
	// already happened by the time this returned an error.
	w.mu.Lock()
	switch {
	case w.closed:
		w.mu.Unlock()
		return nil, ErrWallClosed
	case len(w.open) >= w.max:
		n := len(w.open)
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: this wall holds %d of at most %d; close one before "+
			"opening another", ErrTooManyDisplays, n, w.max)
	}
	d := &OwnedDisplay{wall: w, spec: resolved}
	w.open[d] = struct{}{}
	w.mu.Unlock()

	f, err := w.opener(ctx, resolved)
	if err != nil {
		w.forget(d)
		return nil, err
	}
	d.Stream, d.id, d.closer = f.stream, f.id, f.close
	return d, nil
}

// forget drops a display from the wall's bookkeeping. It is idempotent, which
// is what makes OwnedDisplay.Close idempotent.
func (w *Wall) forget(d *OwnedDisplay) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.open, d)
}

// Close releases every display the wall still holds and refuses further
// [Wall.Open] calls. It is idempotent, and reports the first failure a display
// reported while still closing the rest.
func (w *Wall) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	ds := make([]*OwnedDisplay, 0, len(w.open))
	for d := range w.open {
		ds = append(ds, d)
	}
	w.mu.Unlock()

	var first error
	for _, d := range ds {
		if err := d.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// feed is what the transport hands back for one owned display: the stream of
// pixels, the display id the platform assigned, and how to release it.
//
// The releaser travels WITH the stream rather than being taken from it, so the
// suite can supply a stream it did not build without the wall reaching into it.
// A test stub whose Close is the real Stream.Close crashes on the transport
// lane and passes on the stub lane, which is the kind of difference between
// lanes that hides in a green build.
type feed struct {
	stream *Stream
	id     int
	close  func() error
}

// DisplaySpec says what an owned display should be.
//
// The zero DisplaySpec is not usable: a display has no natural size to fall
// back on the way a capture does, so Width, Height and Content must be given.
type DisplaySpec struct {
	// Width and Height are the display's size in PIXELS. Both are required.
	//
	// Making them small does NOT buy more displays — the count that kills
	// system_server was the same at 640x480 and at 1920x1080 — so choose the
	// size the content wants to be read at.
	Width, Height int

	// DensityDPI is the display's density. Zero means [DefaultDensityDPI].
	// Android lays the view hierarchy out against this, so it decides how
	// large text and controls come out in the ribbon.
	DensityDPI int

	// Content is what Android renders on the display. It is required; see
	// [Content].
	Content Content

	// QueueDepth is how many frame slots the shared buffer holds, exactly as
	// in [Options.QueueDepth]. Zero means [DefaultQueueDepth].
	QueueDepth int
}

// resolve validates the spec and fills its zero fields in, returning the spec
// the host is actually asked for.
func (s DisplaySpec) resolve() (DisplaySpec, error) {
	if s.DensityDPI == 0 {
		s.DensityDPI = DefaultDensityDPI
	}
	if s.QueueDepth == 0 {
		s.QueueDepth = DefaultQueueDepth
	}
	if err := s.Validate(); err != nil {
		return DisplaySpec{}, err
	}
	return s, nil
}

// Validate reports whether the spec describes a display Android could make,
// wrapping [ErrInvalidOption]. It does not consult the system, and it does not
// apply defaults: a zero DensityDPI or QueueDepth is refused here and filled in
// by [Wall.Open] before this is reached.
func (s DisplaySpec) Validate() error {
	switch {
	case s.Width < MinDisplayEdge || s.Height < MinDisplayEdge:
		return fmt.Errorf("%w: a %dx%d display is too small to lay anything out; "+
			"both edges must be at least %d", ErrInvalidOption, s.Width, s.Height, MinDisplayEdge)
	case s.Width > MaxDimension || s.Height > MaxDimension:
		return fmt.Errorf("%w: %dx%d is beyond the %d-pixel limit",
			ErrInvalidOption, s.Width, s.Height, MaxDimension)
	case s.DensityDPI <= 0 || s.DensityDPI > 8192:
		return fmt.Errorf("%w: density %d dpi", ErrInvalidOption, s.DensityDPI)
	case s.QueueDepth < MinQueueDepth || s.QueueDepth > MaxQueueDepth:
		return fmt.Errorf("%w: %d frame slots, want %d..%d",
			ErrInvalidOption, s.QueueDepth, MinQueueDepth, MaxQueueDepth)
	case s.Content == nil:
		return fmt.Errorf("%w: a display with no Content would render nothing; "+
			"see android.Web and android.Sentinel", ErrInvalidOption)
	}
	return s.Content.Validate()
}

// String renders the spec for logs.
func (s DisplaySpec) String() string {
	c := "no content"
	if s.Content != nil {
		c = s.Content.Describe()
	}
	return fmt.Sprintf("%dx%d @%ddpi %s", s.Width, s.Height, s.DensityDPI, c)
}

// Content is what Android renders on an owned display.
//
// # The seam, and why it is a DECLARATION rather than pixels
//
// The obvious seam would hand the host a framebuffer for it to blit. That is
// the wrong way round here, and would be a round trip with no product: pixels
// this process drew, sent to Android, read back unchanged. If the application
// is drawing, it should composite in Go and skip the display entirely.
//
// The reason to route a ribbon panel through a real Android display is the
// opposite one — to get at what Android renders and this process CANNOT: a
// WebView, a MediaCodec surface, a PdfRenderer, a maps view. None of those can
// be reached from a CGO-free Go process, all of them are ordinary Views, and a
// Presentation is how a View gets onto a display. So Content NAMES what should
// render, the Java host builds it, and the pixels come back through the same
// borrowed-frame path as a screen capture.
//
// That also keeps the rule the rest of this package is built on: the host owns
// the Android objects and decides nothing. A URL is a decision the application
// made; a WebView is the host obeying it.
//
// The set is deliberately closed — a Content implemented outside this package
// would name a Java class the host does not have. Implementations are [Web] and
// [Sentinel].
type Content interface {
	// Describe renders the content for logs.
	Describe() string
	// Validate reports whether the content is usable, wrapping
	// [ErrInvalidOption].
	Validate() error

	// kind and payload are how the content reaches the host. They are
	// unexported so the set stays closed.
	kind() uint32
	payload() string
}

// Content kinds on the wire. They are part of the contract and are mirrored in
// XrWallService.java.
const (
	contentSentinel uint32 = 1
	contentWeb      uint32 = 2
)

// Web renders a WebView on the display.
//
// It is the content that justifies the whole mechanism: a CGO-free Go process
// cannot host a web engine, and a ribbon of web panels is a real product that
// falls straight out of a Presentation on an owned display.
//
// The host's WebView has JavaScript enabled and no file access. Reaching a
// http(s) URL needs android.permission.INTERNET in the APK, which is
// protectionLevel normal; a data: URL needs nothing and is what this package's
// own tests use.
type Web struct {
	// URL is what the WebView loads. http, https and data URLs are accepted.
	URL string
}

// Describe renders the content for logs.
func (w Web) Describe() string {
	u := w.URL
	if len(u) > 64 {
		u = u[:61] + "..."
	}
	return "web " + u
}

// Validate reports whether the URL is one the host will load.
func (w Web) Validate() error {
	switch {
	case w.URL == "":
		return fmt.Errorf("%w: Web.URL is empty", ErrInvalidOption)
	case len(w.URL) > MaxPayload/2:
		return fmt.Errorf("%w: Web.URL is %d bytes, which will not fit a message",
			ErrInvalidOption, len(w.URL))
	case strings.HasPrefix(w.URL, "http://"),
		strings.HasPrefix(w.URL, "https://"),
		strings.HasPrefix(w.URL, "data:"):
		return nil
	}
	return fmt.Errorf("%w: Web.URL %q is not http, https or data", ErrInvalidOption, w.URL)
}

func (w Web) kind() uint32    { return contentWeb }
func (w Web) payload() string { return w.URL }

// Sentinel renders quadrants of exactly known colour with a label across them:
// green top-left, blue bottom-right, red elsewhere, white text.
//
// It is this package's SELF-TEST content, and it is exported because a consumer
// needs it too. A feed that "works" and delivers a black buffer is the classic
// silent failure of this whole mechanism, and the only way to catch it is to
// draw something whose pixels are known in advance and then SAMPLE them rather
// than look at them. [SentinelColorAt] says what a correct frame must hold at a
// given point, and that is what this package's live proof asserts on.
type Sentinel struct {
	// Label is drawn across the middle in white. An empty label draws none.
	Label string
}

// Describe renders the content for logs.
func (s Sentinel) Describe() string { return "sentinel " + s.Label }

// Validate reports whether the label will fit a message.
func (s Sentinel) Validate() error {
	if len(s.Label) > 64 {
		return fmt.Errorf("%w: Sentinel.Label is %d bytes, at most 64", ErrInvalidOption, len(s.Label))
	}
	return nil
}

func (s Sentinel) kind() uint32    { return contentSentinel }
func (s Sentinel) payload() string { return s.Label }

// The colours [Sentinel] paints, as packed 0xRRGGBB.
const (
	SentinelBackground  uint32 = 0xFF0000 // red
	SentinelTopLeft     uint32 = 0x00FF00 // green
	SentinelBottomRight uint32 = 0x0000FF // blue
)

// SentinelColorAt is the colour a correct [Sentinel] frame holds at (x, y) on a
// display of the given size, as 0xRRGGBB. It reports false for a point outside
// the display, or one close enough to a quadrant edge or to the label that
// anti-aliasing makes the exact value unsafe to assert on.
//
// Sample with it rather than comparing whole images: the label is rendered by
// Android's own text stack and will not be byte-identical between releases,
// while the quadrants are flat colour and are.
func SentinelColorAt(w, h, x, y int) (uint32, bool) {
	if w <= 0 || h <= 0 || x < 0 || y < 0 || x >= w || y >= h {
		return 0, false
	}
	// Stay clear of the quadrant seams, and of the label, which is drawn across
	// the middle. An eighth of the display on each side of each seam is a
	// generous margin and keeps the safe points well inside flat colour.
	if abs2(2*x-w) < w/4 || abs2(2*y-h) < h/2 {
		return 0, false
	}
	switch {
	case x < w/2 && y < h/2:
		return SentinelTopLeft, true
	case x >= w/2 && y >= h/2:
		return SentinelBottomRight, true
	}
	return SentinelBackground, true
}

func abs2(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// OwnedDisplay is one display this application created, and the feed of pixels
// Android rendered on it.
//
// It embeds [Stream], so a ribbon consumes it exactly as it consumes a screen
// capture — [Stream.Frame] lends borrowed pixels with the stride carried and
// says whether they are fresh, [Stream.WaitFrame] blocks for the next one. That
// is the point: the ribbon does not care whether a panel is the mirrored phone
// or a display we made.
type OwnedDisplay struct {
	*Stream

	wall *Wall
	id   int
	spec DisplaySpec

	// closer releases the feed. It is nil when Open reserved a slot and then
	// failed, which is why Close on such a display gives the slot back and
	// stops rather than reaching through a Stream that was never built.
	closer func() error
}

// ID is the android.view.Display id the platform gave this display. It is only
// meaningful for logs and for matching against `adb shell dumpsys display`; no
// other application may do anything with it.
func (d *OwnedDisplay) ID() int { return d.id }

// Spec is the specification this display was opened with, with its zero fields
// filled in.
func (d *OwnedDisplay) Spec() DisplaySpec { return d.spec }

// String renders the display for logs.
func (d *OwnedDisplay) String() string {
	return fmt.Sprintf("owned display %d %s", d.id, d.spec)
}

// Close releases the display and its feed, and gives the slot back to the wall.
// It is idempotent.
func (d *OwnedDisplay) Close() error {
	d.wall.forget(d)
	if d.closer == nil {
		// Open failed after the slot was reserved; there is nothing else to
		// release, and giving the slot back is the whole job.
		return nil
	}
	return d.closer()
}
