package io.antiapi.android;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.os.Binder;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

public class AntiApiService extends Service {
    private static final String TAG = "AntiApiService";
    private static final String CHANNEL_ID = "antiapi_foreground_channel";
    private static final int NOTIF_ID = 8964;

    public static final String ACTION_START = "io.antiapi.android.START";
    public static final String ACTION_STOP = "io.antiapi.android.STOP";

    private final IBinder binder = new LocalBinder();
    private PowerManager.WakeLock wakeLock;
    private Process serverProcess;
    private boolean isRunning = false;
    private final List<LogListener> listeners = new ArrayList<>();
    private final StringBuilder logBuffer = new StringBuilder();

    public interface LogListener {
        void onLog(String line);
        void onStatusChanged(boolean running);
    }

    public class LocalBinder extends Binder {
        public AntiApiService getService() {
            return AntiApiService.this;
        }
    }

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent != null ? intent.getAction() : null;
        if (ACTION_STOP.equals(action)) {
            stopServer();
            stopForeground(true);
            stopSelf();
            return START_NOT_STICKY;
        }

        startForeground(NOTIF_ID, buildNotification("Starting AntiAPI..."));
        startServer();
        return START_STICKY;
    }

    private synchronized void startServer() {
        if (isRunning) return;

        PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
        if (pm != null) {
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "AntiAPI::ServerWakeLock");
            wakeLock.acquire();
        }

        new Thread(this::runServerLoop, "AntiAPI-Server-Thread").start();
    }

    private void runServerLoop() {
        try {
            ensureCredentials();
            File bin = findExecutable();
            if (bin == null || !bin.exists()) {
                appendLog("[Error] Cannot locate antiapi binary.");
                stopSelf();
                return;
            }

            ProcessBuilder pb = new ProcessBuilder(bin.getAbsolutePath(), "--no-tui");
            Map<String, String> env = pb.environment();
            env.put("HOME", getFilesDir().getAbsolutePath());
            env.put("SSL_CERT_DIR", "/system/etc/security/cacerts");
            env.put("ANTIAPI_STORAGE_PATH", new File(getFilesDir(), ".antigravity-api/accounts.json").getAbsolutePath());

            pb.redirectErrorStream(true);
            serverProcess = pb.start();
            isRunning = true;
            notifyStatus(true);
            updateNotification("AntiAPI running on :8964");
            appendLog("[System] AntiAPI process launched (PID: " + bin.getName() + ")");

            try (BufferedReader reader = new BufferedReader(new InputStreamReader(serverProcess.getInputStream()))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    appendLog(line);
                }
            }

            int exitCode = serverProcess.waitFor();
            appendLog("[System] Server process exited with code " + exitCode);
        } catch (Exception e) {
            appendLog("[Error] " + e.getMessage());
            Log.e(TAG, "Process error", e);
        } finally {
            isRunning = false;
            notifyStatus(false);
            if (wakeLock != null && wakeLock.isHeld()) {
                wakeLock.release();
            }
            stopForeground(true);
            stopSelf();
        }
    }

    public synchronized void stopServer() {
        if (serverProcess != null) {
            try {
                serverProcess.destroy();
            } catch (Exception ignored) {}
            serverProcess = null;
        }
        isRunning = false;
        notifyStatus(false);
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
    }

    private File findExecutable() {
        File nativeLib = new File(getApplicationInfo().nativeLibraryDir, "libantiapi.so");
        if (nativeLib.exists() && nativeLib.canExecute()) {
            return nativeLib;
        }

        // Fallback: extract from assets
        File fallbackBin = new File(getFilesDir(), "antiapi");
        if (!fallbackBin.exists() || fallbackBin.length() == 0) {
            try (InputStream in = getAssets().open("antiapi");
                 OutputStream out = new FileOutputStream(fallbackBin)) {
                byte[] buf = new byte[8192];
                int read;
                while ((read = in.read(buf)) != -1) {
                    out.write(buf, 0, read);
                }
                fallbackBin.setExecutable(true, false);
            } catch (Exception e) {
                Log.w(TAG, "Fallback asset extraction: " + e.getMessage());
            }
        }
        fallbackBin.setExecutable(true, false);
        return fallbackBin.exists() ? fallbackBin : nativeLib;
    }

    private void ensureCredentials() {
        File dotDir = new File(getFilesDir(), ".antigravity-api");
        dotDir.mkdirs();
        File targetAccounts = new File(dotDir, "accounts.json");
        if (!targetAccounts.exists() || targetAccounts.length() == 0) {
            try (InputStream in = getAssets().open("accounts.json");
                 OutputStream out = new FileOutputStream(targetAccounts)) {
                byte[] buf = new byte[4096];
                int read;
                while ((read = in.read(buf)) != -1) {
                    out.write(buf, 0, read);
                }
                appendLog("[Auth] Seeded accounts.json from assets");
            } catch (Exception ignored) {
                // Not fatal; user can sign in via web dashboard
            }
        }
    }

    private void createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationChannel channel = new NotificationChannel(
                    CHANNEL_ID,
                    "AntiAPI Background Service",
                    NotificationManager.IMPORTANCE_LOW
            );
            channel.setDescription("Keeps AntiAPI server running in the background");
            NotificationManager nm = getSystemService(NotificationManager.class);
            if (nm != null) {
                nm.createNotificationChannel(channel);
            }
        }
    }

    private Notification buildNotification(String text) {
        Intent launchIntent = new Intent(this, MainActivity.class);
        PendingIntent pi = PendingIntent.getActivity(this, 0, launchIntent,
                PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);

        Notification.Builder builder;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            builder = new Notification.Builder(this, CHANNEL_ID);
        } else {
            builder = new Notification.Builder(this);
        }

        return builder
                .setContentTitle("AntiAPI Gateway")
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(pi)
                .setOngoing(true)
                .build();
    }

    private void updateNotification(String text) {
        NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (nm != null) {
            nm.notify(NOTIF_ID, buildNotification(text));
        }
    }

    private synchronized void appendLog(String line) {
        logBuffer.append(line).append("\n");
        for (LogListener l : listeners) {
            l.onLog(line);
        }
    }

    private synchronized void notifyStatus(boolean running) {
        for (LogListener l : listeners) {
            l.onStatusChanged(running);
        }
    }

    public synchronized void addListener(LogListener listener) {
        listeners.add(listener);
        listener.onStatusChanged(isRunning);
        if (logBuffer.length() > 0) {
            listener.onLog(logBuffer.toString());
        }
    }

    public synchronized void removeListener(LogListener listener) {
        listeners.remove(listener);
    }

    public boolean isRunning() {
        return isRunning;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return binder;
    }

    @Override
    public void onDestroy() {
        stopServer();
        super.onDestroy();
    }
}
