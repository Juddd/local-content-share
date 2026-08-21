warning: /bin/sh: setlocale: LC_ALL: cannot change locale (C.UTF-8)
package ink.yode.contenttransfer;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Intent;
import android.net.Uri;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public class ShareUploadService extends Service {
    public static final String ACTION_UPLOAD_PROGRESS = "ink.yode.contenttransfer.UPLOAD_PROGRESS";
    public static final String EXTRA_URIS = "uris";
    public static final String EXTRA_NAMES = "names";
    public static final String EXTRA_TEXT = "text";
    private static final String CHANNEL_ID = "share-uploads";
    private static final String BASE_URL = "http://nas.yode.ink:8084";
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());
    private NotificationManager notificationManager;

    @Override public void onCreate() {
        super.onCreate();
        notificationManager = getSystemService(NotificationManager.class);
        notificationManager.createNotificationChannel(new NotificationChannel(
                CHANNEL_ID, "分享上传", NotificationManager.IMPORTANCE_LOW));
        File cache=new File(getCacheDir(),"shared-uploads");File[] stale=cache.listFiles();if(stale!=null){long cutoff=System.currentTimeMillis()-86400000L;for(File file:stale)if(file.lastModified()<cutoff)file.delete();}
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        ArrayList<String> uris = intent == null ? null : intent.getStringArrayListExtra(EXTRA_URIS);
        ArrayList<String> names = intent == null ? null : intent.getStringArrayListExtra(EXTRA_NAMES);
        String sharedText = intent == null ? null : intent.getStringExtra(EXTRA_TEXT);
        int count = uris == null ? 0 : uris.size();
        if (sharedText != null && !sharedText.trim().isEmpty()) {
            startForeground(2401, notification("正在发送文字到 Snippets"));
            io.execute(() -> {
                try {
                    String title = uploadText(sharedText);
                    main.post(() -> Toast.makeText(this,
                            "已发送到 Snippets：" + title, Toast.LENGTH_LONG).show());
                } catch (Exception error) {
                    main.post(() -> Toast.makeText(this,
                            "文字发送失败：" + error.getMessage(), Toast.LENGTH_LONG).show());
                } finally {
                    stopSelf(startId);
                }
            });
            return START_NOT_STICKY;
        }
        startForeground(2401, notification("正在上传 " + count + " 个文件到 Files"));
        if (count == 0 || names == null || names.size() != count) {
            stopSelf(startId);
            return START_NOT_STICKY;
        }
        io.execute(() -> {
            for (int index = 0; index < count; index++) {
                String name = names.get(index);
                File file = null;
                int position = index + 1;
                try {
                    updateProgress("正在准备 " + position + "/" + count + " · " + name, 0, 0, true);
                    file=stage(Uri.parse(uris.get(index)),name);
                    String savedName = uploadWithRetry(file, name, position, count);
                    broadcastProgress(savedName + " · 已上传到 Files", 100, 100, false, true);
                    main.post(() -> Toast.makeText(this,
                            savedName + " 已发送到 Files", Toast.LENGTH_LONG).show());
                } catch (Exception error) {
                    broadcastProgress(name + " · 上传失败：" + error.getMessage(), 0, 100, false, true);
                    main.post(() -> Toast.makeText(this,
                            name + " 上传失败：" + error.getMessage(), Toast.LENGTH_LONG).show());
                } finally {
                    if(file!=null)file.delete();
                }
            }
            stopSelf(startId);
        });
        return START_NOT_STICKY;
    }

    private File stage(Uri uri,String name)throws Exception{File directory=new File(getCacheDir(),"shared-uploads");if(!directory.exists()&&!directory.mkdirs())throw new Exception("无法创建上传缓存");File file=File.createTempFile("share-",".tmp",directory);try(InputStream in=getContentResolver().openInputStream(uri);OutputStream out=new FileOutputStream(file)){if(in==null)throw new Exception("无法读取 "+name);byte[] buffer=new byte[65536];int count;while((count=in.read(buffer))>0)out.write(buffer,0,count);}catch(Exception error){file.delete();throw error;}return file;}

    private String uploadText(String text) throws Exception {
        HttpURLConnection connection = (HttpURLConnection) new URL(BASE_URL + "/submit").openConnection();
        connection.setConnectTimeout(30000);
        connection.setReadTimeout(60000);
        connection.setRequestMethod("POST");
        connection.setDoOutput(true);
        connection.setRequestProperty("Accept", "application/json");
        connection.setRequestProperty("Content-Type", "application/x-www-form-urlencoded; charset=utf-8");
        String body = "content=" + URLEncoder.encode(text, "UTF-8") + "&expiry=Never";
        try (OutputStream out = connection.getOutputStream()) {
            out.write(body.getBytes(StandardCharsets.UTF_8));
        }
        int code = connection.getResponseCode();
        InputStream response = code >= 200 && code < 400
                ? connection.getInputStream() : connection.getErrorStream();
        ByteArrayOutputStream sink = new ByteArrayOutputStream();
        if (response != null) try (InputStream in = response) {
            byte[] buffer = new byte[4096];
            int count;
            while ((count = in.read(buffer)) > 0) sink.write(buffer, 0, count);
        }
        if (code < 200 || code >= 400) throw new Exception("HTTP " + code);
        String title = new JSONObject(sink.toString("UTF-8")).optString("title").trim();
        if (title.isEmpty()) throw new Exception("服务器未返回 Snippet 标题");
        return title;
    }

    private String uploadWithRetry(File file, String name, int position, int totalFiles) throws Exception {
        String idempotencyKey=UUID.randomUUID().toString();
        Exception failure = null;
        for (int attempt = 1; attempt <= 3; attempt++) {
            try { return upload(file, name, position, totalFiles,idempotencyKey); }
            catch (Exception error) { failure = error; }
        }
        throw failure == null ? new Exception("未知错误") : failure;
    }

    private String upload(File file, String name, int position, int totalFiles,String idempotencyKey) throws Exception {
        String boundary = "----ContentTransfer" + System.nanoTime();
        HttpURLConnection connection = (HttpURLConnection) new URL(BASE_URL + "/upload-stream").openConnection();
        connection.setConnectTimeout(30000);
        connection.setReadTimeout(300000);
        connection.setRequestMethod("POST");
        connection.setDoOutput(true);
        connection.setChunkedStreamingMode(65536);
        connection.setRequestProperty("Content-Type", "multipart/form-data; boundary=" + boundary);
        connection.setRequestProperty("Idempotency-Key",idempotencyKey);
        String safeName = name.replace("\"", "").replace("\r", " ").replace("\n", " ");
        try (OutputStream out = connection.getOutputStream()) {
            String head = "--" + boundary + "\r\nContent-Disposition: form-data; name=\"expiry\"\r\n\r\nNever\r\n"
                    + "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file-upload\"; filename=\""
                    + safeName + "\"\r\nContent-Type: application/octet-stream\r\n\r\n";
            out.write(head.getBytes(StandardCharsets.UTF_8));
            try (InputStream in = new FileInputStream(file)) {
                byte[] buffer = new byte[65536];
                int count;
                long sent = 0;
                int lastPercent = -1;
                while ((count = in.read(buffer)) > 0) {
                    out.write(buffer, 0, count);
                    sent += count;
                    int percent = file.length() > 0 ? (int)Math.min(99, sent * 100 / file.length()) : 0;
                    if (percent != lastPercent) {
                        lastPercent = percent;
                        updateProgress("正在上传 " + position + "/" + totalFiles + " · " + name, sent, file.length(), false);
                    }
                }
            }
            out.write(("\r\n--" + boundary + "--\r\n").getBytes(StandardCharsets.UTF_8));
        }
        updateProgress("等待 NAS 保存 " + position + "/" + totalFiles + " · " + name, 0, 0, true);
        int code = connection.getResponseCode();
        InputStream response = code >= 200 && code < 300
                ? connection.getInputStream() : connection.getErrorStream();
        ByteArrayOutputStream sink = new ByteArrayOutputStream();
        if (response != null) try (InputStream in = response) {
            byte[] buffer = new byte[4096];
            int count;
            while ((count = in.read(buffer)) > 0) sink.write(buffer, 0, count);
        }
        String body = sink.toString("UTF-8");
        if (code < 200 || code >= 300) throw new Exception("HTTP " + code);
        JSONObject result = new JSONObject(body);
        JSONArray items = result.optJSONArray("items");
        return items != null && items.length() > 0
                ? items.getJSONObject(0).optString("filename", name) : name;
    }

    private Notification notification(String text) {
        return new Notification.Builder(this, CHANNEL_ID)
                .setSmallIcon(android.R.drawable.stat_sys_upload)
                .setContentTitle("内容中转")
                .setContentText(text)
                .setOnlyAlertOnce(true)
                .setOngoing(true)
                .build();
    }

    private void updateProgress(String text, long sent, long total, boolean indeterminate) {
        int percent = total > 0 ? (int)Math.min(99, sent * 100 / total) : 0;
        String detail = indeterminate || total <= 0 ? text : text + " · " + percent + "%（" + formatSize(sent) + " / " + formatSize(total) + "）";
        Notification notification = new Notification.Builder(this, CHANNEL_ID)
                .setSmallIcon(android.R.drawable.stat_sys_upload)
                .setContentTitle("内容中转")
                .setContentText(detail)
                .setStyle(new Notification.BigTextStyle().bigText(detail))
                .setOnlyAlertOnce(true)
                .setOngoing(true)
                .setProgress(100, percent, indeterminate || total <= 0)
                .build();
        notificationManager.notify(2401, notification);
        broadcastProgress(text, sent, total, indeterminate, false);
    }

    private void broadcastProgress(String text, long sent, long total, boolean indeterminate, boolean finished) {
        Intent progress = new Intent(ACTION_UPLOAD_PROGRESS).setPackage(getPackageName());
        progress.putExtra("message", text);
        progress.putExtra("sent", sent);
        progress.putExtra("total", total);
        progress.putExtra("indeterminate", indeterminate);
        progress.putExtra("finished", finished);
        sendBroadcast(progress);
    }

    private String formatSize(long bytes) {
        if (bytes < 1024) return bytes + " B";
        double value = bytes;
        String[] units = {"B", "KB", "MB", "GB"};
        int unit = 0;
        while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
        return String.format(java.util.Locale.CHINA, value >= 10 ? "%.0f %s" : "%.1f %s", value, units[unit]);
    }

    @Override public IBinder onBind(Intent intent) { return null; }

    @Override public void onDestroy() {
        io.shutdownNow();
        super.onDestroy();
    }
}
