// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.graphics.PixelFormat;
import android.graphics.Point;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.media.projection.MediaProjection;
import android.media.projection.MediaProjectionManager;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.IBinder;
import android.os.ParcelFileDescriptor;
import android.util.DisplayMetrics;
import android.util.Log;
import android.view.Display;

import java.io.DataOutputStream;
import java.io.FileDescriptor;
import java.io.IOException;
import java.io.InputStream;
import java.nio.ByteBuffer;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * The Java half of the go-xrkit Android capture host.
 *
 * <p>It owns the Android objects a CGO-free Go process cannot reach — the
 * MediaProjection token, the ImageReader, the VirtualDisplay that mirrors the
 * screen into it — and forwards bytes. It decides nothing: which display, which
 * size, which rate and when to stop are all the application's, arriving as
 * protocol messages.
 *
 * <p>It is a SERVICE, not an Activity, for two reasons. From API 34 a
 * MediaProjection token is only issued to a process already running a
 * foreground service of type {@code mediaProjection}, so the service is
 * mandatory anyway; and an APK may hold many services but only one Activity may
 * own the drawing surface — which, in a go-widgets application, is
 * go-widgets/android's. The two hosts coexist as two components of one package,
 * each owning what it must, and the single Go process talks to both.
 *
 * <p>Frames travel through a memfd the Go process creates and hands over as an
 * ancillary descriptor on the socket. Both ends map it, and each frame is one
 * native memcpy out of the ImageReader's plane into a slot — measured at 0.325
 * ms for a 1080x2400 frame. There is no zero-copy path: the plane is gralloc
 * memory, and mapping that needs the graphics HAL, which is behind cgo.
 */
public final class XrHostService extends Service {
    public static final String TAG = "xr-host";

    /** Names the abstract socket, matching the Go side's EnvSocket default. */
    private static final String SOCKET_SUFFIX = ".xr";

    static final String ACTION_CONSENT = "org.goxrkit.android.CONSENT";
    static final String EXTRA_CODE = "code";
    static final String EXTRA_DATA = "data";

    private static final String CHANNEL = "goxrkit.capture";
    private static final int NOTIFICATION = 0x5852;

    // Message types, matching protocol.go.
    private static final int MSG_DISPLAYS = 0x01, MSG_CONFIG = 0x02, MSG_FRAME = 0x03;
    private static final int MSG_STOPPED = 0x04, MSG_CONSENT = 0x05, MSG_ERROR = 0x06;
    private static final int MSG_BUFFER = 0x07;
    private static final int MSG_LIST_DISPLAYS = 0x81, MSG_CONSENT_REQUEST = 0x82;
    private static final int MSG_START = 0x83, MSG_STOP = 0x84, MSG_BYE = 0x85;

    private static final int STOP_USER = 0, STOP_SYSTEM = 1, STOP_APP = 2;

    // Error codes with a sentinel on the Go side; see client_linux.go.
    private static final int CODE_CONSENT_DENIED = 1, CODE_NO_DISPLAY = 2;
    private static final int CODE_NOT_FOUND = 3, CODE_NOT_CAPTURABLE = 4;

    /** The largest row padding the host will reserve when it cannot measure one. */
    private static final int STRIDE_ALIGN = 256;

    private LocalServerSocket server;
    private LocalSocket peer;
    private DataOutputStream out;
    private HandlerThread thread;
    private Handler handler;

    private MediaProjection projection;
    private int consentCode;
    private Intent consentData;
    private final Object consentLock = new Object();
    private LocalSocket consentWaiter;

    private ImageReader reader;
    private VirtualDisplay display;
    private ByteBuffer shared;      // the app's memfd, mapped read-write
    private int slots, announcedStride, frameW, frameH;
    private long slotSize;
    private long seq;
    private volatile boolean capturing;
    private final Object pixelLock = new Object();

    @Override
    public IBinder onBind(Intent i) {
        return null;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL,
                "Screen capture", NotificationManager.IMPORTANCE_LOW));
        thread = new HandlerThread("xr-host");
        thread.start();
        handler = new Handler(thread.getLooper());
        try {
            server = new LocalServerSocket(getPackageName() + SOCKET_SUFFIX);
        } catch (IOException e) {
            Log.e(TAG, "cannot listen on @" + getPackageName() + SOCKET_SUFFIX, e);
            stopSelf();
            return;
        }
        new Thread(this::accept, "xr-host-accept").start();
    }

    // ⛔⛔ THE FOREGROUND SERVICE IS STARTED ONLY ONCE CONSENT EXISTS. It used to
    // be started on every onStartCommand, and Android 17 (API 37) enforces what
    // 15 tolerated:
    //
    //   SecurityException: Starting FGS with type mediaProjection ... requires
    //   [FOREGROUND_SERVICE_MEDIA_PROJECTION] and any of [CAPTURE_VIDEO_OUTPUT,
    //   android:project_media] or Media projection screen capture permission
    //
    // And it does not refuse the call: it KILLS THE PROCESS, taking the wall
    // host down with it. Measured on a Pixel 11 Pro Fold.
    //
    // ⚠ WHICH LEFT A DEADLOCK, and that is the part worth keeping: the Go side
    // asks for consent THROUGH this host, so a host that cannot start without
    // consent can never be asked for it. Everything that needs no projection at
    // all -- the display list, which is how a pair of glasses is found -- was
    // unreachable with it.
    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        boolean granted = intent != null && ACTION_CONSENT.equals(intent.getAction())
                && intent.getIntExtra(EXTRA_CODE, Activity_RESULT_CANCELED) == Activity_RESULT_OK;
        if (granted) {
            Notification n = new Notification.Builder(this, CHANNEL)
                    // loadLabel, not getString(labelRes): a manifest label written
                    // as a literal has no resource id, and asking for id 0 is a
                    // fatal Resources$NotFoundException at the first frame.
                    .setContentTitle(getApplicationInfo().loadLabel(getPackageManager())
                            + " is capturing the screen")
                    .setSmallIcon(android.R.drawable.ic_menu_camera)
                    .setOngoing(true)
                    .build();
            startForeground(NOTIFICATION, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION);
        }
        if (intent != null && ACTION_CONSENT.equals(intent.getAction())) {
            onConsent(intent.getIntExtra(EXTRA_CODE, Activity_RESULT_CANCELED),
                    intent.getParcelableExtra(EXTRA_DATA, Intent.class));
        }
        return START_NOT_STICKY;
    }

    // android.app.Activity.RESULT_CANCELED, named rather than imported so this
    // service pulls in no Activity.
    private static final int Activity_RESULT_CANCELED = 0;
    private static final int Activity_RESULT_OK = -1;

    private void onConsent(int code, Intent data) {
        boolean granted = code == Activity_RESULT_OK && data != null;
        synchronized (consentLock) {
            if (granted) {
                consentCode = code;
                consentData = data;
            }
        }
        Log.i(TAG, "consent " + (granted ? "granted" : "refused"));
        send(MSG_CONSENT, new byte[] {(byte) (granted ? 1 : 0)});
    }

    /** Accepts the application's connection and pumps its messages until it ends. */
    private void accept() {
        try {
            peer = server.accept();
            out = new DataOutputStream(peer.getOutputStream());
            pump(peer.getInputStream());
        } catch (IOException e) {
            Log.w(TAG, "application connection ended: " + e);
        } finally {
            handler.post(this::stopCapture);
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

    private void handle(int typ, byte[] body) {
        switch (typ) {
            case MSG_LIST_DISPLAYS:
                sendDisplays();
                break;
            case MSG_CONSENT_REQUEST:
                requestConsent(body.length > 0 && body[0] != 0);
                break;
            case MSG_START:
                handler.post(() -> startCapture(body));
                break;
            case MSG_STOP:
            case MSG_BYE:
                handler.post(this::stopCapture);
                break;
            default:
                Log.w(TAG, "unknown message 0x" + Integer.toHexString(typ));
        }
    }

    // ---- displays -------------------------------------------------------

    private void sendDisplays() {
        Display[] ds = getSystemService(DisplayManager.class).getDisplays();
        Enc e = new Enc();
        e.i32(ds.length);
        for (Display d : ds) {
            Point sz = new Point();
            d.getRealSize(sz);
            DisplayMetrics m = new DisplayMetrics();
            d.getRealMetrics(m);
            e.i32(d.getDisplayId());
            e.str(d.getName());
            e.i32(sz.x);
            e.i32(sz.y);
            e.i32(m.densityDpi);
            e.i32(Math.round(d.getRefreshRate() * 1000f));
            e.i32(d.getFlags());
        }
        send(MSG_DISPLAYS, e.bytes());
    }

    // ---- consent --------------------------------------------------------

    private void requestConsent(boolean prompt) {
        boolean have;
        synchronized (consentLock) {
            have = consentData != null;
        }
        if (have || !prompt) {
            send(MSG_CONSENT, new byte[] {(byte) (have ? 1 : 0)});
            return;
        }
        Intent i = new Intent(this, XrConsentActivity.class)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_NO_ANIMATION);
        startActivity(i);
        // The answer arrives as ACTION_CONSENT and is sent from there.
    }

    // ---- capture --------------------------------------------------------

    private void startCapture(byte[] body) {
        if (body.length < 20) {
            error(0, "start", "truncated start message");
            return;
        }
        int displayId = i32(body, 0), w = i32(body, 4), h = i32(body, 8);
        int milliFps = i32(body, 12);
        int wantSlots = i32(body, 16);

        if (displayId != Display.DEFAULT_DISPLAY) {
            error(CODE_NOT_CAPTURABLE, "createVirtualDisplay",
                    "MediaProjection mirrors the default display only; display "
                            + displayId + " would need CAPTURE_VIDEO_OUTPUT");
            return;
        }
        Intent data;
        int code;
        synchronized (consentLock) {
            data = consentData;
            code = consentCode;
        }
        if (data == null) {
            error(CODE_CONSENT_DENIED, "getMediaProjection", "no projection token; ask for consent first");
            return;
        }
        Display d = getSystemService(DisplayManager.class).getDisplay(Display.DEFAULT_DISPLAY);
        if (d == null) {
            error(CODE_NO_DISPLAY, "getDisplay", "the system lists no default display");
            return;
        }
        DisplayMetrics m = new DisplayMetrics();
        d.getRealMetrics(m);
        if (w <= 0 || h <= 0) {
            Point sz = new Point();
            d.getRealSize(sz);
            w = sz.x;
            h = sz.y;
        }
        stopCapture();
        try {
            MediaProjectionManager mpm = getSystemService(MediaProjectionManager.class);
            projection = mpm.getMediaProjection(code, data);
            if (projection == null) {
                error(CODE_CONSENT_DENIED, "getMediaProjection", "the platform issued no token");
                return;
            }
            // A projection token is single-use: the platform will not issue a
            // second one from the same consent, so it is dropped here and the
            // application must ask again for the next session.
            synchronized (consentLock) {
                consentData = null;
            }
            // A callback is MANDATORY from API 34: createVirtualDisplay throws
            // without one. It is also how a user revoking the projection from
            // the status bar reaches the application at all.
            projection.registerCallback(new MediaProjection.Callback() {
                @Override
                public void onStop() {
                    handler.post(() -> {
                        sendStopped(STOP_USER, "the projection was revoked");
                        stopCapture();
                    });
                }
            }, handler);

            slots = Math.max(3, Math.min(8, wantSlots));
            reader = ImageReader.newInstance(w, h, PixelFormat.RGBA_8888, slots);
            frameW = w;
            frameH = h;

            // The exact row stride is a property of the buffer the graphics
            // allocator hands back, and nothing reports it before the first
            // Image exists. So the first one is waited for and MEASURED rather
            // than guessed; only if none arrives in time is an aligned upper
            // bound announced instead, and every later frame is checked against
            // it.
            final CountDownLatch first = new CountDownLatch(1);
            final int[] measured = {0};
            reader.setOnImageAvailableListener(r -> {
                if (first.getCount() > 0) {
                    try (Image img = r.acquireLatestImage()) {
                        if (img != null) {
                            measured[0] = img.getPlanes()[0].getRowStride();
                        }
                    }
                    first.countDown();
                    return;
                }
                onImage(r);
            }, handler);

            display = projection.createVirtualDisplay("go-xrkit capture", w, h, m.densityDpi,
                    DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR, reader.getSurface(), null, handler);
            if (display == null) {
                error(CODE_NOT_CAPTURABLE, "createVirtualDisplay", "the platform refused the mirror");
                stopCapture();
                return;
            }
            // The wait runs off the handler thread the listener posts to, so it
            // is done on a scratch thread rather than deadlocking against it.
            new Thread(() -> {
                boolean got = false;
                try {
                    got = first.await(2, TimeUnit.SECONDS);
                } catch (InterruptedException ignored) {
                    Thread.currentThread().interrupt();
                }
                int stride = got && measured[0] > 0 ? measured[0] : align(frameW * 4);
                announceConfig(stride, milliFps);
            }, "xr-host-stride").start();
        } catch (RuntimeException e) {
            error(0, "startCapture", String.valueOf(e));
            stopCapture();
        }
    }

    private static int align(int n) {
        return ((n + STRIDE_ALIGN - 1) / STRIDE_ALIGN) * STRIDE_ALIGN;
    }

    private void announceConfig(int stride, int milliFps) {
        announcedStride = stride;
        slotSize = (long) stride * (long) frameH;
        Enc e = new Enc();
        e.i32(frameW);
        e.i32(frameH);
        e.i32(stride);
        e.i32(PixelFormat.RGBA_8888);
        e.i32(slots);
        e.i64(slotSize);
        e.i32(Display.DEFAULT_DISPLAY);
        Log.i(TAG, "capture " + frameW + "x" + frameH + " stride " + stride
                + " slots " + slots + " ceiling " + (milliFps / 1000.0) + " fps");
        send(MSG_CONFIG, e.bytes());
        lendBuffer();
    }

    /**
     * Creates the shared frame buffer and lends it to the application.
     *
     * <p>The buffer is the HOST's, and that is forced rather than chosen. An
     * Android app cannot map a descriptor it RECEIVED read-write:
     * SharedMemory.fromFileDescriptor rejects a Go memfd outright
     * ("FileDescriptor is not a valid ashmem fd"), FileInputStream's and
     * FileOutputStream's channels are each open one way only, and reopening
     * through /proc/self/fd fails with EACCES. A region this process created
     * with SharedMemory.create is the one thing it can write, so it makes one
     * and hands the descriptor over.
     *
     * <p>Ashmem also dirties no page cache, so tens of megabytes a second of
     * pure scratch never reach flash.
     */
    private void lendBuffer() {
        long total = (long) slots * slotSize;
        if (total > Integer.MAX_VALUE) {
            error(0, "buffer", "a " + total + "-byte frame buffer is beyond SharedMemory");
            return;
        }
        try {
            android.os.SharedMemory sm = android.os.SharedMemory.create("go-xrkit capture", (int) total);
            ByteBuffer m = sm.mapReadWrite();
            synchronized (pixelLock) {
                shared = m;
                seq = 0;
                capturing = true;
            }
            // SharedMemory keeps its descriptor to itself, but writeToParcel is
            // public and writes exactly that descriptor, so a Parcel is the
            // supported way back to one.
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
            Log.i(TAG, "lent " + slots + " slots of " + slotSize + " bytes");
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
            if (!capturing) {
                return;
            }
            Image.Plane p = img.getPlanes()[0];
            int stride = p.getRowStride();
            int w = img.getWidth(), h = img.getHeight();
            long need = (long) stride * (long) h;
            if (need > slotSize) {
                // Only reachable when the stride could not be measured before
                // the config went out AND the allocator chose a wider one than
                // the aligned bound. Saying so is far better than a torn frame.
                sendStopped(STOP_SYSTEM, "the capture's row stride " + stride
                        + " does not fit the " + slotSize + "-byte frame slot");
                capturing = false;
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
                // Direct buffer to direct buffer: one native memcpy, no Java
                // heap in the middle and no allocation per frame.
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
        } catch (RuntimeException e) {
            Log.e(TAG, "frame failed", e);
        } finally {
            img.close();
        }
    }

    private void stopCapture() {
        capturing = false;
        if (display != null) {
            display.release();
            display = null;
        }
        if (reader != null) {
            reader.close();
            reader = null;
        }
        if (projection != null) {
            projection.stop();
            projection = null;
        }
        synchronized (pixelLock) {
            shared = null;
        }
    }

    private void sendStopped(int reason, String detail) {
        Enc e = new Enc();
        e.u8(reason);
        e.str(detail);
        send(MSG_STOPPED, e.bytes());
    }

    private void error(int code, String op, String detail) {
        Log.w(TAG, op + ": " + detail);
        Enc e = new Enc();
        e.i32(code);
        e.str(op);
        e.str(detail);
        send(MSG_ERROR, e.bytes());
    }

    /**
     * Sends one message with a descriptor attached. LocalSocket carries
     * ancillary descriptors on the SOCKET rather than on a write, so they are
     * armed, sent and disarmed around a single message under the same lock the
     * ordinary sends take.
     */
    private void sendWithFD(int typ, byte[] body, FileDescriptor fd) {
        DataOutputStream o = out;
        if (o == null) {
            return;
        }
        try {
            synchronized (o) {
                peer.setFileDescriptorsForSend(new FileDescriptor[] {fd});
                o.writeInt(body.length + 1);
                o.write(typ);
                o.write(body);
                o.flush();
                peer.setFileDescriptorsForSend(null);
            }
        } catch (IOException e) {
            Log.w(TAG, "lending the frame buffer: " + e);
        }
    }

    private void send(int typ, byte[] body) {
        DataOutputStream o = out;
        if (o == null) {
            return;
        }
        try {
            synchronized (o) {
                o.writeInt(body.length + 1);
                o.write(typ);
                o.write(body);
                o.flush();
            }
        } catch (IOException e) {
            Log.w(TAG, "sending 0x" + Integer.toHexString(typ) + ": " + e);
        }
    }

    @Override
    public void onDestroy() {
        stopCapture();
        try {
            if (server != null) {
                server.close();
            }
        } catch (IOException ignored) {
            // Closing a listener that is already gone is not a failure.
        }
        if (thread != null) {
            thread.quitSafely();
        }
        super.onDestroy();
    }

    // ---- encoding -------------------------------------------------------

    private static int i32(byte[] b, int off) {
        return ((b[off] & 0xff) << 24) | ((b[off + 1] & 0xff) << 16)
                | ((b[off + 2] & 0xff) << 8) | (b[off + 3] & 0xff);
    }

    private static long i64(byte[] b, int off) {
        long v = 0;
        for (int i = 0; i < 8; i++) {
            v = (v << 8) | (b[off + i] & 0xffL);
        }
        return v;
    }

    /** A big-endian byte builder, matching protocol.go's encoders. */
    private static final class Enc {
        private byte[] b = new byte[64];
        private int n;

        void need(int k) {
            if (n + k > b.length) {
                byte[] c = new byte[Math.max(b.length * 2, n + k)];
                System.arraycopy(b, 0, c, 0, n);
                b = c;
            }
        }

        void u8(int v) {
            need(1);
            b[n++] = (byte) v;
        }

        void i32(int v) {
            need(4);
            b[n++] = (byte) (v >>> 24);
            b[n++] = (byte) (v >>> 16);
            b[n++] = (byte) (v >>> 8);
            b[n++] = (byte) v;
        }

        void i64(long v) {
            need(8);
            for (int i = 7; i >= 0; i--) {
                b[n++] = (byte) (v >>> (i * 8));
            }
        }

        void str(String s) {
            byte[] u = s == null ? new byte[0] : s.getBytes(java.nio.charset.StandardCharsets.UTF_8);
            if (u.length > 0xffff) {
                byte[] c = new byte[0xffff];
                System.arraycopy(u, 0, c, 0, 0xffff);
                u = c;
            }
            need(2 + u.length);
            b[n++] = (byte) (u.length >>> 8);
            b[n++] = (byte) u.length;
            System.arraycopy(u, 0, b, n, u.length);
            n += u.length;
        }

        byte[] bytes() {
            byte[] c = new byte[n];
            System.arraycopy(b, 0, c, 0, n);
            return c;
        }
    }
}
