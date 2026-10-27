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
    private static final int MAX_LOG_LINES = 150;
    private final List<String> logLines = new ArrayList<>();

    private String publicTunnelUrl = "";

    public interface LogListener {
        void onLog(String line);
        void onStatusChanged(boolean running);
        void onTunnelUrl(String url);
    }

    public class LocalBinder extends Binder {
        public AntiApiService getService() {
            return AntiApiService.this;
        }
    }

    public String getPublicTunnelUrl() {
        return publicTunnelUrl;
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

            File cfBin = new File(getApplicationInfo().nativeLibraryDir, "libcloudflared.so");
            List<String> cmd = new ArrayList<>();
            cmd.add(bin.getAbsolutePath());
            cmd.add("--no-tui");
            if (cfBin.exists()) {
                cmd.add("--tunnel");
            }

            ProcessBuilder pb = new ProcessBuilder(cmd);
            Map<String, String> env = pb.environment();
            env.put("HOME", getFilesDir().getAbsolutePath());
            env.put("SSL_CERT_DIR", "/system/etc/security/cacerts");
            env.put("ANTIAPI_STORAGE_PATH", new File(getFilesDir(), ".antigravity-api/accounts.json").getAbsolutePath());
            if (cfBin.exists()) {
                env.put("ANTIAPI_CLOUDFLARED_PATH", cfBin.getAbsolutePath());
                String curPath = System.getenv("PATH");
                env.put("PATH", getApplicationInfo().nativeLibraryDir + (curPath != null ? ":" + curPath : ""));
            }

            pb.redirectErrorStream(true);
            serverProcess = pb.start();
            isRunning = true;
            notifyStatus(true);
            updateNotification("AntiAPI running on :8964");
            appendLog("[System] AntiAPI process launched (PID: " + bin.getName() + ")");

            Thread pollThread = new Thread(() -> {
                while (isRunning && (publicTunnelUrl == null || publicTunnelUrl.isEmpty())) {
                    try {
                        Thread.sleep(2000);
                        java.net.HttpURLConnection conn = (java.net.HttpURLConnection) new java.net.URL("http://127.0.0.1:8964/quota").openConnection();
                        conn.setConnectTimeout(1000);
                        conn.setReadTimeout(1000);
                        if (conn.getResponseCode() == 200) {
                            try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                                String body = r.readLine();
                                if (body != null && body.contains("\"public_url\":\"https://") && !body.contains("api.trycloudflare.com")) {
                                    int s = body.indexOf("\"public_url\":\"") + 14;
                                    int e = body.indexOf("\"", s);
                                    if (e > s) {
                                        String resolved = body.substring(s, e);
                                        publicTunnelUrl = resolved;
                                        updateNotification("AntiAPI: " + resolved);
                                        notifyTunnelUrl(resolved);
                                        break;
                                    }
                                }
                            }
                        }
                    } catch (Exception ignored) {}
                }
            });
            pollThread.setDaemon(true);
            pollThread.start();

            try (BufferedReader reader = new BufferedReader(new InputStreamReader(serverProcess.getInputStream()))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    appendLog(line);
                    if (line.contains("trycloudflare.com") && !line.contains("api.trycloudflare.com")) {
                        int idx = line.indexOf("https://");
                        if (idx != -1) {
                            int end = line.indexOf(".trycloudflare.com", idx);
                            if (end != -1) {
                                final String u = line.substring(idx, end + ".trycloudflare.com".length());
                                if (!u.contains("api.trycloudflare.com")) {
                                    publicTunnelUrl = u;
                                    updateNotification("AntiAPI: " + u);
                                    notifyTunnelUrl(u);
                                }
                            }
                        }
                    }
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
        return new File(getApplicationInfo().nativeLibraryDir, "libantiapi.so");
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
        Log.i(TAG, line);
        if (logLines.size() >= MAX_LOG_LINES) {
            logLines.remove(0);
        }
        logLines.add(line);
        for (LogListener l : listeners) {
            l.onLog(line);
        }
    }

    private synchronized void notifyStatus(boolean running) {
        for (LogListener l : listeners) {
            l.onStatusChanged(running);
        }
    }

    private synchronized void notifyTunnelUrl(String url) {
        for (LogListener l : listeners) {
            l.onTunnelUrl(url);
        }
    }

    public synchronized void addListener(LogListener listener) {
        listeners.add(listener);
        listener.onStatusChanged(isRunning);
        if (publicTunnelUrl != null && !publicTunnelUrl.isEmpty()) {
            listener.onTunnelUrl(publicTunnelUrl);
        }
        if (!logLines.isEmpty()) {
            StringBuilder sb = new StringBuilder();
            for (String l : logLines) sb.append(l).append("\n");
            listener.onLog(sb.toString());
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
