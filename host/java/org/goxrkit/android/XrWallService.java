// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Presentation;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Paint;
import android.graphics.PixelFormat;
import android.graphics.drawable.ColorDrawable;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.IBinder;
import android.os.ParcelFileDescriptor;
import android.util.Log;
import android.view.Display;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.WebView;

import java.io.DataOutputStream;
import java.io.FileDescriptor;
import java.io.IOException;
import java.io.InputStream;
import java.nio.ByteBuffer;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * The WALL host: displays this application creates and owns, each carrying a
 * Presentation of our own content, each read back through an ImageReader.
 *
 * <p>It is a second service rather than part of {@link XrHostService} because
 * the two have nothing in common but a wire format. Capture needs a
 * MediaProjection, a consent dialog and — from API 34 — a mediaProjection
 * foreground service. <b>An owned display needs none of that: no permission at
 * all.</b> Keeping them apart means opening a ribbon panel cannot put a
 * "recording your screen" chip in the status bar, and a capture session ending
 * cannot take a panel with it.
 *
 * <p>It also serves <b>one connection per display</b>, where the capture host
 * serves exactly one connection in total. A display's lifetime is its socket's
 * lifetime, so closing one cannot disturb another's frames, and a client that
 * dies has its displays released by the kernel closing its sockets.
 *
 * <h2>THE LIMIT IS THE SAFETY</h2>
 *
 * Measured on Android 15: creating the 304th virtual display does not fail, it
 * kills system_server with Surface$OutOfResourcesException: NO_MEMORY and the
 * device soft-reboots. The count was the SAME at 640x480 and at 1920x1080, so
 * it is SurfaceControl handles rather than graphics memory and making the
 * displays smaller does not help. {@link #MAX_DISPLAYS} is enforced here as
 * well as in the Go API, because a second process must not be able to get past
 * it by not using that API.
 */
public final class XrWallService extends Service {
    public static final String TAG = "xr-wall";

    /** Matches android.WallSocketSuffix. */
    private static final String SOCKET_SUFFIX = ".xrwall";

    /**
     * The most owned displays this host will serve at once, across every
     * connection. Deliberately nowhere near the 304 that killed system_server.
     */
    public static final int MAX_DISPLAYS = 32;

    // The wire, mirrored from protocol.go.
    private static final int MSG_CONFIG = 0x02, MSG_FRAME = 0x03, MSG_STOPPED = 0x04;
    private static final int MSG_ERROR = 0x06, MSG_BUFFER = 0x07;
    private static final int MSG_OPEN_DISPLAY = 0x86, MSG_STOP = 0x84, MSG_BYE = 0x85;

    private static final int STOP_SYSTEM = 1, STOP_APP = 2;
    private static final int CODE_TOO_MANY_DISPLAYS = 5;

    private static final int CONTENT_SENTINEL = 1, CONTENT_WEB = 2;

    private LocalServerSocket server;
    private HandlerThread thread;
    private Handler handler;

    /** How many displays exist right now, across every connection. */
    private static final AtomicInteger live = new AtomicInteger();

    @Override
    public IBinder onBind(Intent i) {
        return null;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        thread = new HandlerThread("xr-wall");
        thread.start();
        handler = new Handler(thread.getLooper());
        String name = getPackageName() + SOCKET_SUFFIX;
        try {
            server = new LocalServerSocket(name);
        } catch (IOException e) {
            Log.e(TAG, "cannot listen on @" + name, e);
            stopSelf();
            return;
        }
        Log.i(TAG, "listening on @" + name + ", at most " + MAX_DISPLAYS + " displays");
        new Thread(this::accept, "xr-wall-accept").start();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        return START_NOT_STICKY;
    }

    @Override
    public void onDestroy() {
        try {
            if (server != null) {
                server.close();
            }
        } catch (IOException ignored) {
            // Going away regardless.
        }
        if (thread != null) {
            thread.quitSafely();
        }
        super.onDestroy();
    }

    /** Accepts connections forever; each one is one display. */
    private void accept() {
        while (true) {
            LocalSocket s;
            try {
                s = server.accept();
            } catch (IOException e) {
                Log.i(TAG, "accept ended: " + e);
                return;
            }
            Panel p = new Panel(s);
            new Thread(p::run, "xr-wall-panel").start();
        }
    }

    /** One connection, and therefore at most one owned display. */
    private final class Panel {
        private final LocalSocket sock;
        private DataOutputStream out;

        private VirtualDisplay vd;
        private ImageReader reader;
        private Presentation presentation;
        private android.os.SharedMemory sm;
        private ByteBuffer shared;
        private int slots, frameW, frameH;
        private long slotSize, seq;
        private volatile boolean streaming;
        private boolean counted;
        private final Object pixelLock = new Object();

        Panel(LocalSocket s) {
            this.sock = s;
        }

        void run() {
            try {
                out = new DataOutputStream(sock.getOutputStream());
                pump(sock.getInputStream());
            } catch (IOException e) {
                Log.i(TAG, "panel connection ended: " + e);
            } finally {
                release();
            }
        }

        private void pump(InputStream in) throws IOException {
            byte[] hdr = new byte[4];
            while (true) {
                if (!readFully(in, hdr, 4)) {
                    return;
                }
                int n = ((hdr[0] & 0xff) << 24) | ((hdr[1] & 0xff) << 16)
                        | ((hdr[2] & 0xff) << 8) | (hdr[3] & 0xff);
                if (n < 1 || n > (1 << 16)) {
                    Log.e(TAG, "message length " + n + " out of range");
                    return;
                }
                byte[] body = new byte[n];
                if (!readFully(in, body, n)) {
                    return;
                }
                int typ = body[0] & 0xff;
                byte[] payload = new byte[n - 1];
                System.arraycopy(body, 1, payload, 0, n - 1);
                handle(typ, payload);
            }
        }

        private void handle(int typ, byte[] body) {
            switch (typ) {
                case MSG_OPEN_DISPLAY:
                    handler.post(() -> open(body));
                    break;
                case MSG_STOP:
                case MSG_BYE:
                    handler.post(this::release);
                    break;
                default:
                    Log.w(TAG, "ignoring message 0x" + Integer.toHexString(typ));
            }
        }

        /**
         * Creates the display, shows the Presentation and starts the stream.
         * Runs on the host's handler thread, which is where every Android
         * object below belongs.
         */
        private void open(byte[] body) {
            Dec d = new Dec(body);
            int w = d.i32(), h = d.i32(), dpi = d.i32(), wantSlots = d.i32();
            int kind = d.i32();
            String payload = d.str();
            if (w <= 0 || h <= 0 || dpi <= 0 || wantSlots <= 0) {
                error(0, "open", "a " + w + "x" + h + " @" + dpi + "dpi display with "
                        + wantSlots + " slots is not a display");
                return;
            }

            // The limit, enforced here and not only in the Go API: a process
            // that does not use that API must not be able to get past it.
            if (live.incrementAndGet() > MAX_DISPLAYS) {
                live.decrementAndGet();
                error(CODE_TOO_MANY_DISPLAYS, "createVirtualDisplay",
                        "this host serves at most " + MAX_DISPLAYS + " owned displays at once; "
                                + "enough of them kill system_server and reboot the device, and "
                                + "the ceiling does not move if you make them smaller");
                return;
            }
            counted = true;

            frameW = w;
            frameH = h;
            slots = wantSlots;
            try {
                reader = ImageReader.newInstance(w, h, PixelFormat.RGBA_8888, slots);
                // OWN_CONTENT_ONLY | PRESENTATION and nothing else. PUBLIC needs
                // a MediaProjection token; TRUSTED needs ADD_TRUSTED_DISPLAY,
                // which is a signature permission. Neither is asked for, and
                // neither is needed to show a Presentation here.
                vd = getSystemService(DisplayManager.class).createVirtualDisplay(
                        "go-xrkit wall", w, h, dpi, reader.getSurface(),
                        DisplayManager.VIRTUAL_DISPLAY_FLAG_OWN_CONTENT_ONLY
                                | DisplayManager.VIRTUAL_DISPLAY_FLAG_PRESENTATION);
            } catch (Throwable t) {
                error(0, "createVirtualDisplay", String.valueOf(t));
                return;
            }
            if (vd == null) {
                error(0, "createVirtualDisplay", "the platform returned no display");
                return;
            }
            Display display = vd.getDisplay();

            View content = buildContent(display, kind, payload);
            if (content == null) {
                error(0, "content", "content kind " + kind + " is not one this host builds");
                return;
            }
            try {
                Presentation p = new Presentation(XrWallService.this, display);
                p.setContentView(content, new ViewGroup.LayoutParams(
                        ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
                p.getWindow().setBackgroundDrawable(new ColorDrawable(Color.BLACK));
                p.getWindow().setLayout(ViewGroup.LayoutParams.MATCH_PARENT,
                        ViewGroup.LayoutParams.MATCH_PARENT);
                p.show();
                presentation = p;
            } catch (Throwable t) {
                // The measurement that decides whether this service can own the
                // window at all. Reporting exactly what was thrown is the point.
                error(0, "Presentation.show", String.valueOf(t));
                return;
            }
            Log.i(TAG, "display " + display.getDisplayId() + " " + w + "x" + h + " @" + dpi
                    + "dpi showing " + describe(kind, payload));

            reader.setOnImageAvailableListener(this::onImage, handler);
            announceConfig(display.getDisplayId());
        }

        /** Builds the View the caller declared. */
        private View buildContent(Display display, int kind, String payload) {
            Context dc = createDisplayContext(display);
            switch (kind) {
                case CONTENT_SENTINEL:
                    return new SentinelView(dc, payload);
                case CONTENT_WEB:
                    WebView wv = new WebView(dc);
                    wv.getSettings().setJavaScriptEnabled(true);
                    wv.getSettings().setAllowFileAccess(false);
                    wv.getSettings().setAllowContentAccess(false);
                    wv.setBackgroundColor(Color.WHITE);
                    wv.loadUrl(payload);
                    return wv;
                default:
                    return null;
            }
        }

        private String describe(int kind, String payload) {
            return (kind == CONTENT_WEB ? "web " : "sentinel ") + payload;
        }

        private void announceConfig(int displayId) {
            // The stride is not known until the first Image arrives, so the
            // aligned upper bound is announced and the slot is sized for it.
            int stride = align(frameW * 4);
            slotSize = (long) stride * (long) frameH;
            Enc e = new Enc();
            e.i32(frameW);
            e.i32(frameH);
            e.i32(stride);
            e.i32(PixelFormat.RGBA_8888);
            e.i32(slots);
            e.i64(slotSize);
            e.i32(displayId);
            send(MSG_CONFIG, e.bytes());
            lendBuffer();
        }

        /**
         * Creates the shared frame buffer and lends it to the application.
         *
         * <p>The buffer is the HOST's, and that is forced: an Android app
         * cannot map a descriptor it RECEIVED read-write. See XrHostService for
         * the three routes that were tried and what each one answered.
         */
        private void lendBuffer() {
            long total = (long) slots * slotSize;
            if (total > Integer.MAX_VALUE) {
                error(0, "buffer", "a " + total + "-byte frame buffer is beyond SharedMemory");
                return;
            }
            try {
                sm = android.os.SharedMemory.create("go-xrkit wall", (int) total);
                ByteBuffer m = sm.mapReadWrite();
                synchronized (pixelLock) {
                    shared = m;
                    seq = 0;
                    streaming = true;
                }
                android.os.Parcel parcel = android.os.Parcel.obtain();
                try {
                    sm.writeToParcel(parcel, 0);
                    parcel.setDataPosition(0);
                    ParcelFileDescriptor pfd = parcel.readFileDescriptor();
                    if (pfd == null) {
                        error(0, "buffer", "SharedMemory yielded no descriptor to lend");
                        return;
                    }
                    Enc e = new Enc();
                    e.i32(slots);
                    e.i64(slotSize);
                    sendWithFD(MSG_BUFFER, e.bytes(), pfd.getFileDescriptor());
                    pfd.close();
                } finally {
                    parcel.recycle();
                }
            } catch (android.system.ErrnoException | IOException | RuntimeException ex) {
                error(0, "buffer", "creating the shared frame buffer: " + ex);
            }
        }

        private void onImage(ImageReader r) {
            Image img = r.acquireLatestImage();
            if (img == null) {
                return;
            }
            try {
                if (!streaming) {
                    return;
                }
                Image.Plane p = img.getPlanes()[0];
                int stride = p.getRowStride();
                int w = img.getWidth(), h = img.getHeight();
                long need = (long) stride * (long) h;
                if (need > slotSize) {
                    sendStopped(STOP_SYSTEM, "the row stride " + stride + " does not fit the "
                            + slotSize + "-byte frame slot");
                    streaming = false;
                    return;
                }
                ByteBuffer src = p.getBuffer();
                long n;
                int slot;
                synchronized (pixelLock) {
                    if (shared == null) {
                        return;
                    }
                    slot = (int) (seq % slots);
                    seq++;
                    n = seq;
                    src.rewind();
                    int len = Math.min(src.remaining(), (int) need);
                    src.limit(len);
                    shared.position((int) (slot * slotSize));
                    shared.put(src);
                }
                Enc e = new Enc();
                e.i64(n);
                e.i32(slot);
                e.i32(w);
                e.i32(h);
                e.i32(stride);
                e.i64(System.currentTimeMillis() * 1_000_000L);
                send(MSG_FRAME, e.bytes());
            } catch (RuntimeException ex) {
                Log.e(TAG, "delivering a frame", ex);
            } finally {
                img.close();
            }
        }

        /** Releases everything this connection owns. Idempotent. */
        private void release() {
            streaming = false;
            if (presentation != null) {
                try {
                    presentation.dismiss();
                } catch (RuntimeException ignored) {
                    // The display may already be gone.
                }
                presentation = null;
            }
            if (vd != null) {
                vd.release();
                vd = null;
            }
            if (reader != null) {
                reader.setOnImageAvailableListener(null, null);
                reader.close();
                reader = null;
            }
            synchronized (pixelLock) {
                shared = null;
            }
            if (sm != null) {
                sm.close();
                sm = null;
            }
            if (counted) {
                counted = false;
                Log.i(TAG, "released a display, " + live.decrementAndGet() + " left");
            }
            try {
                sock.close();
            } catch (IOException ignored) {
                // Already gone.
            }
        }

        private void error(int code, String op, String detail) {
            Log.e(TAG, "error " + code + " " + op + ": " + detail);
            Enc e = new Enc();
            e.i32(code);
            e.str(op);
            e.str(detail);
            send(MSG_ERROR, e.bytes());
            release();
        }

        private void sendStopped(int reason, String detail) {
            Enc e = new Enc();
            e.i32(reason);
            e.str(detail);
            send(MSG_STOPPED, e.bytes());
        }

        private synchronized void send(int typ, byte[] body) {
            if (out == null) {
                return;
            }
            try {
                out.writeInt(body.length + 1);
                out.write(typ);
                out.write(body);
                out.flush();
            } catch (IOException e) {
                Log.i(TAG, "sending 0x" + Integer.toHexString(typ) + ": " + e);
                out = null;
            }
        }

        private synchronized void sendWithFD(int typ, byte[] body, FileDescriptor fd) {
            try {
                sock.setFileDescriptorsForSend(new FileDescriptor[] {fd});
                out.writeInt(body.length + 1);
                out.write(typ);
                out.write(body);
                out.flush();
                sock.setFileDescriptorsForSend(null);
            } catch (IOException e) {
                Log.e(TAG, "lending the frame buffer: " + e);
                out = null;
            }
        }
    }

    private static final int STRIDE_ALIGN = 256;

    private static int align(int n) {
        return ((n + STRIDE_ALIGN - 1) / STRIDE_ALIGN) * STRIDE_ALIGN;
    }

    private static boolean readFully(InputStream in, byte[] b, int n) throws IOException {
        int off = 0;
        while (off < n) {
            int r = in.read(b, off, n - off);
            if (r < 0) {
                return false;
            }
            off += r;
        }
        return true;
    }

    /**
     * The colours android.Sentinel promises, drawn so a consumer can SAMPLE a
     * frame rather than look at it. A feed that "works" and delivers a black
     * buffer is the classic silent failure of this whole mechanism, and flat
     * quadrants of known colour are the only way to catch it.
     */
    static final class SentinelView extends View {
        static final int BG = 0xFFFF0000;           // red
        static final int TOP_LEFT = 0xFF00FF00;     // green
        static final int BOTTOM_RIGHT = 0xFF0000FF; // blue
        private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final String label;

        SentinelView(Context c, String label) {
            super(c);
            this.label = label == null ? "" : label;
        }

        @Override
        protected void onDraw(Canvas c) {
            int w = getWidth(), h = getHeight();
            c.drawColor(BG);
            paint.setColor(TOP_LEFT);
            c.drawRect(0, 0, w / 2f, h / 2f, paint);
            paint.setColor(BOTTOM_RIGHT);
            c.drawRect(w / 2f, h / 2f, w, h, paint);
            if (!label.isEmpty()) {
                paint.setColor(Color.WHITE);
                paint.setTextSize(h / 8f);
                c.drawText(label, w / 2f - h / 8f, h / 2f, paint);
            }
        }
    }

    /** Big-endian encoder, matching protocol.go. */
    private static final class Enc {
        private byte[] b = new byte[64];
        private int n;

        private void need(int k) {
            if (n + k > b.length) {
                byte[] nb = new byte[Math.max(b.length * 2, n + k)];
                System.arraycopy(b, 0, nb, 0, n);
                b = nb;
            }
        }

        void i32(int v) {
            need(4);
            b[n++] = (byte) (v >> 24);
            b[n++] = (byte) (v >> 16);
            b[n++] = (byte) (v >> 8);
            b[n++] = (byte) v;
        }

        void i64(long v) {
            need(8);
            for (int i = 56; i >= 0; i -= 8) {
                b[n++] = (byte) (v >> i);
            }
        }

        /**
         * A length-prefixed string, with a <b>16-bit</b> length. That is what
         * protocol.go's appendString writes and takeString expects; a 32-bit
         * one here decodes as an empty string on the other side and loses the
         * detail of every error this host reports.
         */
        void str(String s) {
            byte[] u = s.getBytes(java.nio.charset.StandardCharsets.UTF_8);
            if (u.length > 0xffff) {
                byte[] cut = new byte[0xffff];
                System.arraycopy(u, 0, cut, 0, 0xffff);
                u = cut;
            }
            need(2);
            b[n++] = (byte) (u.length >> 8);
            b[n++] = (byte) u.length;
            need(u.length);
            System.arraycopy(u, 0, b, n, u.length);
            n += u.length;
        }

        byte[] bytes() {
            byte[] r = new byte[n];
            System.arraycopy(b, 0, r, 0, n);
            return r;
        }
    }

    /** Big-endian decoder, matching protocol.go. */
    private static final class Dec {
        private final byte[] b;
        private int n;

        Dec(byte[] b) {
            this.b = b;
        }

        int i32() {
            if (n + 4 > b.length) {
                return 0;
            }
            int v = ((b[n] & 0xff) << 24) | ((b[n + 1] & 0xff) << 16)
                    | ((b[n + 2] & 0xff) << 8) | (b[n + 3] & 0xff);
            n += 4;
            return v;
        }

        /** A 16-bit length prefix, matching protocol.go's appendString. */
        String str() {
            if (n + 2 > b.length) {
                return "";
            }
            int len = ((b[n] & 0xff) << 8) | (b[n + 1] & 0xff);
            n += 2;
            if (n + len > b.length) {
                return "";
            }
            String s = new String(b, n, len, java.nio.charset.StandardCharsets.UTF_8);
            n += len;
            return s;
        }
    }
}
