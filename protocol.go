// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// This file is the SOVEREIGN, transport-agnostic codec: the wire messages and
// their framing, over plain Go values. It carries no syscall and no net
// dependency, so it builds — and is unit-tested to 100% — on every GOOS. The
// transport that dials the host, maps the shared buffer and drives a capture
// lives in client_linux.go.
//
// The framing is byte-for-byte the one go-widgets/android already uses between
// a Go application and its Java host: a 4-byte big-endian length covering the
// type byte and the body, then the type byte, then the body. Big-endian keeps
// the Java side on DataInputStream.readInt with no byte-swapping, and a single
// APK carrying both protocols therefore has one framing to get right, not two.

// Message types. Host→app messages are below 0x80 and app→host at or above it,
// so a misrouted message is a decode error rather than a plausible other
// message.
const (
	// MsgDisplays answers MsgListDisplays with everything the host can see.
	MsgDisplays uint8 = 0x01
	// MsgConfig announces a started capture: the frame geometry, the pixel
	// format and the layout of the shared buffer the frames will appear in.
	MsgConfig uint8 = 0x02
	// MsgFrame says a frame has been written into one slot of the shared
	// buffer. It carries no pixels: the pixels are already there.
	MsgFrame uint8 = 0x03
	// MsgStopped says the capture ended, and why. The user revoking the
	// projection from the system UI arrives here, not as an error on a call.
	MsgStopped uint8 = 0x04
	// MsgConsent answers MsgConsentRequest with the user's decision.
	MsgConsent uint8 = 0x05
	// MsgError reports a failure the host could not answer in-band.
	MsgError uint8 = 0x06
	// MsgBuffer hands the application the shared frame buffer, as an ancillary
	// descriptor, once a capture has been configured. It is the ONE message
	// carrying a descriptor, so it is always written in a single sendmsg.
	//
	// The buffer belongs to the HOST, not to the application, and that is
	// forced rather than chosen: an Android app cannot map a descriptor it
	// received read-write. SharedMemory.fromFileDescriptor rejects a Go memfd
	// outright ("FileDescriptor is not a valid ashmem fd"), and reopening it
	// through /proc/self/fd is refused with EACCES. Only a region the host
	// itself created with SharedMemory.create is writable by the host, so the
	// host creates it and lends it here.
	MsgBuffer uint8 = 0x07
	// MsgPresented says one frame the application wrote has reached the view on
	// an existing display, so its slot is free again.
	//
	// It is the ONLY back-pressure [Screen] has. The pixels travel the opposite
	// way there — this process draws and the host blits — so nothing else stops
	// the application overwriting a slot the host has not finished reading, and
	// a tear in a headset is not a cosmetic defect.
	MsgPresented uint8 = 0x08

	// MsgListDisplays asks the host what displays exist.
	MsgListDisplays uint8 = 0x81
	// MsgConsentRequest asks the host to put the screen-capture consent dialog
	// in front of the user. Only the host can: consent is an Activity result,
	// and the application owns no Activity.
	MsgConsentRequest uint8 = 0x82
	// MsgStart asks the host to begin capturing.
	MsgStart uint8 = 0x83
	// MsgStop asks the host to end the capture but stay connected.
	MsgStop uint8 = 0x84
	// MsgBye tells the host the application is going away.
	MsgBye uint8 = 0x85
	// MsgOpenDisplay asks the wall host to create ONE virtual display, show a
	// Presentation carrying the named content on it, and stream the pixels
	// Android renders there. The host answers with MsgConfig — whose DisplayID
	// carries the id the platform assigned — and then MsgBuffer, exactly as
	// MsgStart does, so one Stream implementation serves both.
	MsgOpenDisplay uint8 = 0x86
	// MsgOpenScreen asks the host to show a Presentation on a display that
	// ALREADY EXISTS — the glasses — carrying a view this process paints into.
	//
	// It is the mirror image of MsgOpenDisplay, and the difference is the
	// direction of the pixels, not the kind of window. MsgOpenDisplay exists to
	// get at what Android can render and a CGO-free Go process cannot; this
	// exists because the headset is an OUTPUT, and what belongs on it is the
	// ribbon this process composited. The host answers with MsgConfig and then
	// MsgBuffer, exactly as the other two do.
	MsgOpenScreen uint8 = 0x87
	// MsgPresent says the application has finished drawing one slot and the
	// host should put it on the screen. It carries no pixels: the pixels are
	// already in the shared buffer.
	MsgPresent uint8 = 0x88
)

// Reasons a capture stopped, carried by [StoppedMessage].
const (
	// StopUser is the user revoking the projection — from the cast chip in the
	// status bar, or from the notification the foreground service must show.
	// It is the normal way a capture ends and is NOT a malfunction.
	StopUser uint8 = 0
	// StopSystem is the platform stopping us: a new projection started
	// elsewhere, the device locked, the service was killed.
	StopSystem uint8 = 1
	// StopApp is this application having asked, with MsgStop or MsgBye.
	StopApp uint8 = 2
)

// MaxPayload bounds one decoded message body. The largest message a host
// legitimately sends is a display list, so a frame beyond this is a
// desynchronised stream — refused rather than allocated.
const MaxPayload = 1 << 16

// ErrShortPayload reports a message whose body is too short for its type.
var ErrShortPayload = errors.New("android: truncated message payload")

// ErrBadPayload reports a message whose body is structurally impossible — a
// string longer than the body that holds it, a negative count.
var ErrBadPayload = errors.New("android: malformed message payload")

// FrameMessage returns one framed message: a 4-byte big-endian length covering
// the type byte and the body, then the type byte, then the body.
//
// It exists as bytes rather than as writes because a message that carries an
// ancillary descriptor has to reach the host in ONE sendmsg: split across two
// writes, the host could attribute the descriptor to the wrong message.
func FrameMessage(typ uint8, body []byte) []byte {
	b := make([]byte, 5+len(body))
	binary.BigEndian.PutUint32(b, uint32(len(body)+1))
	b[4] = typ
	copy(b[5:], body)
	return b
}

// WriteMessage writes one framed message.
func WriteMessage(w io.Writer, typ uint8, body []byte) error {
	_, err := w.Write(FrameMessage(typ, body))
	return err
}

// ReadMessage reads one framed message. It returns io.EOF when the stream ends
// cleanly between messages, so a caller can tell a closed host from a
// truncated one.
func ReadMessage(r io.Reader) (typ uint8, body []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[:4]))
	if n < 1 || n > MaxPayload {
		return 0, nil, fmt.Errorf("android: message length %d out of range", n)
	}
	if _, err := io.ReadFull(r, hdr[4:]); err != nil {
		return 0, nil, err
	}
	body = make([]byte, n-1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[4], body, nil
}

// ConfigMessage announces a started capture.
type ConfigMessage struct {
	// Width and Height are the frame size in pixels.
	Width, Height int
	// Stride is the number of BYTES per row in the shared buffer. It is the
	// host's ImageReader row stride and is NOT necessarily Width*4.
	Stride int
	// Format is the pixel layout, always [FormatRGBA] today.
	Format PixelFormat
	// Slots is how many frame slots the shared buffer holds.
	Slots int
	// SlotSize is the byte size of one slot, Stride*Height rounded up to the
	// host's alignment.
	SlotSize int64
	// DisplayID names the display being captured.
	DisplayID int
}

// EncodeConfig encodes a [ConfigMessage].
func EncodeConfig(c ConfigMessage) []byte {
	b := make([]byte, 0, 32)
	b = appendInt32(b, c.Width)
	b = appendInt32(b, c.Height)
	b = appendInt32(b, c.Stride)
	b = binary.BigEndian.AppendUint32(b, uint32(c.Format))
	b = appendInt32(b, c.Slots)
	b = binary.BigEndian.AppendUint64(b, uint64(c.SlotSize))
	return appendInt32(b, c.DisplayID)
}

// DecodeConfig decodes a [ConfigMessage].
func DecodeConfig(b []byte) (ConfigMessage, error) {
	if len(b) < 32 {
		return ConfigMessage{}, fmt.Errorf("%w: config is %d bytes, want 32", ErrShortPayload, len(b))
	}
	return ConfigMessage{
		Width:     int32At(b, 0),
		Height:    int32At(b, 4),
		Stride:    int32At(b, 8),
		Format:    PixelFormat(binary.BigEndian.Uint32(b[12:])),
		Slots:     int32At(b, 16),
		SlotSize:  int64(binary.BigEndian.Uint64(b[20:])),
		DisplayID: int32At(b, 28),
	}, nil
}

// FrameMsg says a frame has been written into a slot of the shared buffer.
type FrameMsg struct {
	// Seq counts frames since the capture started, from 1.
	Seq uint64
	// Slot is which slot of the shared buffer holds it.
	Slot int
	// Width, Height and Stride describe THIS frame. They normally repeat the
	// config, but the host may resize a capture in flight — a phone rotating
	// under a capture does exactly that — so every frame carries its own
	// geometry rather than inheriting one that may be stale.
	Width, Height, Stride int
	// AtUnixNano is when the host's ImageReader delivered it.
	AtUnixNano int64
}

// At renders the delivery instant. A zero AtUnixNano yields the zero Time
// rather than the epoch, so "the host did not say" stays distinguishable.
func (f FrameMsg) At() time.Time {
	if f.AtUnixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, f.AtUnixNano)
}

// EncodeFrame encodes a [FrameMsg].
func EncodeFrame(f FrameMsg) []byte {
	b := make([]byte, 0, 32)
	b = binary.BigEndian.AppendUint64(b, f.Seq)
	b = appendInt32(b, f.Slot)
	b = appendInt32(b, f.Width)
	b = appendInt32(b, f.Height)
	b = appendInt32(b, f.Stride)
	return binary.BigEndian.AppendUint64(b, uint64(f.AtUnixNano))
}

// DecodeFrame decodes a [FrameMsg].
func DecodeFrame(b []byte) (FrameMsg, error) {
	if len(b) < 32 {
		return FrameMsg{}, fmt.Errorf("%w: frame is %d bytes, want 32", ErrShortPayload, len(b))
	}
	return FrameMsg{
		Seq:        binary.BigEndian.Uint64(b),
		Slot:       int32At(b, 8),
		Width:      int32At(b, 12),
		Height:     int32At(b, 16),
		Stride:     int32At(b, 20),
		AtUnixNano: int64(binary.BigEndian.Uint64(b[24:])),
	}, nil
}

// StoppedMessage says a capture ended and why.
type StoppedMessage struct {
	// Reason is one of [StopUser], [StopSystem] or [StopApp].
	Reason uint8
	// Detail is the host's own wording, which may be empty.
	Detail string
}

// String renders the reason and the detail for a log or an error.
func (s StoppedMessage) String() string {
	name := "stopped"
	switch s.Reason {
	case StopUser:
		name = "the user stopped the screen capture"
	case StopSystem:
		name = "the system stopped the screen capture"
	case StopApp:
		name = "the application stopped the screen capture"
	}
	if s.Detail == "" {
		return name
	}
	return name + ": " + s.Detail
}

// EncodeStopped encodes a [StoppedMessage].
func EncodeStopped(s StoppedMessage) []byte {
	return appendString([]byte{s.Reason}, s.Detail)
}

// DecodeStopped decodes a [StoppedMessage].
func DecodeStopped(b []byte) (StoppedMessage, error) {
	if len(b) < 1 {
		return StoppedMessage{}, fmt.Errorf("%w: stopped is empty", ErrShortPayload)
	}
	detail, _, err := takeString(b[1:])
	if err != nil {
		return StoppedMessage{}, err
	}
	return StoppedMessage{Reason: b[0], Detail: detail}, nil
}

// ErrorMessage is a failure the host could not answer in-band.
type ErrorMessage struct {
	// Code is the host's own numeric code, 0 when it has none.
	Code int
	// Op names what failed, e.g. "getMediaProjection".
	Op string
	// Detail is the platform's message, e.g. a SecurityException's text.
	Detail string
}

// Error renders the operation, the code and the platform's message.
func (e ErrorMessage) Error() string {
	op := e.Op
	if op == "" {
		op = "host"
	}
	if e.Code == 0 {
		return fmt.Sprintf("android: %s: %s", op, e.Detail)
	}
	return fmt.Sprintf("android: %s: (%d) %s", op, e.Code, e.Detail)
}

// EncodeError encodes an [ErrorMessage].
func EncodeError(e ErrorMessage) []byte {
	return appendString(appendString(appendInt32(nil, e.Code), e.Op), e.Detail)
}

// DecodeError decodes an [ErrorMessage].
func DecodeError(b []byte) (ErrorMessage, error) {
	if len(b) < 4 {
		return ErrorMessage{}, fmt.Errorf("%w: error is %d bytes, want at least 4", ErrShortPayload, len(b))
	}
	op, rest, err := takeString(b[4:])
	if err != nil {
		return ErrorMessage{}, err
	}
	detail, _, err := takeString(rest)
	if err != nil {
		return ErrorMessage{}, err
	}
	return ErrorMessage{Code: int32At(b, 0), Op: op, Detail: detail}, nil
}

// EncodeDisplays encodes a display list.
func EncodeDisplays(ds []Display) []byte {
	b := appendInt32(nil, len(ds))
	for _, d := range ds {
		b = appendInt32(b, d.ID)
		b = appendString(b, d.Name)
		b = appendInt32(b, d.Width)
		b = appendInt32(b, d.Height)
		b = appendInt32(b, d.DensityDPI)
		b = appendInt32(b, int(d.RefreshRate*1000))
		b = binary.BigEndian.AppendUint32(b, d.Flags)
	}
	return b
}

// DecodeDisplays decodes a display list.
func DecodeDisplays(b []byte) ([]Display, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: displays is %d bytes, want at least 4", ErrShortPayload, len(b))
	}
	n := int32At(b, 0)
	if n < 0 {
		return nil, fmt.Errorf("%w: negative display count %d", ErrBadPayload, n)
	}
	rest := b[4:]
	// Every entry is at least 4+2+4*4+4 bytes, so a count that could not
	// possibly fit is refused before anything is allocated for it.
	if min := n * 26; len(rest) < min {
		return nil, fmt.Errorf("%w: %d displays need at least %d bytes, got %d",
			ErrShortPayload, n, min, len(rest))
	}
	ds := make([]Display, 0, n)
	for i := 0; i < n; i++ {
		if len(rest) < 4 {
			return nil, fmt.Errorf("%w: display %d truncated", ErrShortPayload, i)
		}
		var d Display
		d.ID = int32At(rest, 0)
		var err error
		d.Name, rest, err = takeString(rest[4:])
		if err != nil {
			return nil, err
		}
		if len(rest) < 20 {
			return nil, fmt.Errorf("%w: display %d truncated after its name", ErrShortPayload, i)
		}
		d.Width = int32At(rest, 0)
		d.Height = int32At(rest, 4)
		d.DensityDPI = int32At(rest, 8)
		d.RefreshRate = float64(int32At(rest, 12)) / 1000
		d.Flags = binary.BigEndian.Uint32(rest[16:])
		rest = rest[20:]
		ds = append(ds, d)
	}
	return ds, nil
}

// StartMessage asks the host to begin capturing.
type StartMessage struct {
	// DisplayID is the display to capture. Only the default display can be
	// captured by an unprivileged app; see [ErrNotCapturable].
	DisplayID int
	// Width and Height are the requested frame size in pixels; zero means the
	// display's native size.
	Width, Height int
	// MilliFPS is the frame-rate ceiling in thousandths of a frame per second,
	// so 60 fps is 60000 and a rate below one frame a second is still exact.
	MilliFPS uint32
	// Slots is how many frame slots the shared buffer holds.
	Slots int
}

// EncodeStart encodes a [StartMessage].
func EncodeStart(s StartMessage) []byte {
	b := appendInt32(nil, s.DisplayID)
	b = appendInt32(b, s.Width)
	b = appendInt32(b, s.Height)
	b = binary.BigEndian.AppendUint32(b, s.MilliFPS)
	return appendInt32(b, s.Slots)
}

// DecodeStart decodes a [StartMessage].
func DecodeStart(b []byte) (StartMessage, error) {
	if len(b) < 20 {
		return StartMessage{}, fmt.Errorf("%w: start is %d bytes, want 20", ErrShortPayload, len(b))
	}
	return StartMessage{
		DisplayID: int32At(b, 0),
		Width:     int32At(b, 4),
		Height:    int32At(b, 8),
		MilliFPS:  binary.BigEndian.Uint32(b[12:]),
		Slots:     int32At(b, 16),
	}, nil
}

// EncodeBuffer encodes the layout of the shared buffer accompanying MsgBuffer.
func EncodeBuffer(slots int, slotSize int64) []byte {
	return binary.BigEndian.AppendUint64(appendInt32(nil, slots), uint64(slotSize))
}

// DecodeBuffer decodes the layout of the shared buffer.
func DecodeBuffer(b []byte) (slots int, slotSize int64, err error) {
	if len(b) < 12 {
		return 0, 0, fmt.Errorf("%w: buffer is %d bytes, want 12", ErrShortPayload, len(b))
	}
	return int32At(b, 0), int64(binary.BigEndian.Uint64(b[4:])), nil
}

// EncodeConsent encodes the user's decision.
func EncodeConsent(granted bool) []byte {
	if granted {
		return []byte{1}
	}
	return []byte{0}
}

// DecodeConsent decodes the user's decision.
func DecodeConsent(b []byte) (bool, error) {
	if len(b) < 1 {
		return false, fmt.Errorf("%w: consent is empty", ErrShortPayload)
	}
	return b[0] != 0, nil
}

func appendInt32(b []byte, v int) []byte {
	return binary.BigEndian.AppendUint32(b, uint32(int32(v)))
}

func int32At(b []byte, off int) int { return int(int32(binary.BigEndian.Uint32(b[off:]))) }

// appendString appends a 2-byte big-endian length and the UTF-8 bytes. A
// string longer than the length can express is truncated rather than
// misencoded: a display name is cosmetic, and a corrupted frame length would
// desynchronise the whole stream.
func appendString(b []byte, s string) []byte {
	if len(s) > 0xffff {
		s = s[:0xffff]
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// takeString reads a length-prefixed string and returns it with what follows.
func takeString(b []byte) (string, []byte, error) {
	if len(b) < 2 {
		return "", nil, fmt.Errorf("%w: string length missing", ErrShortPayload)
	}
	n := int(binary.BigEndian.Uint16(b))
	if len(b) < 2+n {
		return "", nil, fmt.Errorf("%w: string of %d bytes in %d remaining", ErrShortPayload, n, len(b)-2)
	}
	return string(b[2 : 2+n]), b[2+n:], nil
}

// SocketSuffix is appended to the application's package name to form the
// abstract socket the host listens on when nothing names one explicitly. It
// matches XrHostService.SOCKET_SUFFIX.
const SocketSuffix = ".xr"

// WallSocketSuffix is appended to the package name to get the abstract socket
// the WALL host listens on, matching XrWallService.SOCKET_SUFFIX.
//
// It is a second socket, and a second service, because the two have nothing in
// common but a wire format. Capture needs a MediaProjection, a consent dialog
// and — from API 34 — a mediaProjection foreground service, none of which an
// owned display needs: it asks for no permission at all. Keeping them apart
// means creating a ribbon panel cannot put a "recording your screen" chip in
// the status bar, and that a capture session ending cannot take a panel with
// it. The capture host also serves exactly one connection, whereas the wall
// serves one PER DISPLAY.
const WallSocketSuffix = ".xrwall"

// EnvWallSocket names the environment variable carrying the wall host's socket,
// the counterpart of [EnvSocket].
const EnvWallSocket = "XR_ANDROID_WALL_SOCKET"

// DeriveWallSocket works out which abstract socket the wall host listens on,
// exactly as [DeriveSocket] does for the capture host.
func DeriveWallSocket(env string, envOK bool, home string) string {
	if envOK && env != "" {
		return env
	}
	pkg := packageFromHome(home)
	if pkg == "" {
		return ""
	}
	return pkg + WallSocketSuffix
}

// OpenDisplayMessage asks the wall host for one owned display.
type OpenDisplayMessage struct {
	// Width and Height are the display's size in pixels.
	Width, Height int
	// DensityDPI is the density Android lays the view hierarchy out against.
	DensityDPI int
	// Slots is how many frame slots the shared buffer holds.
	Slots int
	// ContentKind names the View the host should build; see [Content].
	ContentKind uint32
	// ContentPayload is that content's one string argument — a URL for a
	// WebView, a label for the sentinel.
	ContentPayload string
}

// EncodeOpenDisplay encodes an [OpenDisplayMessage].
func EncodeOpenDisplay(m OpenDisplayMessage) []byte {
	b := appendInt32(nil, m.Width)
	b = appendInt32(b, m.Height)
	b = appendInt32(b, m.DensityDPI)
	b = appendInt32(b, m.Slots)
	b = binary.BigEndian.AppendUint32(b, m.ContentKind)
	return appendString(b, m.ContentPayload)
}

// DecodeOpenDisplay decodes an [OpenDisplayMessage].
func DecodeOpenDisplay(b []byte) (OpenDisplayMessage, error) {
	if len(b) < 20 {
		return OpenDisplayMessage{}, fmt.Errorf("%w: open-display is %d bytes, want at least 20",
			ErrShortPayload, len(b))
	}
	payload, _, err := takeString(b[20:])
	if err != nil {
		return OpenDisplayMessage{}, err
	}
	return OpenDisplayMessage{
		Width:          int32At(b, 0),
		Height:         int32At(b, 4),
		DensityDPI:     int32At(b, 8),
		Slots:          int32At(b, 12),
		ContentKind:    binary.BigEndian.Uint32(b[16:]),
		ContentPayload: payload,
	}, nil
}

// DeriveSocket works out which abstract socket to dial.
//
// An explicit name wins: a host that spawned this process sets [EnvSocket] and
// that is the end of it. But in the composition this package is really for, the
// process is spawned by a DIFFERENT host — go-widgets/android's Activity, which
// owns the drawing surface and knows nothing about capture — and never sets it.
// Android gives such a process no way to ask its own package name either.
//
// What it does give it is HOME, which that host sets to the app's private
// files directory: "/data/user/0/org.example.app/files". The package name is in
// there, and the host's socket name follows from it. That is why the two hosts
// can live in one APK without either one having to know about the other.
//
// It returns "" when neither route yields a name.
func DeriveSocket(env string, envOK bool, home string) string {
	if envOK && env != "" {
		return env
	}
	pkg := packageFromHome(home)
	if pkg == "" {
		return ""
	}
	return pkg + SocketSuffix
}

// packageFromHome extracts the package name from an Android app's private
// files directory. Both spellings Android has used are accepted:
// /data/user/<n>/<pkg>/... and the older /data/data/<pkg>/....
func packageFromHome(home string) string {
	const (
		multi  = "/data/user/"
		legacy = "/data/data/"
	)
	rest := ""
	switch {
	case len(home) > len(multi) && home[:len(multi)] == multi:
		rest = home[len(multi):]
		// Skip the user number and its slash.
		i := indexByte(rest, '/')
		if i < 0 {
			return ""
		}
		rest = rest[i+1:]
	case len(home) > len(legacy) && home[:len(legacy)] == legacy:
		rest = home[len(legacy):]
	default:
		return ""
	}
	if i := indexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	// A package name has at least one dot; anything else is not one, and
	// dialling a guess would be worse than reporting no host at all.
	if indexByte(rest, '.') < 0 {
		return ""
	}
	return rest
}

// indexByte is strings.IndexByte, written out so this file keeps the single
// import set it has always had — it is the codec, and it stays a leaf.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// OpenScreenMessage asks the host for a Presentation on a display that already
// exists, and for the shared buffer this process will paint into.
type OpenScreenMessage struct {
	// DisplayID is the android.view.Display id to present on. It must be a
	// display the host can see and whose FLAG_PRESENTATION is set; the host
	// refuses anything else rather than guessing.
	DisplayID int
	// Width and Height are the size of the frames this process will write, in
	// pixels. They need not match the display: the host scales the bitmap to
	// fill it, which is how a ribbon rendered at one resolution reaches panels
	// of another.
	Width, Height int
	// Slots is how many frame slots the shared buffer holds.
	Slots int
}

// EncodeOpenScreen encodes an [OpenScreenMessage].
func EncodeOpenScreen(m OpenScreenMessage) []byte {
	b := appendInt32(nil, m.DisplayID)
	b = appendInt32(b, m.Width)
	b = appendInt32(b, m.Height)
	return appendInt32(b, m.Slots)
}

// DecodeOpenScreen decodes an [OpenScreenMessage].
func DecodeOpenScreen(b []byte) (OpenScreenMessage, error) {
	if len(b) < 16 {
		return OpenScreenMessage{}, fmt.Errorf("%w: open-screen is %d bytes, want 16",
			ErrShortPayload, len(b))
	}
	return OpenScreenMessage{
		DisplayID: int32At(b, 0),
		Width:     int32At(b, 4),
		Height:    int32At(b, 8),
		Slots:     int32At(b, 12),
	}, nil
}

// PresentMessage hands one slot of the shared buffer to the host to put on the
// screen. The geometry travels with it rather than being implied by the config,
// for the same reason [FrameMsg] carries it: the one place a wrong number would
// become an out-of-range slice over shared memory is the place to state it
// explicitly and check it.
type PresentMessage struct {
	// Seq numbers the frame, from 1. It comes back in [PresentedMessage].
	Seq int64
	// Slot is which slot of the shared buffer holds the pixels.
	Slot int
	// Width, Height and Stride describe the pixels in that slot.
	Width, Height, Stride int
}

// EncodePresent encodes a [PresentMessage].
func EncodePresent(m PresentMessage) []byte {
	b := binary.BigEndian.AppendUint64(nil, uint64(m.Seq))
	b = appendInt32(b, m.Slot)
	b = appendInt32(b, m.Width)
	b = appendInt32(b, m.Height)
	return appendInt32(b, m.Stride)
}

// DecodePresent decodes a [PresentMessage].
func DecodePresent(b []byte) (PresentMessage, error) {
	if len(b) < 24 {
		return PresentMessage{}, fmt.Errorf("%w: present is %d bytes, want 24",
			ErrShortPayload, len(b))
	}
	return PresentMessage{
		Seq:    int64(binary.BigEndian.Uint64(b)),
		Slot:   int32At(b, 8),
		Width:  int32At(b, 12),
		Height: int32At(b, 16),
		Stride: int32At(b, 20),
	}, nil
}

// PresentedMessage says one frame reached the screen and its slot is free.
type PresentedMessage struct {
	// Seq is the [PresentMessage.Seq] of the frame that was presented.
	Seq int64
	// Slot is the slot it occupied, which the application may now draw into.
	Slot int
}

// EncodePresented encodes a [PresentedMessage].
func EncodePresented(m PresentedMessage) []byte {
	return appendInt32(binary.BigEndian.AppendUint64(nil, uint64(m.Seq)), m.Slot)
}

// DecodePresented decodes a [PresentedMessage].
func DecodePresented(b []byte) (PresentedMessage, error) {
	if len(b) < 12 {
		return PresentedMessage{}, fmt.Errorf("%w: presented is %d bytes, want 12",
			ErrShortPayload, len(b))
	}
	return PresentedMessage{Seq: int64(binary.BigEndian.Uint64(b)), Slot: int32At(b, 8)}, nil
}

// CensusMessage types. The census asks what hardware is there, which needs no
// permission of any kind; see census.go.
const (
	// MsgCameras answers MsgListCameras with every camera the camera2 API can
	// see, including external ones.
	MsgCameras uint8 = 0x09
	// MsgUSBDevices answers MsgListUSBDevices with every device on the USB host
	// port, down to each interface's endpoints.
	MsgUSBDevices uint8 = 0x0a

	// MsgListCameras asks the host what cameras exist.
	MsgListCameras uint8 = 0x89
	// MsgListUSBDevices asks the host what is attached to the USB host port.
	//
	// It reads the device list, which needs no permission: a permission is
	// needed to OPEN a device, not to see that it is there. So a census costs
	// the user no dialog, which is what makes it usable before deciding
	// anything.
	MsgListUSBDevices uint8 = 0x8a
)

// EncodeCameras encodes a camera list.
func EncodeCameras(cs []Camera) []byte {
	b := appendInt32(nil, len(cs))
	for _, c := range cs {
		b = appendString(b, c.ID)
		b = appendInt32(b, int(c.Facing))
		b = appendInt32(b, c.Width)
		b = appendInt32(b, c.Height)
	}
	return b
}

// DecodeCameras decodes a camera list.
func DecodeCameras(b []byte) ([]Camera, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("%w: cameras is %d bytes, want at least 4", ErrShortPayload, len(b))
	}
	n := int32At(b, 0)
	if n < 0 {
		return nil, fmt.Errorf("%w: negative camera count %d", ErrBadPayload, n)
	}
	rest := b[4:]
	// Every entry is at least a 2-byte name length and three int32s, so a count
	// that could not possibly fit is refused before anything is allocated.
	if min := n * 14; len(rest) < min {
		return nil, fmt.Errorf("%w: %d cameras need at least %d bytes, got %d",
			ErrShortPayload, n, min, len(rest))
	}
	cs := make([]Camera, 0, min(n, len(rest)/14+1))
	for i := 0; i < n; i++ {
		var c Camera
		var err error
		c.ID, rest, err = takeString(rest)
		if err != nil {
			return nil, err
		}
		if len(rest) < 12 {
			return nil, fmt.Errorf("%w: camera %d truncated after its id", ErrShortPayload, i)
		}
		c.Facing = CameraFacing(int32At(rest, 0))
		c.Width = int32At(rest, 4)
		c.Height = int32At(rest, 8)
		rest = rest[12:]
		cs = append(cs, c)
	}
	return cs, nil
}

// EncodeUSBDevices encodes a USB device list, down to each endpoint.
func EncodeUSBDevices(ds []USBDevice) []byte {
	b := appendInt32(nil, len(ds))
	for _, d := range ds {
		b = appendString(b, d.Name)
		b = appendInt32(b, d.VendorID)
		b = appendInt32(b, d.ProductID)
		b = appendString(b, d.Manufacturer)
		b = appendString(b, d.Product)
		b = appendInt32(b, d.Class)
		b = appendInt32(b, d.Subclass)
		b = appendInt32(b, d.Protocol)
		b = appendInt32(b, len(d.Interfaces))
		for _, i := range d.Interfaces {
			b = appendInt32(b, i.Number)
			b = appendInt32(b, i.Alternate)
			b = appendInt32(b, i.Class)
			b = appendInt32(b, i.Subclass)
			b = appendInt32(b, i.Protocol)
			b = appendInt32(b, len(i.Endpoints))
			for _, e := range i.Endpoints {
				b = appendInt32(b, e.Address)
				b = appendInt32(b, e.Attributes)
				b = appendInt32(b, e.MaxPacketSize)
				b = appendInt32(b, e.Interval)
			}
		}
	}
	return b
}

// DecodeUSBDevices decodes a USB device list.
func DecodeUSBDevices(b []byte) ([]USBDevice, error) {
	n, rest, err := takeCount(b, "usb devices")
	if err != nil {
		return nil, err
	}
	// The smallest a device entry can be is two empty-string lengths, its two
	// ids, its class triple and an interface count: 30 bytes. Capacity is bounded
	// by what is actually LEFT rather than by the count, so a host that announced
	// two billion devices cannot turn one message into an out-of-memory.
	ds := make([]USBDevice, 0, min(n, len(rest)/30+1))
	for i := 0; i < n; i++ {
		var d USBDevice
		if d.Name, rest, err = takeString(rest); err != nil {
			return nil, err
		}
		if len(rest) < 8 {
			return nil, fmt.Errorf("%w: usb device %d truncated after its name", ErrShortPayload, i)
		}
		d.VendorID, d.ProductID = int32At(rest, 0), int32At(rest, 4)
		rest = rest[8:]
		if d.Manufacturer, rest, err = takeString(rest); err != nil {
			return nil, err
		}
		if d.Product, rest, err = takeString(rest); err != nil {
			return nil, err
		}
		if len(rest) < 16 {
			return nil, fmt.Errorf("%w: usb device %d truncated after its strings", ErrShortPayload, i)
		}
		d.Class, d.Subclass, d.Protocol = int32At(rest, 0), int32At(rest, 4), int32At(rest, 8)
		ni := int32At(rest, 12)
		rest = rest[16:]
		if ni < 0 {
			return nil, fmt.Errorf("%w: usb device %d has %d interfaces", ErrBadPayload, i, ni)
		}
		d.Interfaces = make([]USBInterface, 0, min(ni, len(rest)/24+1))
		for j := 0; j < ni; j++ {
			if len(rest) < 24 {
				return nil, fmt.Errorf("%w: usb device %d interface %d truncated",
					ErrShortPayload, i, j)
			}
			iface := USBInterface{
				Number: int32At(rest, 0), Alternate: int32At(rest, 4),
				Class: int32At(rest, 8), Subclass: int32At(rest, 12), Protocol: int32At(rest, 16),
			}
			ne := int32At(rest, 20)
			rest = rest[24:]
			if ne < 0 {
				return nil, fmt.Errorf("%w: interface %d has %d endpoints", ErrBadPayload, j, ne)
			}
			iface.Endpoints = make([]USBEndpoint, 0, min(ne, len(rest)/16+1))
			for k := 0; k < ne; k++ {
				if len(rest) < 16 {
					return nil, fmt.Errorf("%w: usb device %d interface %d endpoint %d truncated",
						ErrShortPayload, i, j, k)
				}
				iface.Endpoints = append(iface.Endpoints, USBEndpoint{
					Address: int32At(rest, 0), Attributes: int32At(rest, 4),
					MaxPacketSize: int32At(rest, 8), Interval: int32At(rest, 12),
				})
				rest = rest[16:]
			}
			d.Interfaces = append(d.Interfaces, iface)
		}
		ds = append(ds, d)
	}
	return ds, nil
}

// takeCount reads a leading count and refuses one that is negative.
//
// ⛔ THE CAPACITY IS BOUNDED BY WHAT IS LEFT, not by the count. A host that
// announced two billion interfaces would otherwise have this allocate for them
// before reading a byte, which is a message turning into an out-of-memory.
func takeCount(b []byte, what string) (int, []byte, error) {
	if len(b) < 4 {
		return 0, nil, fmt.Errorf("%w: %s is %d bytes, want at least 4", ErrShortPayload, what, len(b))
	}
	n := int32At(b, 0)
	if n < 0 {
		return 0, nil, fmt.Errorf("%w: negative %s count %d", ErrBadPayload, what, n)
	}
	return n, b[4:], nil
}

// USB handover messages. Opening a USB device is the one thing in this package
// that ASKS THE USER — a system dialog naming the device and the application —
// so it is the one census question that costs a click.
const (
	// MsgUSBHandle answers MsgOpenUSBDevice with the device's file descriptor,
	// as an ancillary descriptor. It carries no body: the descriptor is the
	// message.
	//
	// It is the SECOND message in this protocol to ride on SCM_RIGHTS, and for
	// the opposite reason from MsgBuffer: there the host owns memory the
	// application reads, here the host owns a KERNEL HANDLE the application
	// drives with ioctl. Android will not let an ordinary process open
	// /dev/bus/usb at all, so the descriptor can only come from a component
	// that asked the user.
	MsgUSBHandle uint8 = 0x0b

	// MsgOpenUSBDevice asks the host to open one USB device by name, after
	// obtaining the user's permission for it, and to lend the descriptor.
	MsgOpenUSBDevice uint8 = 0x8b
)

// EncodeOpenUSBDevice encodes the name of the device to open — the node name
// from [USBDevice.Name], which is what a USB permission is granted against.
func EncodeOpenUSBDevice(name string) []byte { return appendString(nil, name) }

// DecodeOpenUSBDevice decodes it.
func DecodeOpenUSBDevice(b []byte) (string, error) {
	name, _, err := takeString(b)
	return name, err
}
