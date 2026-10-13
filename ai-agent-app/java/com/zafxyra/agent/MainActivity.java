package com.zafxyra.agent;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Intent;
import android.os.Bundle;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;

public class MainActivity extends Activity {

    private static final String DEFAULT_URL = "http://127.0.0.1:8787";
    private WebView web;

    @Override
    protected void onCreate(Bundle bundle) {
        super.onCreate(bundle);

        String url = DEFAULT_URL;
        if (getIntent() != null && getIntent().getStringExtra("url") != null) {
            url = getIntent().getStringExtra("url");
        }

        if (getIntent() != null && getIntent().getBooleanExtra("boot", false)) {
            Intent svc = new Intent(this, ChatNotifService.class);
            svc.putExtra("url", url);
            startForegroundService(svc);
            finish();
            return;
        }

        WebView.setWebContentsDebuggingEnabled(true);

        web = new WebView(this);
        setContentView(web);

        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(true);

        web.setWebViewClient(new WebViewClient() {
            @Override
            public void onReceivedError(WebView view, int code, String desc, String failingUrl) {
                runOnUiThread(() -> new AlertDialog.Builder(MainActivity.this)
                        .setTitle("Daemon belum nyala")
                        .setMessage(failingUrl + "\n(" + desc + ")\nCoba reload setelah daemon aktif.")
                        .setPositiveButton("Reload", (d, x) -> view.reload())
                        .setNegativeButton("Tutup", (d, x) -> finish())
                        .show());
            }
        });
        web.setWebChromeClient(new WebChromeClient());
        web.loadUrl(url);
    }

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) {
            web.goBack();
        } else {
            super.onBackPressed();
        }
    }
}
