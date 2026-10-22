package com.zafxyra.agent;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Intent;
import android.graphics.Color;
import android.os.Build;
import android.os.Bundle;
import android.view.View;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;

public class MainActivity extends Activity {

    private static final String DEFAULT_URL = "http://127.0.0.1:8787";
    // 和 Web UI 的背景色一致，避免加载时白屏闪一下
    private static final int BG = 0xFF151517;

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

        // 状态栏 / 导航栏跟着深色主题走
        try {
            getWindow().setStatusBarColor(BG);
            getWindow().setNavigationBarColor(BG);
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                getWindow().setDecorFitsSystemWindows(true);
            }
            View decor = getWindow().getDecorView();
            decor.setSystemUiVisibility(0);
        } catch (Throwable ignored) {
            // 某些 ROM 会拒绝改系统栏颜色，不影响主流程
        }

        web = new WebView(this);
        web.setBackgroundColor(BG);
        setContentView(web);

        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(true);
        s.setTextZoom(100);
        s.setSupportZoom(false);

        web.setWebViewClient(new WebViewClient() {
            @Override
            public void onReceivedError(WebView view, int code, String desc, String failingUrl) {
                runOnUiThread(() -> new AlertDialog.Builder(MainActivity.this)
                        .setTitle("daemon 还没启动")
                        .setMessage(failingUrl + "\n(" + desc + ")\n\n"
                                + "后台服务可能还在启动，稍等一下再重试。")
                        .setPositiveButton("重试", (d, x) -> view.reload())
                        .setNegativeButton("关闭", (d, x) -> finish())
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
