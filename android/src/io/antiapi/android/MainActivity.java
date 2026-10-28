package io.antiapi.android;

import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.ServiceConnection;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;

public class MainActivity extends Activity implements AntiApiService.LogListener {

    private static final int TAB_GATEWAY = 0;
    private static final int TAB_QUOTAS = 1;
    private static final int TAB_LOGS = 2;

    private int currentTab = TAB_GATEWAY;
    private AntiApiService apiService;
    private boolean isBound = false;
    private String currentTunnelUrl = "";

    private final Handler mainHandler = new Handler(Looper.getMainLooper());

    // Top Header Views
    private TextView tvHeaderStatus;
    private Button btnRefresh;
    private Button btnHeaderToggle;

    // Tab Bar Buttons
    private Button btnTabGateway;
    private Button btnTabQuotas;
    private Button btnTabLogs;

    // Tab Container Layouts
    private ScrollView layoutTabGateway;
    private ScrollView layoutTabQuotas;
    private LinearLayout layoutTabLogs;

    // Gateway Tab Views
    private TextView tvStatus;
    private TextView tvEndpoint;
    private Button btnCopyLocal;
    private TextView tvPublicUrl;
    private Button btnCopyPublic;
    private Button btnSharePublic;
    private Button btnCopyEnv;
    private Button btnCopyCurl;
    private Button btnToggle;

    // Quotas Tab Views
    private TextView tvAccountsSummary;
    private Button btnSyncQuotas;
    private LinearLayout llAccountChips;
    private TextView tvSelectedAccountEmail;
    private TextView tvSelectedAccountProject;
    private TextView tvSelectedAccountStatus;
    private Button btnRemoveAccount;
    private TextView tvQuotaCardTitle;
    private TextView tvQuotaCardSubtitle;
    private TextView tvGeminiWeeklyPct;
    private ProgressBar progressGeminiWeekly;
    private TextView tvGeminiWeeklyReset;
    private TextView tvGemini5hPct;
    private ProgressBar progressGemini5h;
    private TextView tvGemini5hReset;
    private Button btnOauthLogin;
    private android.widget.EditText etRefreshToken;
    private Button btnAddToken;

    // Account state & individual quotas
    private final java.util.List<String> accountList = new java.util.ArrayList<>();
    private final java.util.Map<String, JSONObject> accountQuotaMap = new java.util.HashMap<>();
    private final java.util.Map<String, JSONObject> accountMetaMap = new java.util.HashMap<>();
    private String currentSelectedAccount = "";

    // Logs Tab Views
    private TextView tvLogs;
    private ScrollView scrollLogs;
    private Button btnClearLogs;
    private Button btnCopyLogs;

    private final ServiceConnection connection = new ServiceConnection() {
        @Override
        public void onServiceConnected(ComponentName name, IBinder binder) {
            AntiApiService.LocalBinder localBinder = (AntiApiService.LocalBinder) binder;
            apiService = localBinder.getService();
            isBound = true;
            apiService.addListener(MainActivity.this);
            fetchQuotasAsync();
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

        setupWindowInsets();
        initViews();
        setupListeners();
        switchTab(TAB_GATEWAY);

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 101);
        }

        startDaemonService();
    }

    private void setupWindowInsets() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            getWindow().setStatusBarColor(Color.parseColor("#0D1015"));
            getWindow().setNavigationBarColor(Color.parseColor("#090B0E"));
        }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            android.view.WindowInsetsController controller = getWindow().getInsetsController();
            if (controller != null) {
                controller.setSystemBarsAppearance(0, android.view.WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS);
            }
        }

        View root = findViewById(R.id.root_layout);
        View statusBarSpacer = findViewById(R.id.status_bar_spacer);
        View navBarSpacer = findViewById(R.id.nav_bar_spacer);

        int initialSb = getStatusBarHeight();
        int initialNb = getNavigationBarHeight();
        if (statusBarSpacer != null && initialSb > 0) {
            statusBarSpacer.getLayoutParams().height = initialSb;
            statusBarSpacer.requestLayout();
        }
        if (navBarSpacer != null && initialNb > 0) {
            navBarSpacer.getLayoutParams().height = initialNb;
            navBarSpacer.requestLayout();
        }

        if (root != null) {
            root.setOnApplyWindowInsetsListener((v, insets) -> {
                int topInset = 0;
                int bottomInset = 0;

                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    android.graphics.Insets statusInsets = insets.getInsets(
                            android.view.WindowInsets.Type.statusBars() | android.view.WindowInsets.Type.displayCutout()
                    );
                    android.graphics.Insets navInsets = insets.getInsets(
                            android.view.WindowInsets.Type.navigationBars()
                    );
                    topInset = statusInsets.top;
                    bottomInset = navInsets.bottom;
                } else {
                    topInset = insets.getSystemWindowInsetTop();
                    bottomInset = insets.getSystemWindowInsetBottom();
                }

                if (topInset <= 0) topInset = getStatusBarHeight();
                if (bottomInset <= 0) bottomInset = getNavigationBarHeight();

                if (statusBarSpacer != null && topInset > 0) {
                    statusBarSpacer.getLayoutParams().height = topInset;
                    statusBarSpacer.requestLayout();
                }
                if (navBarSpacer != null && bottomInset > 0) {
                    navBarSpacer.getLayoutParams().height = bottomInset;
                    navBarSpacer.requestLayout();
                }
                return insets;
            });
            root.requestApplyInsets();
        }
    }

    private int getStatusBarHeight() {
        int result = 0;
        int resourceId = getResources().getIdentifier("status_bar_height", "dimen", "android");
        if (resourceId > 0) {
            result = getResources().getDimensionPixelSize(resourceId);
        }
        return result;
    }

    private int getNavigationBarHeight() {
        int result = 0;
        int resourceId = getResources().getIdentifier("navigation_bar_height", "dimen", "android");
        if (resourceId > 0) {
            result = getResources().getDimensionPixelSize(resourceId);
        }
        return result;
    }

    private void initViews() {
        tvHeaderStatus = findViewById(R.id.tv_header_status);
        btnRefresh = findViewById(R.id.btn_refresh);
        btnHeaderToggle = findViewById(R.id.btn_header_toggle);

        btnTabGateway = findViewById(R.id.btn_tab_gateway);
        btnTabQuotas = findViewById(R.id.btn_tab_quotas);
        btnTabLogs = findViewById(R.id.btn_tab_logs);

        layoutTabGateway = findViewById(R.id.layout_tab_gateway);
        layoutTabQuotas = findViewById(R.id.layout_tab_quotas);
        layoutTabLogs = findViewById(R.id.layout_tab_logs);

        tvStatus = findViewById(R.id.tv_status);
        tvEndpoint = findViewById(R.id.tv_endpoint);
        btnCopyLocal = findViewById(R.id.btn_copy_local);
        tvPublicUrl = findViewById(R.id.tv_public_url);
        btnCopyPublic = findViewById(R.id.btn_copy_public);
        btnSharePublic = findViewById(R.id.btn_share_public);
        btnCopyEnv = findViewById(R.id.btn_copy_env);
        btnCopyCurl = findViewById(R.id.btn_copy_curl);
        btnToggle = findViewById(R.id.btn_toggle);

        tvAccountsSummary = findViewById(R.id.tv_accounts_summary);
        btnSyncQuotas = findViewById(R.id.btn_sync_quotas);
        llAccountChips = findViewById(R.id.ll_account_chips);
        tvSelectedAccountEmail = findViewById(R.id.tv_selected_account_email);
        tvSelectedAccountProject = findViewById(R.id.tv_selected_account_project);
        tvSelectedAccountStatus = findViewById(R.id.tv_selected_account_status);
        btnRemoveAccount = findViewById(R.id.btn_remove_account);
        tvQuotaCardTitle = findViewById(R.id.tv_quota_card_title);
        tvQuotaCardSubtitle = findViewById(R.id.tv_quota_card_subtitle);
        tvGeminiWeeklyPct = findViewById(R.id.tv_gemini_weekly_pct);
        progressGeminiWeekly = findViewById(R.id.progress_gemini_weekly);
        tvGeminiWeeklyReset = findViewById(R.id.tv_gemini_weekly_reset);
        tvGemini5hPct = findViewById(R.id.tv_gemini_5h_pct);
        progressGemini5h = findViewById(R.id.progress_gemini_5h);
        tvGemini5hReset = findViewById(R.id.tv_gemini_5h_reset);
        btnOauthLogin = findViewById(R.id.btn_oauth_login);
        etRefreshToken = findViewById(R.id.et_refresh_token);
        btnAddToken = findViewById(R.id.btn_add_token);

        tvLogs = findViewById(R.id.tv_logs);
        scrollLogs = findViewById(R.id.scroll_logs);
        btnClearLogs = findViewById(R.id.btn_clear_logs);
        btnCopyLogs = findViewById(R.id.btn_copy_logs);
    }

    private void setupListeners() {
        btnTabGateway.setOnClickListener(v -> switchTab(TAB_GATEWAY));
        btnTabQuotas.setOnClickListener(v -> {
            switchTab(TAB_QUOTAS);
            fetchQuotasAsync();
        });
        btnTabLogs.setOnClickListener(v -> switchTab(TAB_LOGS));

        btnHeaderToggle.setOnClickListener(v -> toggleServer());
        btnToggle.setOnClickListener(v -> toggleServer());

        btnRefresh.setOnClickListener(v -> {
            fetchQuotasAsync();
            Toast.makeText(this, "Telemetry synchronized", Toast.LENGTH_SHORT).show();
        });

        btnSyncQuotas.setOnClickListener(v -> fetchQuotasAsync());

        btnOauthLogin.setOnClickListener(v -> {
            try {
                Intent browserIntent = new Intent(Intent.ACTION_VIEW, Uri.parse("http://127.0.0.1:8964/oauth/login"));
                startActivity(browserIntent);
                Toast.makeText(this, "Opening Google sign-in in browser...", Toast.LENGTH_SHORT).show();
            } catch (Exception e) {
                Toast.makeText(this, "Could not open browser: " + e.getMessage(), Toast.LENGTH_SHORT).show();
            }
        });

        btnAddToken.setOnClickListener(v -> {
            String token = etRefreshToken.getText().toString().trim();
            if (token.isEmpty()) {
                Toast.makeText(this, "Please enter a refresh token", Toast.LENGTH_SHORT).show();
                return;
            }
            addAccountAsync(token);
        });

        btnRemoveAccount.setOnClickListener(v -> {
            if (currentSelectedAccount == null || currentSelectedAccount.isEmpty()) return;
            removeAccountAsync(currentSelectedAccount);
        });

        btnCopyLocal.setOnClickListener(v -> copyToClipboard("Local Base URL", "http://127.0.0.1:8964/v1"));

        btnCopyPublic.setOnClickListener(v -> {
            if (!currentTunnelUrl.isEmpty()) {
                copyToClipboard("Public Tunnel URL", currentTunnelUrl + "/v1");
            } else {
                Toast.makeText(this, "Tunnel connecting...", Toast.LENGTH_SHORT).show();
            }
        });

        btnSharePublic.setOnClickListener(v -> {
            if (!currentTunnelUrl.isEmpty()) {
                shareEndpoint(currentTunnelUrl + "/v1");
            } else {
                Toast.makeText(this, "Tunnel connecting...", Toast.LENGTH_SHORT).show();
            }
        });

        btnCopyEnv.setOnClickListener(v -> {
            String target = currentTunnelUrl.isEmpty() ? "http://127.0.0.1:8964/v1" : currentTunnelUrl + "/v1";
            String env = "export OPENAI_BASE_URL=\"" + target + "\"\nexport OPENAI_API_KEY=\"antiapi\"";
            copyToClipboard("Client Environment Variables", env);
        });

        btnCopyCurl.setOnClickListener(v -> {
            String target = currentTunnelUrl.isEmpty() ? "http://127.0.0.1:8964/v1" : currentTunnelUrl + "/v1";
            String curl = "curl -s \"" + target + "/models\"";
            copyToClipboard("cURL Command", curl);
        });

        btnClearLogs.setOnClickListener(v -> {
            tvLogs.setText("");
            Toast.makeText(this, "Logs cleared", Toast.LENGTH_SHORT).show();
        });

        btnCopyLogs.setOnClickListener(v -> {
            String logs = tvLogs.getText().toString();
            if (!logs.isEmpty()) {
                copyToClipboard("AntiAPI Logs", logs);
            } else {
                Toast.makeText(this, "Log buffer empty", Toast.LENGTH_SHORT).show();
            }
        });
    }

    private void switchTab(int tab) {
        currentTab = tab;

        layoutTabGateway.setVisibility(tab == TAB_GATEWAY ? View.VISIBLE : View.GONE);
        layoutTabQuotas.setVisibility(tab == TAB_QUOTAS ? View.VISIBLE : View.GONE);
        layoutTabLogs.setVisibility(tab == TAB_LOGS ? View.VISIBLE : View.GONE);

        updateTabButton(btnTabGateway, tab == TAB_GATEWAY);
        updateTabButton(btnTabQuotas, tab == TAB_QUOTAS);
        updateTabButton(btnTabLogs, tab == TAB_LOGS);
    }

    private void updateTabButton(Button btn, boolean selected) {
        if (selected) {
            btn.setBackgroundResource(R.drawable.tab_active_bg);
            btn.setTextColor(Color.parseColor("#F8FAFC"));
        } else {
            btn.setBackgroundResource(R.drawable.tab_inactive_bg);
            btn.setTextColor(Color.parseColor("#94A3B8"));
        }
    }

    private void fetchQuotasAsync() {
        new Thread(() -> {
            try {
                URL statusUrl = new URL("http://127.0.0.1:8964/api/status");
                HttpURLConnection conn = (HttpURLConnection) statusUrl.openConnection();
                conn.setConnectTimeout(2000);
                conn.setReadTimeout(2000);

                if (conn.getResponseCode() != 200) return;

                BufferedReader br = new BufferedReader(new InputStreamReader(conn.getInputStream()));
                StringBuilder sb = new StringBuilder();
                String line;
                while ((line = br.readLine()) != null) {
                    sb.append(line);
                }
                br.close();

                JSONObject statusJson = new JSONObject(sb.toString());
                JSONArray accountsArr = statusJson.optJSONArray("accounts");
                String activeAccount = statusJson.optString("selected_account", "");
                JSONObject quotaSummaries = statusJson.optJSONObject("quota_summaries");

                java.util.Map<String, JSONObject> metaMap = new java.util.HashMap<>();
                try {
                    URL accUrl = new URL("http://127.0.0.1:8964/api/accounts");
                    HttpURLConnection accConn = (HttpURLConnection) accUrl.openConnection();
                    accConn.setConnectTimeout(2000);
                    accConn.setReadTimeout(2000);
                    if (accConn.getResponseCode() == 200) {
                        BufferedReader accBr = new BufferedReader(new InputStreamReader(accConn.getInputStream()));
                        StringBuilder accSb = new StringBuilder();
                        String aLine;
                        while ((aLine = accBr.readLine()) != null) {
                            accSb.append(aLine);
                        }
                        accBr.close();
                        JSONObject accsObj = new JSONObject(accSb.toString());
                        JSONArray rawAccs = accsObj.optJSONArray("accounts");
                        if (rawAccs != null) {
                            for (int i = 0; i < rawAccs.length(); i++) {
                                JSONObject a = rawAccs.getJSONObject(i);
                                metaMap.put(a.optString("email", ""), a);
                            }
                        }
                    }
                } catch (Exception ignored) {}

                final java.util.List<String> newAccounts = new java.util.ArrayList<>();
                final java.util.Map<String, JSONObject> newQuotaMap = new java.util.HashMap<>();
                if (accountsArr != null) {
                    for (int i = 0; i < accountsArr.length(); i++) {
                        String em = accountsArr.getString(i);
                        newAccounts.add(em);
                        if (quotaSummaries != null) {
                            JSONObject q = quotaSummaries.optJSONObject(em);
                            if (q != null) newQuotaMap.put(em, q);
                        }
                    }
                }

                runOnUiThread(() -> {
                    accountList.clear();
                    accountList.addAll(newAccounts);
                    accountQuotaMap.clear();
                    accountQuotaMap.putAll(newQuotaMap);
                    accountMetaMap.clear();
                    accountMetaMap.putAll(metaMap);

                    if (tvAccountsSummary != null) {
                        tvAccountsSummary.setText(accountList.size() + " account(s) loaded · Tap account to view individual quota");
                    }

                    renderAccountChips();

                    if (currentSelectedAccount.isEmpty() || !accountList.contains(currentSelectedAccount)) {
                        if (!activeAccount.isEmpty() && accountList.contains(activeAccount)) {
                            currentSelectedAccount = activeAccount;
                        } else if (!accountList.isEmpty()) {
                            currentSelectedAccount = accountList.get(0);
                        } else {
                            currentSelectedAccount = "";
                        }
                    }

                    if (!currentSelectedAccount.isEmpty()) {
                        selectAccount(currentSelectedAccount);
                    } else {
                        if (tvSelectedAccountEmail != null) tvSelectedAccountEmail.setText("No accounts configured");
                        if (tvSelectedAccountProject != null) tvSelectedAccountProject.setText("Add an account below to begin");
                        if (tvSelectedAccountStatus != null) tvSelectedAccountStatus.setText("Status: Offline");
                    }
                });

            } catch (Exception ignored) {}
        }).start();
    }

    private void renderAccountChips() {
        if (llAccountChips == null) return;
        llAccountChips.removeAllViews();

        for (final String email : accountList) {
            Button chip = new Button(this);
            chip.setText(email);
            chip.setTextSize(11f);
            chip.setAllCaps(false);
            chip.setSingleLine(true);
            chip.setPadding(dpToPx(12), dpToPx(4), dpToPx(12), dpToPx(4));

            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.WRAP_CONTENT,
                    dpToPx(32)
            );
            lp.setMargins(0, 0, dpToPx(8), 0);
            chip.setLayoutParams(lp);

            boolean isSelected = email.equals(currentSelectedAccount);
            chip.setBackgroundResource(isSelected ? R.drawable.chip_active_bg : R.drawable.chip_inactive_bg);
            chip.setTextColor(isSelected ? Color.parseColor("#38BDF8") : Color.parseColor("#94A3B8"));

            chip.setOnClickListener(v -> selectAccount(email));
            llAccountChips.addView(chip);
        }
    }

    private void selectAccount(String email) {
        currentSelectedAccount = email;
        renderAccountChips();

        if (tvSelectedAccountEmail != null) {
            tvSelectedAccountEmail.setText(email);
        }

        JSONObject meta = accountMetaMap.get(email);
        String project = (meta != null) ? meta.optString("projectId", "--") : "--";
        boolean rateLimited = (meta != null) && meta.optBoolean("isRateLimited", false);

        if (tvSelectedAccountProject != null) {
            tvSelectedAccountProject.setText("Project: " + (project.isEmpty() ? "Default" : project));
        }

        if (tvSelectedAccountStatus != null) {
            if (rateLimited) {
                String until = meta.optString("rateLimitedUntil", "");
                tvSelectedAccountStatus.setText("Status: Rate Limited (" + until + ")");
                tvSelectedAccountStatus.setTextColor(Color.parseColor("#EF4444"));
            } else {
                tvSelectedAccountStatus.setText("Status: Active · Ready");
                tvSelectedAccountStatus.setTextColor(Color.parseColor("#10B981"));
            }
        }

        if (tvQuotaCardSubtitle != null) {
            tvQuotaCardSubtitle.setText("Individual upstream quota for " + email);
        }

        double geminiWeekly = 100.0;
        String geminiWeeklyReset = "--";
        double gemini5h = 100.0;
        String gemini5hReset = "--";

        JSONObject sum = accountQuotaMap.get(email);
        if (sum != null) {
            JSONArray groups = sum.optJSONArray("groups");
            if (groups != null) {
                for (int i = 0; i < groups.length(); i++) {
                    JSONObject grp = groups.optJSONObject(i);
                    if (grp == null) continue;
                    String name = grp.optString("displayName", "");
                    if (name.contains("Gemini")) {
                        JSONArray buckets = grp.optJSONArray("buckets");
                        if (buckets == null) continue;
                        for (int j = 0; j < buckets.length(); j++) {
                            JSONObject b = buckets.optJSONObject(j);
                            if (b == null) continue;
                            String window = b.optString("window", "");
                            double fraction = b.optDouble("remainingFraction", 1.0) * 100.0;
                            String resetTime = b.optString("resetTime", "");
                            if ("weekly".equalsIgnoreCase(window)) {
                                geminiWeekly = fraction;
                                geminiWeeklyReset = formatResetTime(resetTime);
                            } else if ("5h".equalsIgnoreCase(window)) {
                                gemini5h = fraction;
                                gemini5hReset = formatResetTime(resetTime);
                            }
                        }
                    }
                }
            }
        }

        if (tvGeminiWeeklyPct != null) {
            tvGeminiWeeklyPct.setText(String.format("%.1f%%", geminiWeekly));
            progressGeminiWeekly.setProgress((int) Math.round(geminiWeekly));
        }
        if (tvGeminiWeeklyReset != null) {
            tvGeminiWeeklyReset.setText("Reset: " + geminiWeeklyReset);
        }

        if (tvGemini5hPct != null) {
            tvGemini5hPct.setText(String.format("%.1f%%", gemini5h));
            progressGemini5h.setProgress((int) Math.round(gemini5h));
        }
        if (tvGemini5hReset != null) {
            tvGemini5hReset.setText("Reset: " + gemini5hReset);
        }
    }

    private void addAccountAsync(String token) {
        new Thread(() -> {
            try {
                URL url = new URL("http://127.0.0.1:8964/api/accounts/add");
                HttpURLConnection conn = (HttpURLConnection) url.openConnection();
                conn.setRequestMethod("POST");
                conn.setRequestProperty("Content-Type", "application/json");
                conn.setDoOutput(true);
                conn.setConnectTimeout(3000);
                conn.setReadTimeout(3000);

                JSONObject payload = new JSONObject();
                payload.put("refreshToken", token);
                byte[] bytes = payload.toString().getBytes("UTF-8");
                conn.getOutputStream().write(bytes);

                int code = conn.getResponseCode();
                if (code == 200) {
                    runOnUiThread(() -> {
                        etRefreshToken.setText("");
                        Toast.makeText(MainActivity.this, "Account added successfully", Toast.LENGTH_SHORT).show();
                        fetchQuotasAsync();
                    });
                } else {
                    runOnUiThread(() -> Toast.makeText(MainActivity.this, "Failed to add account (" + code + ")", Toast.LENGTH_SHORT).show());
                }
            } catch (Exception e) {
                runOnUiThread(() -> Toast.makeText(MainActivity.this, "Error adding account: " + e.getMessage(), Toast.LENGTH_SHORT).show());
            }
        }).start();
    }

    private void removeAccountAsync(String email) {
        new Thread(() -> {
            try {
                URL url = new URL("http://127.0.0.1:8964/api/accounts/remove");
                HttpURLConnection conn = (HttpURLConnection) url.openConnection();
                conn.setRequestMethod("POST");
                conn.setRequestProperty("Content-Type", "application/json");
                conn.setDoOutput(true);
                conn.setConnectTimeout(3000);
                conn.setReadTimeout(3000);

                JSONObject payload = new JSONObject();
                payload.put("email", email);
                byte[] bytes = payload.toString().getBytes("UTF-8");
                conn.getOutputStream().write(bytes);

                int code = conn.getResponseCode();
                if (code == 200) {
                    runOnUiThread(() -> {
                        Toast.makeText(MainActivity.this, "Removed " + email, Toast.LENGTH_SHORT).show();
                        fetchQuotasAsync();
                    });
                } else {
                    runOnUiThread(() -> Toast.makeText(MainActivity.this, "Failed to remove account (" + code + ")", Toast.LENGTH_SHORT).show());
                }
            } catch (Exception e) {
                runOnUiThread(() -> Toast.makeText(MainActivity.this, "Error removing account: " + e.getMessage(), Toast.LENGTH_SHORT).show());
            }
        }).start();
    }

    private int dpToPx(int dp) {
        return (int) (dp * getResources().getDisplayMetrics().density + 0.5f);
    }

    private String formatResetTime(String iso) {
        if (iso == null || iso.isEmpty()) return "--";
        try {
            java.time.Instant instant = java.time.Instant.parse(iso);
            java.time.Duration dur = java.time.Duration.between(java.time.Instant.now(), instant);
            if (dur.isNegative()) return "Immediate";
            long hours = dur.toHours();
            long minutes = dur.toMinutes() % 60;
            if (hours > 24) {
                long days = hours / 24;
                return "in " + days + "d " + (hours % 24) + "h";
            }
            if (hours > 0) {
                return "in " + hours + "h " + minutes + "m";
            }
            return "in " + minutes + "m";
        } catch (Exception e) {
            return iso;
        }
    }

    private void toggleServer() {
        if (apiService != null && apiService.isRunning()) {
            Intent stopIntent = new Intent(this, AntiApiService.class);
            stopIntent.setAction(AntiApiService.ACTION_STOP);
            startService(stopIntent);
        } else {
            startDaemonService();
            mainHandler.postDelayed(this::fetchQuotasAsync, 1000);
        }
    }

    private void startDaemonService() {
        Intent startIntent = new Intent(this, AntiApiService.class);
        startIntent.setAction(AntiApiService.ACTION_START);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(startIntent);
        } else {
            startService(startIntent);
        }
    }

    private void copyToClipboard(String label, String text) {
        ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        if (cm != null) {
            cm.setPrimaryClip(ClipData.newPlainText(label, text));
            Toast.makeText(this, "Copied: " + text, Toast.LENGTH_SHORT).show();
        }
    }

    private void shareEndpoint(String endpoint) {
        Intent sendIntent = new Intent();
        sendIntent.setAction(Intent.ACTION_SEND);
        sendIntent.putExtra(Intent.EXTRA_TEXT, endpoint);
        sendIntent.setType("text/plain");
        Intent shareIntent = Intent.createChooser(sendIntent, "Share AntiAPI Endpoint");
        startActivity(shareIntent);
    }

    @Override
    public void onBackPressed() {
        if (currentTab != TAB_GATEWAY) {
            switchTab(TAB_GATEWAY);
        } else {
            moveTaskToBack(true);
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
            if (tvLogs != null) {
                tvLogs.append(line + "\n");
                if (tvLogs.getText().length() > 10000) {
                    String cur = tvLogs.getText().toString();
                    tvLogs.setText(cur.substring(cur.length() - 5000));
                }
                scrollLogs.post(() -> scrollLogs.fullScroll(View.FOCUS_DOWN));
            }
        });
    }

    @Override
    public void onStatusChanged(final boolean running) {
        runOnUiThread(() -> {
            if (running) {
                tvHeaderStatus.setText("● Running :8964");
                tvHeaderStatus.setTextColor(Color.parseColor("#34D399"));
                btnHeaderToggle.setText("Stop");
                btnHeaderToggle.setBackgroundResource(R.drawable.btn_danger_bg);

                tvStatus.setText("Status: Running on :8964");
                tvStatus.setTextColor(Color.parseColor("#34D399"));
                btnToggle.setText("Stop Server");
                btnToggle.setBackgroundResource(R.drawable.btn_danger_bg);

                if (currentTunnelUrl.isEmpty()) {
                    tvPublicUrl.setText("Public Tunnel: Connecting...");
                    tvPublicUrl.setTextColor(Color.parseColor("#FBBF24"));
                }
                fetchQuotasAsync();
            } else {
                tvHeaderStatus.setText("● Stopped");
                tvHeaderStatus.setTextColor(Color.parseColor("#EF4444"));
                btnHeaderToggle.setText("Start");
                btnHeaderToggle.setBackgroundResource(R.drawable.btn_success_bg);

                tvStatus.setText("Status: Stopped");
                tvStatus.setTextColor(Color.parseColor("#EF4444"));
                btnToggle.setText("Start Server");
                btnToggle.setBackgroundResource(R.drawable.btn_success_bg);

                tvPublicUrl.setText("Public Tunnel: Offline");
                tvPublicUrl.setTextColor(Color.parseColor("#64748B"));
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
                tvPublicUrl.setTextColor(Color.parseColor("#34D399"));
            }
        });
    }
}
