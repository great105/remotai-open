package com.tgcontrol.app;

import android.app.DownloadManager;
import android.content.Context;
import android.content.res.Configuration;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.view.View;
import android.view.Window;
import android.view.WindowManager;
import android.webkit.CookieManager;
import android.webkit.URLUtil;
import android.widget.Toast;

import androidx.core.view.WindowCompat;

import com.getcapacitor.BridgeActivity;
import java.util.Locale;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        // Edge-to-edge: WebView рисует под status/navigation bar — на планшете
        // даёт нативный полноэкранный вид. CSS env(safe-area-inset-*) учитывается.
        Window window = getWindow();
        WindowCompat.setDecorFitsSystemWindows(window, false);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            window.setStatusBarColor(0x00000000);
            window.setNavigationBarColor(0x00000000);
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            // Прозрачный nav-bar без серой подложки на API 27+.
            window.setNavigationBarContrastEnforced(false);
        }

        // Wire <a download> in WebView to Android DownloadManager so
        // tapping a file in DownloadSheet actually saves to /Download.
        getBridge().getWebView().setDownloadListener((url, userAgent, contentDisposition, mimetype, contentLength) ->
            getBridge().getWebView().evaluateJavascript("document.documentElement.lang", language -> {
            Configuration messageConfig = new Configuration(getResources().getConfiguration());
            messageConfig.setLocale("\"ru\"".equals(language) ? new Locale("ru") : Locale.ENGLISH);
            Context messages = createConfigurationContext(messageConfig);
            try {
                String filename = URLUtil.guessFileName(url, contentDisposition, mimetype);
                DownloadManager.Request request = new DownloadManager.Request(Uri.parse(url));
                request.setMimeType(mimetype);
                String cookies = CookieManager.getInstance().getCookie(url);
                if (cookies != null) request.addRequestHeader("cookie", cookies);
                if (userAgent != null) request.addRequestHeader("User-Agent", userAgent);
                request.setTitle(filename);
                request.setDescription("Remotai");
                request.allowScanningByMediaScanner();
                request.setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED);
                request.setDestinationInExternalPublicDir(Environment.DIRECTORY_DOWNLOADS, filename);
                DownloadManager dm = (DownloadManager) getSystemService(Context.DOWNLOAD_SERVICE);
                if (dm != null) {
                    dm.enqueue(request);
                    Toast.makeText(getApplicationContext(), messages.getString(R.string.download_started, filename), Toast.LENGTH_SHORT).show();
                } else {
                    Toast.makeText(getApplicationContext(), messages.getString(R.string.download_unavailable), Toast.LENGTH_SHORT).show();
                }
            } catch (Exception e) {
                Toast.makeText(getApplicationContext(), messages.getString(R.string.download_failed), Toast.LENGTH_LONG).show();
            }
        }));
    }
}
