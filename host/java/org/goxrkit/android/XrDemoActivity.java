// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.util.Log;
import android.widget.TextView;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.util.ArrayList;
import java.util.List;

/**
 * The standalone demo's Activity, and NOTHING an application needs.
 *
 * <p>In a real application the Activity is go-widgets/android's — it owns the
 * drawing surface, and there can only be one — and it spawns the single Go
 * process. This one exists so this repository's proof runs without pulling in a
 * widget toolkit: it starts {@link XrHostService} and spawns the Go binary
 * against it, which is all any host has to do.
 */
public final class XrDemoActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        TextView tv = new TextView(this);
        tv.setText("go-xrkit capture probe\n\nadb logcat -s xrcapture xr-host");
        tv.setTextSize(20);
        tv.setPadding(48, 200, 48, 48);
        setContentView(tv);

        // The wall host is an ordinary service: no projection, no consent, no
        // foreground requirement. It is started first and unconditionally,
        // because nothing can refuse it.
        startService(new Intent(this, XrWallService.class));

        // ⛔ startService, NOT startForegroundService. The capture host only
        // becomes a mediaProjection foreground service once consent exists --
        // see XrHostService.onStartCommand, and the Android 17 enforcement that
        // kills the whole process otherwise. startForegroundService would
        // promise a startForeground within five seconds that the host must not
        // make yet, and break the thing it is trying to protect.
        //
        // It is still started here, and unconditionally when capture is wanted,
        // because the display list and the consent request both travel over its
        // socket and neither needs any projection at all.
        if (getIntent().getBooleanExtra("capture", true)) {
            startService(new Intent(this, XrHostService.class));
        }
        try {
            spawn();
        } catch (IOException e) {
            Log.e(XrHostService.TAG, "cannot start the Go application", e);
        }
    }

    /**
     * Spawns the CGO-free Go executable shipped as {@code lib/<abi>/libxrapp.so}
     * — the app's native library directory is the one place an app may execute
     * from, and it is a plain PIE executable, not a shared library.
     */
    private void spawn() throws IOException {
        List<String> argv = new ArrayList<>();
        argv.add(getApplicationInfo().nativeLibraryDir + "/libxrapp.so");
        String args = getIntent().getStringExtra("args");
        if (args != null && !args.trim().isEmpty()) {
            for (String a : args.trim().split("\\s+")) {
                argv.add(a);
            }
        }
        ProcessBuilder pb = new ProcessBuilder(argv);
        // The Unix environment Android gives a spawned process and Go expects:
        // without HOME, os.UserHomeDir and os.UserConfigDir both fail outright.
        // HOME is also how the application derives the host's socket name when
        // nothing names one for it -- see android.DeriveSocket.
        pb.environment().put("HOME", getFilesDir().getAbsolutePath());
        pb.environment().put("XDG_CACHE_HOME", getCacheDir().getAbsolutePath());
        pb.environment().put("TMPDIR", getCacheDir().getAbsolutePath());
        pb.directory(getExternalFilesDir(null));
        pb.redirectErrorStream(true);
        Process p = pb.start();
        drain(p.getInputStream());
    }

    private static void drain(InputStream in) {
        new Thread(() -> {
            try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
                for (String line = r.readLine(); line != null; line = r.readLine()) {
                    Log.i("xrcapture", line);
                }
            } catch (IOException ignored) {
                // The application ended; there is nothing left to read.
            }
        }, "xr-drain").start();
    }
}
