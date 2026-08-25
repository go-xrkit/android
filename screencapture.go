// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Package android captures the Android screen from a pure-Go, CGO-free
// application, for an XR compositor that redraws every frame.
//
// # What Android permits, and what it does not
//
// Everything here is shaped by one measured fact: an ordinary, unprivileged APK
// may MIRROR the screen and may draw its own content on an attached display,
// and it may do nothing else. It cannot give itself extra desktops. On
// Android 15 (API 35), creating a virtual display and launching an activity on
// it fails with
//
//	SecurityException: Permission Denial: starting Intent { ... }
//	    ... with launchDisplayId=N
//
// for ANY activity, including the caller's own; asking for
// VIRTUAL_DISPLAY_FLAG_TRUSTED — which is not in the public SDK at all — fails
// with "Requires ADD_TRUSTED_DISPLAY permission", a signature|role permission;
// and VirtualDeviceManager.createVirtualDevice fails with "Access denied,
// requires: android.permission.CREATE_VIRTUAL_DEVICE", which is internal|role.
// See the README for the whole transcript. So on Android an XR ribbon carries
// the phone's real screen, and the application's own content, and that is all.
//
// # The split, and why there is a Java host
//
// Android hands no drawable surface, and no MediaProjection, to a process that
// is not the app: both are behind JNI, and JNI needs cgo. So the app is two
// processes, exactly as in go-widgets/android — a small Java host owning the
// Android objects, and an ordinary CGO_ENABLED=0 GOOS=android executable owning
// everything above the pixels. This package is the application half.
//
// # The hot path
//
// [Stream.Frame] hands back a BORROWED view of the most recent frame: the bytes
// are the shared mapping the host wrote into, not a copy, and the borrow lasts
// until the next [Stream.Frame], [Stream.WaitFrame] or [Stream.Close]. In
// steady state a Frame call performs no allocation at all. Copy out with
// [Frame.CopyTight] or [Frame.NRGBA] to keep it longer.
//
// # Stride
//
// A captured frame's rows may be PADDED: Stride is the number of BYTES per row
// and it is not necessarily Width*4. Always index with Stride, or use
// [Frame.Row]. The Android 15 emulator measured here reported stride 4320 for a
// 1080-wide capture — exactly Width*4, no padding — which is precisely why the
// API carries the number instead of trusting it.
//
// # Frames only arrive when something changes
//
// A MediaProjection capture is change-driven, like ScreenCaptureKit on macOS.
// [Options.FPS] is a CEILING, not a rate: with a still screen, this package's
// own probe took 39 frames in 4.0 s and then nothing at all; with the screen
// scrolling it took 574 frames in 11.9 s. Do not treat a missing frame as a
// failure; the second return value of [Stream.Frame] is the truth about whether
// anything changed.
//
// # Consent
//
// Every capture session needs the user to agree through a system dialog, and
// the agreement dies with the session. [Authorized] reports whether the host
// already holds a projection token; [RequestAuthorization] asks the host to put
// the dialog in front of the user and blocks until they answer.
package android

import (
	"errors"
	"fmt"
	"image"
	"time"
)

// Sentinel errors. All are stable and may be matched with errors.Is.
var (
	// ErrUnsupported is reported on every platform that is not Android, and on
	// Android when the process was not started by a host that speaks this
	// protocol. The module still builds and vets everywhere, so an application
	// keeps cross-building on a developer's workstation.
	ErrUnsupported = errors.New("android: screen capture is unavailable " +
		"(not running under a go-xrkit Android host)")

	// ErrPermissionDenied is reported when the user refused the screen-capture
	// consent dialog, or revoked a projection already running. Its message
	// names the remedy.
	ErrPermissionDenied = errors.New("android: screen-capture consent denied — " +
		"Android asks the user for permission once per capture session and the " +
		"answer cannot be remembered; call RequestAuthorization again and accept " +
		"the system dialog")

	// ErrNoDisplay is reported when a capture was asked for and the host
	// listed no display at all.
	ErrNoDisplay = errors.New("android: no capturable display")

	// ErrNotFound is reported when a display ID does not name anything the
	// host can see.
	ErrNotFound = errors.New("android: no such display")

	// ErrNotCapturable is reported for a display that exists but that an
	// unprivileged app may not capture. MediaProjection mirrors the DEFAULT
	// display and nothing else: capturing a second display needs
	// CAPTURE_VIDEO_OUTPUT, which is a signature permission.
	ErrNotCapturable = errors.New("android: only the default display can be captured " +
		"by an unprivileged app (capturing another display needs the signature " +
		"permission CAPTURE_VIDEO_OUTPUT)")

	// ErrClosed is reported by every [Stream] method after [Stream.Close].
	ErrClosed = errors.New("android: stream is closed")

	// ErrNoFrame is reported by [Stream.WaitFrame] when no frame arrived
	// before its context expired. It is NOT a malfunction: a motionless screen
	// legitimately produces no frames.
	ErrNoFrame = errors.New("android: no frame available")

	// ErrInvalidOption is reported by [Options.Validate] and wraps a
	// description of the offending field.
	ErrInvalidOption = errors.New("android: invalid option")

	// ErrShortBuffer is reported by [Frame.CopyTight] when the destination is
	// too small to hold the frame.
	ErrShortBuffer = errors.New("android: destination buffer too short")
)

// PixelFormat names the layout of a captured frame, using Android's own
// android.graphics.PixelFormat constants so the value on the wire is the one
// the host read off the ImageReader.
type PixelFormat uint32

// FormatRGBA is 32-bit RGBA, android.graphics.PixelFormat.RGBA_8888. It is the
// only format this package streams, and it is what ImageReader produces for a
// MediaProjection mirror. It is also byte-for-byte what a go-widgets painter
// writes, so a captured frame composites with no conversion.
//
// This differs from the sibling packages on other platforms: macOS
// ScreenCaptureKit hands back BGRA. [Frame.NRGBA] hides the difference, and
// anything compositing raw bytes must ask [Stream.Format].
const FormatRGBA PixelFormat = 1 // android.graphics.PixelFormat.RGBA_8888

// String renders the format for logs.
func (f PixelFormat) String() string {
	if f == FormatRGBA {
		return "RGBA_8888"
	}
	return fmt.Sprintf("PixelFormat(%d)", uint32(f))
}

// BytesPerPixel is the size of one pixel in this format, 0 for a format this
// package does not stream.
func (f PixelFormat) BytesPerPixel() int {
	if f == FormatRGBA {
		return 4
	}
	return 0
}

// Display flags, mirroring android.view.Display's public FLAG_* constants. The
// two that matter to an XR application are [FlagPresentation], which says the
// system considers a display suitable for an app's own full-screen content, and
// [FlagPrivate], which says it belongs to one app.
const (
	FlagSupportsProtectedBuffers uint32 = 1 << 0
	FlagSecure                   uint32 = 1 << 1
	FlagPrivate                  uint32 = 1 << 2
	FlagPresentation             uint32 = 1 << 3
	FlagRound                    uint32 = 1 << 4
)

// Display is a display the host can see.
//
// Width and Height are in PIXELS — Android reports no separate point space, so
// unlike the macOS sibling there is only one pair of numbers, and DensityDPI
// carries what a scale factor would carry elsewhere.
type Display struct {
	// ID is the android.view.Display display id. 0 is the built-in screen.
	ID int
	// Name is what the system calls the display. For the built-in panel this
	// is a device string ("Built-in Screen" on the Android 15 emulator); for a
	// display attached over USB-C DP Alt Mode it comes from the sink's EDID,
	// which is how XR glasses announce their model.
	Name string
	// Width and Height are the display's real size in pixels.
	Width, Height int
	// DensityDPI is the display's density in dots per inch.
	DensityDPI int
	// RefreshRate is the active mode's rate in hertz.
	RefreshRate float64
	// Flags are android.view.Display's FLAG_* bits; see [FlagPresentation].
	Flags uint32
}

// Default reports whether this is the built-in screen, the only display an
// unprivileged app may capture.
func (d Display) Default() bool { return d.ID == DefaultDisplayID }

// DefaultDisplayID is android.view.Display.DEFAULT_DISPLAY.
const DefaultDisplayID = 0

// Presentation reports whether the system considers this display suitable for
// an application's own full-screen content — which is what an XR headset
// attached over DP Alt Mode is for.
func (d Display) Presentation() bool { return d.Flags&FlagPresentation != 0 }

// String renders the display for logs.
func (d Display) String() string {
	s := fmt.Sprintf("display %d %q %dx%d @%ddpi %.3gHz", d.ID, d.Name,
		d.Width, d.Height, d.DensityDPI, d.RefreshRate)
	if d.Default() {
		s += " (default)"
	}
	if d.Presentation() {
		s += " (presentation)"
	}
	return s
}

// Options configures a capture stream.
//
// The zero Options is usable: it captures the default display at its native
// pixel size, at up to 60 frames per second.
type Options struct {
	// Width and Height are the requested frame size in PIXELS. Zero means the
	// display's native size.
	Width, Height int

	// FPS is the CEILING on the frame rate, not a guarantee: a MediaProjection
	// only produces a frame when the content changed. Zero means [DefaultFPS].
	FPS float64

	// QueueDepth is how many frame slots the shared buffer holds. Zero means
	// [DefaultQueueDepth]. It must leave room for the frame lent out and the
	// one being written, so values below [MinQueueDepth] are rejected — with
	// two slots a host that produced two frames while the consumer held one
	// would overwrite the borrow.
	QueueDepth int
}

// Defaults applied to the zero value of the corresponding [Options] field.
const (
	// DefaultFPS is the frame-rate ceiling used when Options.FPS is zero.
	DefaultFPS = 60.0
	// DefaultQueueDepth is the slot count used when Options.QueueDepth is zero.
	DefaultQueueDepth = 3
	// MinQueueDepth is the smallest slot count this package accepts.
	MinQueueDepth = 3
	// MaxQueueDepth bounds the slot count. A slot is a whole framebuffer —
	// 10.4 MB for a 1080x2400 phone — so this is a real memory limit, not a
	// formality.
	MaxQueueDepth = 8
	// MaxDimension is the largest frame edge accepted, a sanity bound well
	// above any real display.
	MaxDimension = 32768
)

// Validate reports whether the options are self-consistent, wrapping
// [ErrInvalidOption]. It does not consult the system.
func (o Options) Validate() error {
	if o.Width < 0 || o.Height < 0 {
		return fmt.Errorf("%w: negative size %dx%d", ErrInvalidOption, o.Width, o.Height)
	}
	if (o.Width == 0) != (o.Height == 0) {
		return fmt.Errorf("%w: Width and Height must both be set or both be zero, got %dx%d",
			ErrInvalidOption, o.Width, o.Height)
	}
	if o.Width > MaxDimension || o.Height > MaxDimension {
		return fmt.Errorf("%w: size %dx%d exceeds the %d-pixel limit",
			ErrInvalidOption, o.Width, o.Height, MaxDimension)
	}
	if o.FPS < 0 {
		return fmt.Errorf("%w: negative FPS %g", ErrInvalidOption, o.FPS)
	}
	if o.FPS > 0 && o.FPS < 0.01 {
		return fmt.Errorf("%w: FPS %g is below the 0.01 minimum", ErrInvalidOption, o.FPS)
	}
	if o.QueueDepth < 0 {
		return fmt.Errorf("%w: negative QueueDepth %d", ErrInvalidOption, o.QueueDepth)
	}
	if o.QueueDepth > 0 && o.QueueDepth < MinQueueDepth {
		return fmt.Errorf("%w: QueueDepth %d is below the minimum of %d",
			ErrInvalidOption, o.QueueDepth, MinQueueDepth)
	}
	if o.QueueDepth > MaxQueueDepth {
		return fmt.Errorf("%w: QueueDepth %d exceeds the maximum of %d",
			ErrInvalidOption, o.QueueDepth, MaxQueueDepth)
	}
	return nil
}

// resolve fills the zero fields from the defaults and from the display's native
// size, and returns the options actually used.
func (o Options) resolve(nativeW, nativeH int) (Options, error) {
	if err := o.Validate(); err != nil {
		return Options{}, err
	}
	r := o
	if r.Width == 0 {
		if nativeW <= 0 || nativeH <= 0 {
			return Options{}, fmt.Errorf("%w: no size given and the display reports %dx%d",
				ErrInvalidOption, nativeW, nativeH)
		}
		r.Width, r.Height = nativeW, nativeH
	}
	if r.FPS == 0 {
		r.FPS = DefaultFPS
	}
	if r.QueueDepth == 0 {
		r.QueueDepth = DefaultQueueDepth
	}
	return r, nil
}

// milliFPS converts the frame-rate ceiling to the thousandths of a frame per
// second the wire carries. A non-positive rate yields [DefaultFPS] rather than
// zero, which the host would read as "as fast as possible".
func milliFPS(fps float64) uint32 {
	if fps <= 0 {
		fps = DefaultFPS
	}
	v := int64(fps*1000 + 0.5)
	if v < 1 {
		v = 1
	}
	if v > int64(^uint32(0)) {
		v = int64(^uint32(0))
	}
	return uint32(v)
}

// Frame is a BORROWED view of one captured frame.
//
// Pix aliases the shared mapping the host wrote into. It stays valid only until
// the next [Stream.Frame], [Stream.WaitFrame] or [Stream.Close] on the stream
// that produced it. Do not retain it; copy with [Frame.CopyTight] or
// [Frame.NRGBA] if you need it to outlive the borrow.
type Frame struct {
	// Pix is the frame's bytes in [FormatRGBA], Stride bytes per row, Height
	// rows. len(Pix) == Stride*Height.
	Pix []byte
	// Width and Height are the frame's size in pixels.
	Width, Height int
	// Stride is the number of BYTES per row. It may be padded and is NOT
	// necessarily Width*4.
	Stride int
	// Seq counts frames since the stream started; it is 0 before the first
	// frame and strictly increases afterwards.
	Seq uint64
	// At is when the host's ImageReader delivered the frame.
	At time.Time
}

// Valid reports whether the frame holds pixels.
func (f Frame) Valid() bool {
	return f.Width > 0 && f.Height > 0 && f.Stride >= f.Width*4 && len(f.Pix) >= f.Stride*f.Height
}

// TightLen is the number of bytes the frame occupies with no row padding,
// Width*4*Height.
func (f Frame) TightLen() int { return f.Width * 4 * f.Height }

// Row returns row y of the frame, Width*4 bytes with the padding trimmed off.
// It does not allocate. It returns nil for an out-of-range y or an invalid
// frame.
func (f Frame) Row(y int) []byte {
	if !f.Valid() || y < 0 || y >= f.Height {
		return nil
	}
	off := y * f.Stride
	return f.Pix[off : off+f.Width*4 : off+f.Width*4]
}

// CopyTight copies the frame into dst with the row padding removed, so dst
// holds Width*4*Height bytes of contiguous RGBA. It reports how many bytes it
// wrote, or [ErrShortBuffer] if dst is too small. It allocates nothing.
func (f Frame) CopyTight(dst []byte) (int, error) {
	if !f.Valid() {
		return 0, ErrNoFrame
	}
	n := f.TightLen()
	if len(dst) < n {
		return 0, fmt.Errorf("%w: need %d bytes, got %d", ErrShortBuffer, n, len(dst))
	}
	rowLen := f.Width * 4
	// The fast path: an unpadded frame is one contiguous run, which is what a
	// full-width display capture usually is.
	if f.Stride == rowLen {
		return copy(dst, f.Pix[:n]), nil
	}
	for y := 0; y < f.Height; y++ {
		src := y * f.Stride
		copy(dst[y*rowLen:(y+1)*rowLen], f.Pix[src:src+rowLen])
	}
	return n, nil
}

// NRGBA copies the frame into a freshly allocated image.NRGBA. It is the
// convenience path for saving a frame to disk; it allocates, so it does not
// belong in a per-frame loop.
//
// Android's RGBA_8888 is already image.NRGBA's byte order, so unlike the macOS
// sibling this is a copy with no channel swap — but it is still a copy, because
// the source is padded and borrowed.
func (f Frame) NRGBA() (*image.NRGBA, error) {
	if !f.Valid() {
		return nil, ErrNoFrame
	}
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	// The rows are copied here rather than through CopyTight so there is no
	// error branch that cannot happen: img.Pix is exactly TightLen bytes by
	// construction, and a check that can never fail is untestable code.
	rowLen := f.Width * 4
	for y := 0; y < f.Height; y++ {
		copy(img.Pix[y*img.Stride:y*img.Stride+rowLen], f.Pix[y*f.Stride:y*f.Stride+rowLen])
	}
	return img, nil
}

// Stats reports what a stream has seen since it started.
type Stats struct {
	// Frames is the number of frames the host delivered.
	Frames uint64
	// Superseded is the number of delivered frames replaced by a newer one
	// before the consumer ever asked for them. A large value next to Frames
	// means the consumer is slower than the capture.
	Superseded uint64
	// Last is when the most recent frame arrived.
	Last time.Time
	// Interval is the gap between the two most recent frames.
	Interval time.Duration
}

// FPS is the instantaneous rate implied by [Stats.Interval], 0 when fewer than
// two frames have arrived.
func (s Stats) FPS() float64 {
	if s.Interval <= 0 {
		return 0
	}
	return float64(time.Second) / float64(s.Interval)
}
