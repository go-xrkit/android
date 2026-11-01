// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package org.goxrkit.android;

import android.app.Activity;
import android.content.Intent;
import android.media.projection.MediaProjectionManager;
import android.os.Bundle;
import android.util.Log;

/**
 * A transparent Activity whose only job is to hold Android's screen-capture
 * consent dialog and hand the answer to {@link XrHostService}.
 *
 * <p>It exists because consent is an ACTIVITY RESULT and nothing else: a
 * Service cannot ask, and the Go application owns no Activity at all. It is
 * also why capture does not live in the widget back-end's Activity — an APK may
 * hold many Activities, but only one of them may own the drawing surface, and
 * that one is go-widgets/android's.
 */
public final class XrConsentActivity extends Activity {
    private static final int REQ = 0x5852; // 'XR'

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        MediaProjectionManager mpm = getSystemService(MediaProjectionManager.class);
        try {
            startActivityForResult(mpm.createScreenCaptureIntent(), REQ);
        } catch (RuntimeException e) {
            Log.e(XrHostService.TAG, "cannot ask for screen-capture consent", e);
            answer(RESULT_CANCELED, null);
        }
    }

    @Override
    protected void onActivityResult(int req, int code, Intent data) {
        super.onActivityResult(req, code, data);
        answer(code, data);
    }

    private void answer(int code, Intent data) {
        // ⭐ SAY WHAT THE PLATFORM HANDED BACK. "Refused" is a verdict about a
        // PERSON, and this host printed it without ever naming the number it
        // read. On 2026-11-01 the wearer tapped Start, the host logged
        // `consent refused`, and nothing said whether that was RESULT_CANCELED,
        // a null Intent, or an answer that never arrived -- three different
        // defects wearing one sentence.
        Log.i(XrHostService.TAG, "consent result code=" + code
                + " (OK=" + RESULT_OK + ", CANCELED=" + RESULT_CANCELED + ")"
                + ", data=" + (data == null ? "null" : "present"));

        Intent i = new Intent(this, XrHostService.class)
                .setAction(XrHostService.ACTION_CONSENT)
                .putExtra(XrHostService.EXTRA_CODE, code);
        if (data != null) {
            i.putExtra(XrHostService.EXTRA_DATA, data);
        }
        if (code == RESULT_OK && data != null) {
            // A foreground service: from API 34 the projection token is refused
            // unless a mediaProjection service is ALREADY running when it is
            // asked for, so the service must come up on the way in, not
            // afterwards.
            startForegroundService(i);
        } else {
            // ⛔⛔ A REFUSAL MUST NOT BE A FOREGROUND START, AND IT KILLED THE
            // PROCESS WHEN IT WAS. XrHostService.onStartCommand calls
            // startForeground() only when consent was GRANTED -- it has to,
            // because starting an FGS of type mediaProjection without a
            // projection permission does not fail, it kills the process (see
            // the note there). So a refusal arrived through
            // startForegroundService, nothing ever went foreground, and thirty
            // seconds later the platform threw:
            //
            //   ForegroundServiceDidNotStartInTimeException:
            //   Context.startForegroundService() did not then call
            //   Service.startForeground()
            //
            // Measured on a Pixel 11 Pro Fold on 2026-11-01 at 11:48:54. It
            // takes the WHOLE process with it -- the wall host, the display
            // list, the application -- so one declined dialog ended a session
            // that had every right to carry on and simply go without capture.
            //
            // An ordinary start delivers a refusal perfectly well, and this
            // activity is in the foreground while it runs, so the start is
            // permitted.
            startService(i);
        }
        finish();
        overridePendingTransition(0, 0);
    }
}
