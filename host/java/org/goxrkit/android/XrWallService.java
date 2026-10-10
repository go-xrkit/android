// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Presentation;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Paint;
import android.graphics.PixelFormat;
import android.graphics.Rect;
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
 * The PRESENTATION host, in both directions.
 *
 * <p>Two things live here, and they are the same Android object pointed two
 * ways:
 *
 * <ul>
 *   <li>a <b>wall</b> panel — a display this application creates and owns,
 *       carrying a Presentation of content the host builds, read back through
 *       an ImageReader. The pixels come FROM Android, which is the whole
 *       reason: a WebView, a MediaCodec surface, a PdfRenderer are things a
 *       CGO-free Go process cannot render and an ordinary View can.
 *   <li>a <b>screen</b> — a Presentation on a display that already exists, the
 *       glasses on the USB-C port, carrying a Bitmap the application painted.
 *       The pixels go TO Android, because a headset is an output and what
 *       belongs on it is the ribbon the application composited.
 * </ul>
 *
 * <p>They share this service because they are one window kind with one
 * permission story — <b>none</b> — and splitting them would have been a third
 * copy of the same socket plumbing. What differs is which display carries the
 * Presentation and which way the pixels travel.
 *
 * <h2>And everything else an application may ask for FREE OF CHARGE</h2>
 *
 * This host also answers the <b>census</b>: what displays, cameras and USB
 * devices exist. Those are not Presentations, and they are here for a property
 * that is checkable rather than for convenience — <b>they need no permission,
 * no consent and no foreground service</b>, which is exactly what this service
 * is. Listing a camera needs no CAMERA permission; listing a USB device needs
 * no USB permission; a display list needs no MediaProjection. All three are
 * required to OPEN the thing, never to be told it is there.
 *
 * <p>So an application that wants to paint on the glasses and follow a head
 * can find out everything it needs to DECIDE without putting a single dialog in
 * front of anybody. The moment something has to be opened, it stops belonging
 * here.
 *
 * <p>It is a second service rather than part of {@link XrHostService} because
 * the two have nothing in common but a wire format. Capture needs a
 * MediaProjection, a consent dialog and — from API 34 — a mediaProjection
 * foreground service. <b>An owned display needs none of that: no permission at
 * all.</b> Keeping them apart means opening a ribbon panel cannot put a
 * "recording your screen" chip in the status bar, and a capture session ending
 * cannot take a panel with it.
 *
 * <p>It also serves <b>one connection per panel or screen</b>, where the capture
 * host serves exactly one connection in total. A Presentation's lifetime is its
 * socket's lifetime, so closing one cannot disturb another's frames, and a
 * client that dies has its windows released by the kernel closing its sockets.
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
    private static final int MSG_ERROR = 0x06, MSG_BUFFER = 0x07, MSG_PRESENTED = 0x08;
    private static final int MSG_OPEN_DISPLAY = 0x86, MSG_STOP = 0x84, MSG_BYE = 0x85;
    private static final int MSG_OPEN_SCREEN = 0x87, MSG_PRESENT = 0x88;
    private static final int MSG_DISPLAYS = 0x01, MSG_LIST_DISPLAYS = 0x81;
    private static final int MSG_CAMERAS = 0x09, MSG_USB_DEVICES = 0x0a;
    private static final int MSG_LIST_CAMERAS = 0x89, MSG_LIST_USB_DEVICES = 0x8a;
    private static final int MSG_USB_HANDLE = 0x0b, MSG_OPEN_USB_DEVICE = 0x8b;

    /** Matches android.ErrUSBPermissionDenied's code on the wire. */
    private static final int CODE_USB_PERMISSION_DENIED = 7;

    /** The action a USB permission result comes back on. */
    static final String ACTION_USB_PERMISSION = "org.goxrkit.android.USB_PERMISSION";

    private static final int STOP_SYSTEM = 1, STOP_APP = 2;
    private static final int CODE_NOT_FOUND = 3;
    private static final int CODE_TOO_MANY_DISPLAYS = 5;
    private static final int CODE_NOT_PRESENTABLE = 6;

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

    /** One connection, and therefore at most one Presentation: a panel or a screen. */
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

        /** The view a SCREEN paints into, and its bitmap. Null for a panel. */
        private FrameView frameView;
        private Bitmap bmp;
        /**
         * A screen's row stride, which is exactly frameW*4 and NOT the aligned
         * one a panel announces. The alignment exists because a GPU writes a
         * panel's surface; nothing writes a screen's buffer but the application,
         * and Bitmap.copyPixelsFromBuffer takes tightly packed rows or nothing.
         */
        private int screenStride;

        /**
         * The USB device this connection opened, held open for as long as the
         * application holds the descriptor: the platform drops the device when
         * this is closed, and the descriptor over there would then name nothing.
         */
        private android.hardware.usb.UsbDeviceConnection usb;

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
                case MSG_LIST_DISPLAYS:
                    handler.post(this::sendDisplays);
                    break;
                case MSG_LIST_CAMERAS:
                    handler.post(this::sendCameras);
                    break;
                case MSG_LIST_USB_DEVICES:
                    handler.post(this::sendUsbDevices);
                    break;
                case MSG_OPEN_USB_DEVICE:
                    handler.post(() -> openUsbDevice(new Dec(body).str()));
                    break;
                case MSG_OPEN_SCREEN:
                    handler.post(() -> openScreen(body));
                    break;
                case MSG_PRESENT:
                    handler.post(() -> present(body));
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

        /**
         * Answers what displays exist.
         *
         * <p>⛔ THE WALL HOST SERVES THIS TOO, and not for convenience: a
         * display list needs no MediaProjection, no consent and no foreground
         * service, so an application that only wants to paint on the glasses
         * must not have to start the capture host to FIND them. It did have to,
         * and the cost was the whole mediaProjection apparatus running for a
         * feature that uses none of it.
         */
        private void sendDisplays() {
            Display[] ds = getSystemService(DisplayManager.class).getDisplays();
            Enc e = new Enc();
            e.i32(ds.length);
            for (Display d : ds) {
                android.graphics.Point sz = new android.graphics.Point();
                d.getRealSize(sz);
                android.util.DisplayMetrics m = new android.util.DisplayMetrics();
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

        /**
         * Answers what cameras exist, including external USB ones.
         *
         * <p>⛔ NO PERMISSION IS ASKED FOR, AND NONE IS NEEDED.
         * android.permission.CAMERA is required to OPEN a camera, not to be
         * told one is there. A census that cost the user a dialog would be a
         * census nobody runs.
         *
         * <p>The answer that matters is whether anything comes back with
         * LENS_FACING_EXTERNAL: supporting external USB cameras is left to the
         * vendor's HAL, so whether a headset's camera is reachable through the
         * platform is a per-device measurement with no documentation that
         * settles it.
         */
        private void sendCameras() {
            Enc e = new Enc();
            android.hardware.camera2.CameraManager cm =
                    getSystemService(android.hardware.camera2.CameraManager.class);
            String[] ids;
            try {
                ids = cm.getCameraIdList();
            } catch (Throwable t) {
                error(0, "getCameraIdList", String.valueOf(t));
                return;
            }
            e.i32(ids.length);
            for (String id : ids) {
                int facing = -1, w = 0, h = 0;
                try {
                    android.hardware.camera2.CameraCharacteristics c =
                            cm.getCameraCharacteristics(id);
                    Integer f = c.get(android.hardware.camera2.CameraCharacteristics.LENS_FACING);
                    if (f != null) {
                        facing = f;
                    }
                    android.hardware.camera2.params.StreamConfigurationMap m = c.get(
                            android.hardware.camera2.CameraCharacteristics
                                    .SCALER_STREAM_CONFIGURATION_MAP);
                    if (m != null) {
                        android.util.Size[] sizes = m.getOutputSizes(android.graphics.ImageFormat.YUV_420_888);
                        if (sizes != null) {
                            for (android.util.Size s : sizes) {
                                if ((long) s.getWidth() * s.getHeight() > (long) w * h) {
                                    w = s.getWidth();
                                    h = s.getHeight();
                                }
                            }
                        }
                    }
                } catch (Throwable t) {
                    // A camera that will not describe itself is still a camera
                    // that is THERE, and saying so with zeroes beats dropping
                    // it from the list and reporting one fewer.
                    Log.w(TAG, "characteristics of camera " + id + ": " + t);
                }
                e.str(id);
                e.i32(facing);
                e.i32(w);
                e.i32(h);
            }
            send(MSG_CAMERAS, e.bytes());
        }

        /**
         * Answers what is attached to the USB host port, down to each
         * interface's endpoints.
         *
         * <p>The endpoints are the whole point. Android's Java USB API submits
         * control, bulk and interrupt transfers and <b>nothing else</b> — there
         * is no isochronous request in UsbDeviceConnection or UsbRequest — so
         * whether a UVC camera can be read through it comes down to whether its
         * streaming endpoints are bulk. Nothing but a census says.
         *
         * <p>Listing needs no permission either: a USB permission is granted
         * against a device in order to OPEN it, and the device list is public.
         */
        private void sendUsbDevices() {
            android.hardware.usb.UsbManager um =
                    getSystemService(android.hardware.usb.UsbManager.class);
            Enc e = new Enc();
            if (um == null) {
                // A phone with no USB host support at all. Zero devices is the
                // honest answer and is not the same as an error.
                Log.w(TAG, "no UsbManager: this device has no USB host support");
                e.i32(0);
                send(MSG_USB_DEVICES, e.bytes());
                return;
            }
            java.util.Collection<android.hardware.usb.UsbDevice> devs =
                    um.getDeviceList().values();
            e.i32(devs.size());
            for (android.hardware.usb.UsbDevice d : devs) {
                e.str(d.getDeviceName());
                e.i32(d.getVendorId());
                e.i32(d.getProductId());
                e.str(d.getManufacturerName() == null ? "" : d.getManufacturerName());
                e.str(d.getProductName() == null ? "" : d.getProductName());
                e.i32(d.getDeviceClass());
                e.i32(d.getDeviceSubclass());
                e.i32(d.getDeviceProtocol());
                e.i32(d.getInterfaceCount());
                for (int i = 0; i < d.getInterfaceCount(); i++) {
                    android.hardware.usb.UsbInterface in = d.getInterface(i);
                    e.i32(in.getId());
                    e.i32(in.getAlternateSetting());
                    e.i32(in.getInterfaceClass());
                    e.i32(in.getInterfaceSubclass());
                    e.i32(in.getInterfaceProtocol());
                    e.i32(in.getEndpointCount());
                    for (int j = 0; j < in.getEndpointCount(); j++) {
                        android.hardware.usb.UsbEndpoint ep = in.getEndpoint(j);
                        e.i32(ep.getAddress());
                        e.i32(ep.getAttributes());
                        e.i32(ep.getMaxPacketSize());
                        e.i32(ep.getInterval());
                    }
                }
            }
            send(MSG_USB_DEVICES, e.bytes());
        }

        /**
         * Opens one USB device, asking the user if it has not been allowed
         * before, and lends the kernel's descriptor to the application.
         *
         * <p>⛔ THIS IS THE ONE THING HERE THAT COSTS A CLICK. Everything else
         * this service answers needs no permission at all; opening a USB device
         * puts a system dialog in front of the user naming the device and this
         * application, and they may refuse. That is reported as a decision
         * rather than as a failure.
         *
         * <p>It exists because a headset's camera is reachable through NEITHER
         * Android camera API on this phone — no external camera2 device, and
         * every UVC streaming endpoint isochronous, which UsbDeviceConnection
         * cannot submit. The descriptor is the only remaining route, and
         * whether the kernel lets an untrusted application drive it is what the
         * application is about to find out.
         */
        private void openUsbDevice(String name) {
            android.hardware.usb.UsbManager um =
                    getSystemService(android.hardware.usb.UsbManager.class);
            if (um == null) {
                error(CODE_NOT_FOUND, "openUsbDevice", "this device has no USB host support");
                return;
            }
            android.hardware.usb.UsbDevice dev = um.getDeviceList().get(name);
            if (dev == null) {
                error(CODE_NOT_FOUND, "openUsbDevice",
                        "no USB device called " + name + " is attached any more");
                return;
            }
            Log.i(TAG, "openUsbDevice " + name + ": myUid=" + android.os.Process.myUid()
                    + " myPid=" + android.os.Process.myPid()
                    + " deviceName=" + dev.getDeviceName()
                    + " hasPermission=" + um.hasPermission(dev));
            if (um.hasPermission(dev)) {
                lendUsbDevice(um, dev);
                return;
            }
            // The answer comes back as a broadcast. The receiver unregisters
            // itself: a connection that asked twice would otherwise leave one
            // behind per attempt, and the second answer would be delivered to
            // both.
            android.content.BroadcastReceiver rx = new android.content.BroadcastReceiver() {
                @Override
                public void onReceive(Context c, Intent i) {
                    // ⛔ SAY WHAT ARRIVED, NOT WHAT IT MEANT. A broadcast with no
                    // extras and a broadcast carrying a refusal both read as
                    // "denied" through getBooleanExtra's default, and they are
                    // different failures: one is the user, the other is the
                    // request never reaching the system.
                    Log.i(TAG, "usb permission broadcast: action=" + i.getAction()
                            + " extras=" + i.getExtras()
                            + " granted=" + i.getBooleanExtra(
                                    android.hardware.usb.UsbManager.EXTRA_PERMISSION_GRANTED, false)
                            + " hasPermission=" + um.hasPermission(dev));
                    try {
                        c.unregisterReceiver(this);
                    } catch (RuntimeException ignored) {
                        // Already gone.
                    }
                    if (!i.getBooleanExtra(
                            android.hardware.usb.UsbManager.EXTRA_PERMISSION_GRANTED, false)) {
                        error(CODE_USB_PERMISSION_DENIED, "requestPermission",
                                "the permission broadcast for " + name + " came back not "
                                        + "granted -- which is the user refusing, OR the "
                                        + "request never reaching anybody");
                        return;
                    }
                    handler.post(() -> lendUsbDevice(um, dev));
                }
            };
            registerReceiver(rx, new android.content.IntentFilter(ACTION_USB_PERMISSION),
                    Context.RECEIVER_NOT_EXPORTED);
            // ⛔ AN ACTIVITY ASKS, NOT THIS SERVICE. requestPermission from here
            // answered DENIED IN EIGHT MILLISECONDS with no dialog shown: the
            // dialog is an activity, and a service asking for one is a background
            // activity launch, which the platform refuses and reports as a refusal
            // BY THE USER. Same shape as XrConsentActivity, same reason.
            startActivity(new Intent(XrWallService.this, XrUsbPermissionActivity.class)
                    .putExtra(XrUsbPermissionActivity.EXTRA_DEVICE_NAME, name)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_NO_ANIMATION));
        }

        /**
         * Hands the open device's descriptor over, and keeps the connection.
         *
         * <p>⛔ THE OWNERSHIP DANCE IS THE POINT. UsbDeviceConnection owns the
         * descriptor; SCM_RIGHTS gives the application its OWN copy of it, so
         * both sides can use it independently — but the wrapper used to put it
         * on the socket must not take ownership on the way past. adoptFd makes
         * a ParcelFileDescriptor that would close the connection's descriptor
         * when collected, so detachFd hands it straight back afterwards.
         *
         * <p>The connection itself is held until this panel is released: the
         * platform drops the device when it is closed, and the descriptor in
         * the application would then name nothing.
         */
        private void lendUsbDevice(android.hardware.usb.UsbManager um,
                android.hardware.usb.UsbDevice dev) {
            android.hardware.usb.UsbDeviceConnection c = um.openDevice(dev);
            if (c == null) {
                error(0, "openDevice", "the platform would not open " + dev.getDeviceName());
                return;
            }
            usb = c;
            android.os.ParcelFileDescriptor pfd =
                    android.os.ParcelFileDescriptor.adoptFd(c.getFileDescriptor());
            try {
                sendWithFD(MSG_USB_HANDLE, new byte[0], pfd.getFileDescriptor());
            } finally {
                pfd.detachFd();
            }
            Log.i(TAG, "lent the descriptor of " + dev.getDeviceName() + " "
                    + String.format("%04x:%04x", dev.getVendorId(), dev.getProductId()));
        }

        /**
         * Shows a Presentation on a display that ALREADY EXISTS and lends the
         * buffer the application will paint into.
         *
         * <p>Runs on the host's handler thread. Every Android object below
         * belongs to it, including the Presentation's view hierarchy, which is
         * why the frames are copied there too.
         */
        private void openScreen(byte[] body) {
            Dec d = new Dec(body);
            int displayId = d.i32(), w = d.i32(), h = d.i32(), wantSlots = d.i32();
            if (w <= 0 || h <= 0 || wantSlots <= 0) {
                error(0, "openScreen", "a " + w + "x" + h + " screen with " + wantSlots
                        + " slots is not a screen");
                return;
            }

            Display display = getSystemService(DisplayManager.class).getDisplay(displayId);
            if (display == null) {
                error(CODE_NOT_FOUND, "getDisplay", "there is no display " + displayId
                        + " any more; it was probably unplugged");
                return;
            }
            // The platform's own answer, asked again HERE rather than trusted
            // from the display list the application read: a headset can be
            // unplugged between the two, and this is the side that would throw.
            if ((display.getFlags() & Display.FLAG_PRESENTATION) == 0) {
                error(CODE_NOT_PRESENTABLE, "getDisplay", "display " + displayId + " \""
                        + display.getName() + "\" has no FLAG_PRESENTATION, so the platform "
                        + "will not take a presentation on it");
                return;
            }

            frameW = w;
            frameH = h;
            slots = wantSlots;
            screenStride = w * 4;
            try {
                bmp = Bitmap.createBitmap(w, h, Bitmap.Config.ARGB_8888);
            } catch (RuntimeException ex) {
                error(0, "createBitmap", "a " + w + "x" + h + " bitmap: " + ex);
                return;
            }
            frameView = new FrameView(createDisplayContext(display), bmp);
            try {
                Presentation p = new Presentation(XrWallService.this, display);
                p.setContentView(frameView, new ViewGroup.LayoutParams(
                        ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
                p.getWindow().setBackgroundDrawable(new ColorDrawable(Color.BLACK));
                p.getWindow().setLayout(ViewGroup.LayoutParams.MATCH_PARENT,
                        ViewGroup.LayoutParams.MATCH_PARENT);
                p.show();
                presentation = p;
            } catch (Throwable t) {
                error(0, "Presentation.show", String.valueOf(t));
                return;
            }
            Log.i(TAG, "screen on display " + displayId + " \"" + display.getName() + "\" "
                    + display.getMode().getPhysicalWidth() + "x"
                    + display.getMode().getPhysicalHeight() + ", painted at " + w + "x" + h);

            slotSize = (long) screenStride * (long) h;
            Enc e = new Enc();
            e.i32(w);
            e.i32(h);
            e.i32(screenStride);
            e.i32(PixelFormat.RGBA_8888);
            e.i32(slots);
            e.i64(slotSize);
            e.i32(displayId);
            send(MSG_CONFIG, e.bytes());
            lendBuffer();
        }

        /**
         * Puts one slot of the shared buffer on the screen, then frees it.
         *
         * <p>The acknowledgement goes out after the COPY, not after the draw:
         * the pixels are in the host's own bitmap by then and the application
         * may have the slot back. Waiting for the compositor instead would pace
         * the application against the panel's refresh with no buffering left,
         * which is the stutter this queue exists to avoid.
         */
        private void present(byte[] body) {
            Dec d = new Dec(body);
            long n = d.i64();
            int slot = d.i32(), w = d.i32(), h = d.i32(), stride = d.i32();
            // Read ONCE into a local: release() runs on the socket thread and
            // can null these while this runs on the handler thread.
            FrameView view = frameView;
            if (view == null) {
                error(0, "present", "this connection is not a screen");
                return;
            }
            // A slot number reaches us as an offset into shared memory, and a
            // geometry that does not match the one announced would read past
            // the slot. Both are refused rather than clamped: a clamp would
            // show a torn frame and say nothing.
            if (slot < 0 || slot >= slots || w != frameW || h != frameH || stride != screenStride) {
                error(0, "present", "frame " + n + " says slot " + slot + " " + w + "x" + h
                        + " stride " + stride + ", on a screen of " + slots + " slots of "
                        + frameW + "x" + frameH + " stride " + screenStride);
                return;
            }
            // The bitmap is recycled under this same lock, so holding it is what
            // keeps a copy from running into a freed one.
            synchronized (pixelLock) {
                if (shared == null || bmp == null) {
                    return;
                }
                shared.position((int) (slot * slotSize));
                shared.limit((int) (slot * slotSize + slotSize));
                try {
                    bmp.copyPixelsFromBuffer(shared);
                } finally {
                    shared.limit(shared.capacity());
                }
            }
            view.invalidate();
            Enc e = new Enc();
            e.i64(n);
            e.i32(slot);
            send(MSG_PRESENTED, e.bytes());
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
            if (usb != null) {
                usb.close();
                usb = null;
            }
            frameView = null;
            synchronized (pixelLock) {
                shared = null;
                if (bmp != null) {
                    // Recycled rather than dropped: a screen's bitmap is a
                    // whole framebuffer, and a ribbon opens and closes these as
                    // the glasses come and go. Under the lock, because present()
                    // runs on another thread and would copy into a freed one.
                    bmp.recycle();
                    bmp = null;
                }
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
    /**
     * The view a SCREEN paints into: one bitmap, scaled to fill the display.
     *
     * <p>It is a plain View rather than an ImageView because an ImageView
     * caches what it was handed and would have to be told the bitmap changed on
     * every frame. Here the bitmap is written in place and {@code invalidate()}
     * is the whole message.
     *
     * <p>FILTER_BITMAP is on: a ribbon composited at one size and shown at
     * another is the normal case, and nearest-neighbour on text is unreadable.
     */
    static final class FrameView extends View {
        private final Bitmap bmp;
        private final Paint paint = new Paint(Paint.FILTER_BITMAP_FLAG);
        private final Rect src = new Rect();
        private final Rect dst = new Rect();

        FrameView(Context c, Bitmap b) {
            super(c);
            this.bmp = b;
            setBackgroundColor(Color.BLACK);
            src.set(0, 0, b.getWidth(), b.getHeight());
        }

        @Override
        protected void onDraw(Canvas canvas) {
            if (bmp.isRecycled()) {
                return;
            }
            dst.set(0, 0, getWidth(), getHeight());
            canvas.drawBitmap(bmp, src, dst, paint);
        }
    }

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

        long i64() {
            if (n + 8 > b.length) {
                return 0;
            }
            long v = 0;
            for (int i = 0; i < 8; i++) {
                v = (v << 8) | (b[n + i] & 0xffL);
            }
            n += 8;
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
