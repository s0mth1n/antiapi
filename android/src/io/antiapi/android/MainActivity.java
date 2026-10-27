package io.antiapi.android;

import android.app.Activity;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.ServiceConnection;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.IBinder;
import android.view.View;
import android.widget.Button;
import android.widget.ScrollView;
import android.widget.TextView;

public class MainActivity extends Activity implements AntiApiService.LogListener {
    private TextView tvStatus;
    private TextView tvPublicUrl;
    private TextView tvLogs;
    private Button btnToggle;
    private Button btnCopyPublic;
    private Button btnDashboard;
    private ScrollView scrollLogs;

    private AntiApiService apiService;
    private boolean isBound = false;
    private String currentTunnelUrl = "";

    private final ServiceConnection connection = new ServiceConnection() {
        @Override
        public void onServiceConnected(ComponentName name, IBinder binder) {
            AntiApiService.LocalBinder localBinder = (AntiApiService.LocalBinder) binder;
            apiService = localBinder.getService();
            isBound = true;
            apiService.addListener(MainActivity.this);
        }

        @Override
        public void onServiceDisconnected(ComponentName name) {
            isBound = false;
            apiService = null;
        }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        tvStatus = findViewById(R.id.tv_status);
        tvPublicUrl = findViewById(R.id.tv_public_url);
        tvLogs = findViewById(R.id.tv_logs);
        btnToggle = findViewById(R.id.btn_toggle);
        btnCopyPublic = findViewById(R.id.btn_copy_public);
        btnDashboard = findViewById(R.id.btn_dashboard);
        scrollLogs = findViewById(R.id.scroll_logs);

        btnToggle.setOnClickListener(v -> {
            if (apiService != null && apiService.isRunning()) {
                Intent stopIntent = new Intent(this, AntiApiService.class);
                stopIntent.setAction(AntiApiService.ACTION_STOP);
                startService(stopIntent);
            } else {
                Intent startIntent = new Intent(this, AntiApiService.class);
                startIntent.setAction(AntiApiService.ACTION_START);
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                    startForegroundService(startIntent);
                } else {
                    startService(startIntent);
                }
            }
        });

        btnCopyPublic.setOnClickListener(v -> {
            if (!currentTunnelUrl.isEmpty()) {
                android.content.ClipboardManager cm = (android.content.ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
                if (cm != null) {
                    cm.setPrimaryClip(android.content.ClipData.newPlainText("AntiAPI Tunnel", currentTunnelUrl + "/v1"));
                    android.widget.Toast.makeText(this, "Copied: " + currentTunnelUrl + "/v1", android.widget.Toast.LENGTH_SHORT).show();
                }
            } else {
                android.widget.Toast.makeText(this, "Tunnel initializing...", android.widget.Toast.LENGTH_SHORT).show();
            }
        });

        btnDashboard.setOnClickListener(v -> {
            Intent browserIntent = new Intent(Intent.ACTION_VIEW, Uri.parse("http://127.0.0.1:8964/"));
            startActivity(browserIntent);
        });

        // Request notification permission on Android 13+
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 101);
        }
    }

    @Override
    protected void onStart() {
        super.onStart();
        Intent intent = new Intent(this, AntiApiService.class);
        bindService(intent, connection, Context.BIND_AUTO_CREATE);
    }

    @Override
    protected void onStop() {
        super.onStop();
        if (isBound && apiService != null) {
            apiService.removeListener(this);
            unbindService(connection);
            isBound = false;
        }
    }

    @Override
    public void onLog(final String line) {
        runOnUiThread(() -> {
            tvLogs.append(line + "\n");
            if (tvLogs.getText().length() > 8000) {
                String cur = tvLogs.getText().toString();
                tvLogs.setText(cur.substring(cur.length() - 4000));
            }
            scrollLogs.post(() -> scrollLogs.fullScroll(View.FOCUS_DOWN));
        });
    }

    @Override
    public void onStatusChanged(final boolean running) {
        runOnUiThread(() -> {
            if (running) {
                tvStatus.setText("Status: Running on :8964");
                tvStatus.setTextColor(Color.parseColor("#4CAF50"));
                btnToggle.setText("Stop Server");
                btnToggle.setBackgroundColor(Color.parseColor("#C62828"));
                btnDashboard.setEnabled(true);
            } else {
                tvStatus.setText("Status: Stopped");
                tvStatus.setTextColor(Color.parseColor("#FF5252"));
                btnToggle.setText("Start Server");
                btnToggle.setBackgroundColor(Color.parseColor("#2E7D32"));
                if (tvPublicUrl != null) {
                    tvPublicUrl.setText("Public Tunnel: Offline");
                    tvPublicUrl.setTextColor(Color.parseColor("#757575"));
                }
                currentTunnelUrl = "";
            }
        });
    }

    @Override
    public void onTunnelUrl(final String url) {
        runOnUiThread(() -> {
            currentTunnelUrl = url;
            if (tvPublicUrl != null) {
                tvPublicUrl.setText("Public Tunnel:\n" + url + "/v1");
                tvPublicUrl.setTextColor(Color.parseColor("#00E676"));
            }
        });
    }
}
