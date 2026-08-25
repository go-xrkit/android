// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

// The transport. Android IS Linux, and everything this file needs from the
// operating system is an ordinary Linux facility — an abstract Unix socket, a
// descriptor arriving over SCM_RIGHTS and an mmap — which is why the build tag
// is linux rather than android: the same code runs against a fake host on a CI
// runner and on a developer's machine, so the suite exercises the REAL
// back-end rather than a stand-in.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// EnvSocket names the environment variable carrying the abstract socket the XR
// host listens on. A host that spawns this process sets it.
//
// It is not the only route, and in the composition this package is really for
// it is not the route taken: the process is spawned by go-widgets/android's
// Activity, which owns the drawing surface, knows nothing about capture and
// sets no such variable. [DeriveSocket] covers that case from HOME.
const EnvSocket = "XR_ANDROID_SOCKET"

// Indirections the tests replace to reach the failure branches a real kernel
// will not produce on demand.
var (
	mmap      = syscall.Mmap
	munmap    = syscall.Munmap
	closeFD   = unix.Close
	dialUnix  = net.DialUnix
	lookupEnv = os.LookupEnv
)

// fdConn reads a Unix socket while keeping the ancillary descriptors that
// arrive with the bytes, so [ReadMessage] can stay an ordinary io.Reader
// consumer and the one message that carries a descriptor can still find it.
//
// Only the pump goroutine touches it, so it holds no lock.
type fdConn struct {
	uc   *net.UnixConn
	rbuf []byte
	oob  []byte
	buf  []byte // the unread tail of rbuf
	// read is the one read the transport makes. It is a FIELD rather than a
	// package variable so the suite can present the answers a kernel gives only
	// at the edges — an end-of-stream that arrives as no bytes, no descriptors
	// and no error — without reaching across every other connection's pump
	// goroutine to do it, which would be a data race in the harness.
	read func(b, oob []byte) (int, int, error)
	// fd is the descriptor received but not yet claimed, or -1.
	//
	// ONE, not a queue. A stream socket does not promise that a descriptor is
	// handed over on the same read as the first byte of the message it was
	// sent with — measured here, the descriptor sent with MsgBuffer arrives
	// while the PRECEDING message is still being parsed — so a descriptor
	// cannot be attributed by position. Holding exactly one, replacing it
	// (and closing the old one) when another arrives, bounds what a
	// misbehaving host can accumulate at one descriptor, ever.
	fd int
}

func newFDConn(uc *net.UnixConn) *fdConn {
	c := &fdConn{uc: uc, fd: -1,
		rbuf: make([]byte, 8192), oob: make([]byte, unix.CmsgSpace(4)*4)}
	c.read = func(b, oob []byte) (int, int, error) {
		n, oobn, _, _, err := uc.ReadMsgUnix(b, oob)
		return n, oobn, err
	}
	return c
}

func (c *fdConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		n, oobn, err := c.read(c.rbuf, c.oob)
		if oobn > 0 {
			c.takeRights(c.oob[:oobn])
		}
		if n > 0 {
			c.buf = c.rbuf[:n]
		}
		if err != nil {
			return 0, err
		}
		if n == 0 && oobn == 0 {
			// Neither bytes nor descriptors and no error is the peer closing;
			// treating it as anything else would spin.
			return 0, io.EOF
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *fdConn) takeRights(oob []byte) {
	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return
	}
	for i := range scms {
		fds, err := unix.ParseUnixRights(&scms[i])
		if err != nil {
			continue
		}
		for _, fd := range fds {
			c.closeUnclaimed()
			c.fd = fd
		}
	}
}

// claimFD returns the unclaimed descriptor, or -1.
func (c *fdConn) claimFD() int {
	fd := c.fd
	c.fd = -1
	return fd
}

// closeUnclaimed releases a descriptor nobody took, so a host that attaches one
// to the wrong message costs a descriptor rather than leaking it.
func (c *fdConn) closeUnclaimed() {
	if c.fd >= 0 {
		_ = closeFD(c.fd)
		c.fd = -1
	}
}

// Available reports whether this process is running under a host that speaks
// this protocol. It consults the environment only: it opens nothing and blocks
// on nothing, so it is safe to call before deciding whether Android capture is
// this program's platform at all.
func Available() bool { return socketName() != "" }

// socketName is the abstract socket to dial: whatever the host named, or the
// name derived from the package this process belongs to. See [DeriveSocket].
func socketName() string {
	env, ok := lookupEnv(EnvSocket)
	home, _ := lookupEnv("HOME")
	return DeriveSocket(env, ok, home)
}

// session is the process-wide connection to the host. There is one because
// there is one host and one projection: a second connection would be a second
// consent dialog for the same screen.
type session struct {
	uc *net.UnixConn
	fc *fdConn

	wmu sync.Mutex // serialises socket writes

	reqMu   sync.Mutex // one request in flight at a time
	replies chan reply

	mu     sync.Mutex
	stream *Stream
	closed bool
	done   chan struct{}
	err    error
}

type reply struct {
	typ  uint8
	body []byte
	fd   int
}

var (
	sessMu   sync.Mutex
	sessCur  *session
	sessErr  error
	sessOnce bool
)

// connect returns the process-wide session, dialling the host on first use.
//
// A failure is remembered: the host is either there for the life of the process
// or it is not, and re-dialling a missing socket on every frame of a compositor
// would be a syscall storm in the hot path.
func connect() (*session, error) {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sessOnce {
		return sessCur, sessErr
	}
	sessOnce = true
	name := socketName()
	if name == "" {
		sessErr = ErrUnsupported
		return nil, sessErr
	}
	uc, err := dialHost(name)
	if err != nil {
		sessErr = fmt.Errorf("%w: dialling the host on @%s: %w", ErrUnsupported, name, err)
		return nil, sessErr
	}
	s := &session{uc: uc, fc: newFDConn(uc), replies: make(chan reply, 1), done: make(chan struct{})}
	go s.pump()
	sessCur = s
	return s, nil
}

// resetForTest drops the memoised session. It exists for the suite, which
// stands up a succession of fake hosts in one process.
func resetForTest() {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sessCur != nil {
		sessCur.shutdown(ErrClosed)
	}
	sessCur, sessErr, sessOnce = nil, nil, false
}

// How long connect keeps trying before giving up, and how often. Nothing
// orders the host's listener against the application's first call: the host
// that SPAWNS the Go process is go-widgets/android's Activity, which starts
// the capture service as a peer and cannot wait for it. So a refused
// connection early on means "not yet", not "never" — and 5 seconds is far
// longer than a service takes to bind while still being a bounded failure.
var (
	dialBudget = 5 * time.Second
	dialPause  = 25 * time.Millisecond
)

// dialHost connects to the host's abstract socket, retrying while it is still
// coming up. Linux spells the abstract namespace with a leading NUL, which Go
// writes as a leading '@' in the address.
func dialHost(name string) (*net.UnixConn, error) {
	addr := &net.UnixAddr{Name: "@" + name, Net: "unix"}
	deadline := time.Now().Add(dialBudget)
	for {
		uc, err := dialUnix("unix", nil, addr)
		if err == nil {
			return uc, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(dialPause)
	}
}

// pump reads the host forever, routing control messages to whoever is waiting
// and frame messages to the active stream.
func (s *session) pump() {
	for {
		typ, body, err := ReadMessage(s.fc)
		if err != nil {
			s.fc.closeUnclaimed()
			s.shutdown(fmt.Errorf("android: host connection ended: %w", err))
			return
		}
		fd := -1
		if typ == MsgBuffer {
			fd = s.fc.claimFD()
		}
		switch typ {
		case MsgFrame, MsgStopped:
			s.mu.Lock()
			st := s.stream
			s.mu.Unlock()
			if st != nil {
				st.deliver(typ, body)
			}
		default:
			// A reply nobody is waiting for is dropped rather than queued: it
			// belongs to a request whose context already expired, and keeping
			// it would answer the NEXT request with the previous one's answer.
			select {
			case s.replies <- reply{typ, body, fd}:
			default:
				if fd >= 0 {
					_ = closeFD(fd)
				}
			}
		}
	}
}

func (s *session) shutdown(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed, s.err = true, err
	st := s.stream
	close(s.done)
	s.mu.Unlock()
	_ = s.uc.Close()
	if st != nil {
		st.fail(err)
	}
}

// send writes one framed message.
func (s *session) send(typ uint8, body []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.uc.Write(FrameMessage(typ, body))
	return err
}

// request sends one message and waits for the host's answer.
func (s *session) request(ctx context.Context, typ uint8, body []byte, want uint8) ([]byte, error) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	// Drain an answer left over from an abandoned request, so this one is not
	// handed the previous one's.
	select {
	case r := <-s.replies:
		if r.fd >= 0 {
			_ = closeFD(r.fd)
		}
	default:
	}
	if err := s.send(typ, body); err != nil {
		return nil, fmt.Errorf("android: sending 0x%02x: %w", typ, err)
	}
	r, err := s.await(ctx, want)
	return r.body, err
}

// await waits for the host's next answer of the wanted type, without sending
// anything. Starting a capture needs it: the host answers MsgStart with a
// config and THEN lends the frame buffer, two messages to one request.
func (s *session) await(ctx context.Context, want uint8) (reply, error) {
	for {
		select {
		case <-ctx.Done():
			return reply{fd: -1}, ctx.Err()
		case <-s.done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			return reply{fd: -1}, err
		case r := <-s.replies:
			if r.typ == want {
				return r, nil
			}
			// Anything else is not this request's answer, and a descriptor
			// riding on it would leak. One close covers every such case.
			if r.fd >= 0 {
				_ = closeFD(r.fd)
			}
			if r.typ == MsgError {
				e, derr := DecodeError(r.body)
				if derr != nil {
					return reply{fd: -1}, derr
				}
				return reply{fd: -1}, hostError(e)
			}
			// A stale answer to a request that has since timed out. Ignoring
			// it is what keeps a slow host from permanently offsetting every
			// reply by one.
		}
	}
}

// hostError maps the host's own failure onto a sentinel where one fits, so a
// caller can use errors.Is without knowing Android's vocabulary.
func hostError(e ErrorMessage) error {
	switch {
	case e.Code == codeConsentDenied:
		return fmt.Errorf("%w: %s", ErrPermissionDenied, e.Detail)
	case e.Code == codeNoDisplay:
		return fmt.Errorf("%w: %s", ErrNoDisplay, e.Detail)
	case e.Code == codeNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, e.Detail)
	case e.Code == codeNotCapturable:
		return fmt.Errorf("%w: %s", ErrNotCapturable, e.Detail)
	}
	return e
}

// Codes the host uses for the failures that have a sentinel here. They are part
// of the wire contract and are mirrored in XrHostService.java.
const (
	codeConsentDenied = 1
	codeNoDisplay     = 2
	codeNotFound      = 3
	codeNotCapturable = 4
)

// Displays returns every display the host can see.
//
// Only the default one can be captured (see [ErrNotCapturable]), but the others
// matter all the same: a headset attached over USB-C DP Alt Mode appears here,
// and its [Display.Name] is how a catalogue identifies the model.
func Displays(ctx context.Context) ([]Display, error) {
	s, err := connect()
	if err != nil {
		return nil, err
	}
	body, err := s.request(ctx, MsgListDisplays, nil, MsgDisplays)
	if err != nil {
		return nil, err
	}
	ds, err := DecodeDisplays(body)
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return nil, ErrNoDisplay
	}
	return ds, nil
}

// DefaultDisplay returns the built-in screen, the only display an unprivileged
// app may capture.
func DefaultDisplay(ctx context.Context) (Display, error) {
	ds, err := Displays(ctx)
	if err != nil {
		return Display{}, err
	}
	for _, d := range ds {
		if d.Default() {
			return d, nil
		}
	}
	return Display{}, fmt.Errorf("%w: display %d", ErrNotFound, DefaultDisplayID)
}

// Authorized reports whether the host already holds a projection token, so a
// capture can start without a dialog. It never prompts.
//
// Unlike a macOS TCC grant this is NOT a lasting permission: Android issues a
// token per session and forgets it, so this answers "right now", not "ever".
func Authorized() bool {
	s, err := connect()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body, err := s.request(ctx, MsgConsentRequest, []byte{0}, MsgConsent)
	if err != nil {
		return false
	}
	ok, err := DecodeConsent(body)
	return err == nil && ok
}

// RequestAuthorization asks the host to put Android's screen-capture consent
// dialog in front of the user, and blocks until they answer or ctx expires.
//
// It is a separate call rather than something [CaptureDisplay] does implicitly
// because the dialog takes over the screen: an application wants to choose when
// that happens, and to say why first.
func RequestAuthorization(ctx context.Context) (bool, error) {
	s, err := connect()
	if err != nil {
		return false, err
	}
	body, err := s.request(ctx, MsgConsentRequest, []byte{1}, MsgConsent)
	if err != nil {
		return false, err
	}
	return DecodeConsent(body)
}

// CaptureDisplay starts streaming a display.
//
// The user must already have consented — see [RequestAuthorization] — and only
// the default display can be captured. The returned stream must be closed.
func CaptureDisplay(ctx context.Context, d Display, o Options) (*Stream, error) {
	s, err := connect()
	if err != nil {
		return nil, err
	}
	if !d.Default() {
		return nil, fmt.Errorf("%w: display %d %q", ErrNotCapturable, d.ID, d.Name)
	}
	opts, err := o.resolve(d.Width, d.Height)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	busy := s.stream != nil
	s.mu.Unlock()
	if busy {
		return nil, errors.New("android: a capture is already running; close it first")
	}

	body, err := s.request(ctx, MsgStart, EncodeStart(StartMessage{
		DisplayID: d.ID,
		Width:     opts.Width,
		Height:    opts.Height,
		MilliFPS:  milliFPS(opts.FPS),
		Slots:     opts.QueueDepth,
	}), MsgConfig)
	if err != nil {
		return nil, err
	}
	cfg, err := DecodeConfig(body)
	if err != nil {
		return nil, err
	}
	if err := checkConfig(cfg); err != nil {
		s.stop()
		return nil, err
	}

	// The second half of the handshake: the host lends its frame buffer. It
	// cannot come earlier, because the host only learns its own row stride
	// from the first buffer its ImageReader hands back.
	r, err := s.await(ctx, MsgBuffer)
	if err != nil {
		s.stop()
		return nil, err
	}
	slots, slotSize, berr := DecodeBuffer(r.body)
	if berr == nil && (r.fd < 0 || slots != cfg.Slots || slotSize != cfg.SlotSize) {
		berr = fmt.Errorf("android: the host lent %d slots of %d bytes with fd %d, "+
			"having announced %d of %d", slots, slotSize, r.fd, cfg.Slots, cfg.SlotSize)
	}
	if berr != nil {
		if r.fd >= 0 {
			_ = closeFD(r.fd)
		}
		s.stop()
		return nil, berr
	}

	st := &Stream{s: s, opts: opts, cfg: cfg, wake: make(chan struct{}, 1)}
	if err := st.mapBuffer(r.fd); err != nil {
		s.stop()
		return nil, err
	}
	s.mu.Lock()
	s.stream = st
	s.mu.Unlock()
	return st, nil
}

// checkConfig refuses a config that cannot describe a real capture, before any
// memory is reserved on its word.
func checkConfig(c ConfigMessage) error {
	switch {
	case c.Width <= 0 || c.Height <= 0:
		return fmt.Errorf("android: host announced a %dx%d capture", c.Width, c.Height)
	case c.Width > MaxDimension || c.Height > MaxDimension:
		return fmt.Errorf("android: host announced a %dx%d capture, beyond the %d-pixel limit",
			c.Width, c.Height, MaxDimension)
	case c.Stride < c.Width*4:
		return fmt.Errorf("android: host announced stride %d for a %d-pixel row", c.Stride, c.Width)
	case c.Format != FormatRGBA:
		return fmt.Errorf("android: host announced pixel format %s, want %s", c.Format, FormatRGBA)
	case c.Slots < MinQueueDepth || c.Slots > MaxQueueDepth:
		return fmt.Errorf("android: host announced %d frame slots, want %d..%d",
			c.Slots, MinQueueDepth, MaxQueueDepth)
	case c.SlotSize < int64(c.Stride)*int64(c.Height):
		return fmt.Errorf("android: host announced a %d-byte slot for a %dx%d frame of stride %d",
			c.SlotSize, c.Width, c.Height, c.Stride)
	}
	return nil
}

func (s *session) stop() { _ = s.send(MsgStop, nil) }

// Stream is a running capture.
type Stream struct {
	s    *session
	opts Options
	cfg  ConfigMessage
	buf  []byte

	mu      sync.Mutex
	latest  FrameMsg
	cur     FrameMsg
	stats   Stats
	err     error
	closed  bool
	pending bool
	wake    chan struct{}
}

// mapBuffer maps the frame buffer the host lent, and gives the descriptor back.
//
// It is mapped READ-ONLY: the host writes the pixels and this process only ever
// reads them, so a stray write here traps instead of silently corrupting a
// frame under the compositor. The descriptor is closed once mapped, because a
// mapping outlives the descriptor it came from and holding one open would leak
// a descriptor per capture.
//
// The region on the other side is an Android SharedMemory — ashmem, which
// dirties no page cache at all. That the HOST owns it rather than this process
// is forced: an app cannot map a received descriptor read-write on Android 15,
// which is measured in the README.
func (st *Stream) mapBuffer(fd int) error {
	defer func() { _ = closeFD(fd) }()
	size := int64(st.cfg.Slots) * st.cfg.SlotSize
	b, err := mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("android: mapping the %d-byte frame buffer the host lent: %w", size, err)
	}
	st.buf = b
	return nil
}

// deliver takes one host message on the pump goroutine.
func (st *Stream) deliver(typ uint8, body []byte) {
	if typ == MsgStopped {
		s, err := DecodeStopped(body)
		if err != nil {
			st.fail(err)
			return
		}
		if s.Reason == StopUser {
			st.fail(fmt.Errorf("%w: %s", ErrPermissionDenied, s.String()))
			return
		}
		st.fail(errors.New("android: " + s.String()))
		return
	}
	f, err := DecodeFrame(body)
	if err != nil {
		st.fail(err)
		return
	}
	st.mu.Lock()
	if st.closed || !st.frameFits(f) {
		st.mu.Unlock()
		return
	}
	now := time.Now()
	if st.stats.Frames > 0 {
		st.stats.Interval = now.Sub(st.stats.Last)
	}
	if st.pending {
		st.stats.Superseded++
	}
	st.stats.Frames++
	st.stats.Last = now
	st.latest, st.pending = f, true
	st.mu.Unlock()
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// frameFits reports whether a frame message describes a region actually inside
// the mapping. A host that says otherwise is refused rather than believed: this
// is the one place a bad number would become an out-of-range slice over shared
// memory.
func (st *Stream) frameFits(f FrameMsg) bool {
	if f.Slot < 0 || f.Slot >= st.cfg.Slots {
		return false
	}
	if f.Width <= 0 || f.Height <= 0 || f.Stride < f.Width*4 {
		return false
	}
	need := int64(f.Stride) * int64(f.Height)
	return need <= st.cfg.SlotSize
}

func (st *Stream) fail(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.mu.Unlock()
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// Frame returns the most recent frame and whether it is newer than the one the
// previous call returned.
//
// The bytes are BORROWED from the shared mapping and stay valid until the next
// Frame, WaitFrame or Close. In steady state this allocates nothing.
func (st *Stream) Frame() (Frame, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return Frame{}, false
	}
	fresh := st.pending
	if fresh {
		st.cur, st.pending = st.latest, false
	}
	if st.cur.Seq == 0 {
		return Frame{}, false
	}
	return st.frameLocked(), fresh
}

func (st *Stream) frameLocked() Frame {
	f := st.cur
	off := int64(f.Slot) * st.cfg.SlotSize
	n := int64(f.Stride) * int64(f.Height)
	return Frame{
		Pix:    st.buf[off : off+n : off+n],
		Width:  f.Width,
		Height: f.Height,
		Stride: f.Stride,
		Seq:    f.Seq,
		At:     f.At(),
	}
}

// WaitFrame blocks until a frame newer than the last one returned arrives, or
// ctx expires. It reports [ErrNoFrame] on expiry, which on a motionless screen
// is the ordinary answer rather than a malfunction.
func (st *Stream) WaitFrame(ctx context.Context) (Frame, error) {
	for {
		st.mu.Lock()
		switch {
		case st.closed:
			st.mu.Unlock()
			return Frame{}, ErrClosed
		case st.err != nil:
			err := st.err
			st.mu.Unlock()
			return Frame{}, err
		case st.pending:
			st.cur, st.pending = st.latest, false
			f := st.frameLocked()
			st.mu.Unlock()
			return f, nil
		}
		st.mu.Unlock()
		select {
		case <-ctx.Done():
			return Frame{}, fmt.Errorf("%w: %w", ErrNoFrame, ctx.Err())
		case <-st.wake:
		}
	}
}

// Stats reports what the stream has seen since it started.
func (st *Stream) Stats() Stats {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.stats
}

// Options returns the options actually in force, with every zero field
// resolved.
func (st *Stream) Options() Options { return st.opts }

// Format returns the pixel layout of every frame this stream produces.
func (st *Stream) Format() PixelFormat { return st.cfg.Format }

// Size returns the capture's frame size in pixels.
func (st *Stream) Size() (int, int) { return st.cfg.Width, st.cfg.Height }

// Err reports why the SYSTEM stopped the capture, nil while it is running. The
// user revoking the projection arrives here wrapping [ErrPermissionDenied].
func (st *Stream) Err() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

// String renders the stream for logs.
func (st *Stream) String() string {
	return fmt.Sprintf("android capture %dx%d stride %d %s, %d slots",
		st.cfg.Width, st.cfg.Height, st.cfg.Stride, st.cfg.Format, st.cfg.Slots)
}

// Close ends the capture and releases the shared mapping. It is idempotent, and
// every borrowed frame is invalid afterwards.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	buf := st.buf
	st.buf = nil
	st.mu.Unlock()

	st.s.mu.Lock()
	if st.s.stream == st {
		st.s.stream = nil
	}
	st.s.mu.Unlock()
	st.s.stop()

	var err error
	if buf != nil {
		err = munmap(buf)
	}
	select {
	case st.wake <- struct{}{}:
	default:
	}
	return err
}
