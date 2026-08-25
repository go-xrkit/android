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
        Intent i = new Intent(this, XrHostService.class)
                .setAction(XrHostService.ACTION_CONSENT)
                .putExtra(XrHostService.EXTRA_CODE, code);
        if (data != null) {
            i.putExtra(XrHostService.EXTRA_DATA, data);
        }
        // A foreground service: from API 34 the projection token is refused
        // unless a mediaProjection service is ALREADY running when it is asked
        // for, so the service must come up on the way in, not afterwards.
        startForegroundService(i);
        finish();
        overridePendingTransition(0, 0);
    }
}
