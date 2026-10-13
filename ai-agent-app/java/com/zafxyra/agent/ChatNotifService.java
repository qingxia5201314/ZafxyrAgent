package com.zafxyra.agent;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.os.IBinder;

public class ChatNotifService extends Service {

    private static final String CHANNEL = "aiagent";
    private static final int NOTIF_ID = 1;

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String url = "http://127.0.0.1:8787";
        if (intent != null && intent.getStringExtra("url") != null) {
            url = intent.getStringExtra("url");
        }
        startForeground(NOTIF_ID, buildNotification(url));
        return START_STICKY;
    }

    private Notification buildNotification(String url) {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        if (nm != null && nm.getNotificationChannel(CHANNEL) == null) {
            NotificationChannel ch = new NotificationChannel(CHANNEL, getString(R.string.channel_name),
                    NotificationManager.IMPORTANCE_HIGH);
            ch.setDescription(getString(R.string.channel_desc));
            nm.createNotificationChannel(ch);
        }

        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP | Intent.FLAG_ACTIVITY_SINGLE_TOP);
        open.putExtra("url", url);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);

        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat)
                .setContentTitle("ZafxyrAgent AI")
                .setContentText("Tap untuk buka chat")
                .setContentIntent(pi)
                .setOngoing(true)
                .setCategory(Notification.CATEGORY_SERVICE)
                .setShowWhen(false)
                .build();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
