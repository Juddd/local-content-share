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
import java.io.IOException;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Date;
import java.util.Locale;
import java.util.TimeZone;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public class ShareUploadService extends Service {
    public static final String ACTION_UPLOAD_PROGRESS = "ink.yode.contenttransfer.UPLOAD_PROGRESS";
    public static final String EXTRA_URIS = "uris";
    public static final String EXTRA_NAMES = "names";
    public static final String EXTRA_TEXT = "text";
    private static final String CHANNEL_ID = "share-uploads";
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());
    private NotificationManager notificationManager;
    private SyncDatabase syncDb;

    @Override public void onCreate() {
        super.onCreate();
        notificationManager = getSystemService(NotificationManager.class);
        syncDb=new SyncDatabase(this);
        notificationManager.createNotificationChannel(new NotificationChannel(
                CHANNEL_ID, "分享上传", NotificationManager.IMPORTANCE_LOW));
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String baseUrl=ServerConfig.get(this);
        if(baseUrl.isEmpty()){Toast.makeText(this,"请先打开内容中转配置服务器地址",Toast.LENGTH_LONG).show();stopSelf(startId);return START_NOT_STICKY;}
        ArrayList<String> uris = intent == null ? null : intent.getStringArrayListExtra(EXTRA_URIS);
        ArrayList<String> names = intent == null ? null : intent.getStringArrayListExtra(EXTRA_NAMES);
        String sharedText = intent == null ? null : intent.getStringExtra(EXTRA_TEXT);
        int count = uris == null ? 0 : uris.size();
        if (sharedText != null && !sharedText.trim().isEmpty()) {
            startForeground(2401, notification("正在发送文字到 Snippets"));
            io.execute(() -> {
                String itemId=UUID.randomUUID().toString(),operationId=null;
                try {
                    java.text.SimpleDateFormat titleFormat=new java.text.SimpleDateFormat("MM／dd HH-mm-ss",Locale.CHINA);titleFormat.setTimeZone(TimeZone.getTimeZone("Asia/Shanghai"));String title=titleFormat.format(new Date());
                    JSONObject item=new JSONObject().put("id",itemId).put("storageId","").put("type","text").put("filename",title).put("content",sharedText).put("createdAt",new Date().toInstant().toString()).put("modifiedAt",new Date().toInstant().toString()).put("size",0).put("favorite",false).put("revision",0);
                    syncDb.putLocal(baseUrl,item,SyncDatabase.PENDING);
                    JSONObject values=new JSONObject().put("clientId",itemId).put("content",sharedText).put("expiry","Never");JSONObject payload=new JSONObject().put("endpoint","/submit").put("values",values);
                    operationId=syncDb.enqueue(baseUrl,itemId,"create",payload,0);SyncRetryJobService.schedule(this);
                    if(!SyncDatabase.beginOperations())throw new IOException("后台同步任务正在运行");
                    JSONObject result;try{result=uploadText(baseUrl,sharedText,itemId,operationId);syncDb.complete(operationId,baseUrl,itemId,result.optJSONObject("item"));}finally{SyncDatabase.endOperations();}
                    title = result.optString("title",title);
                    String savedTitle=title;
                    main.post(() -> Toast.makeText(this,
                            "已发送到 Snippets：" + savedTitle, Toast.LENGTH_LONG).show());
                } catch (Exception error) {
                    SyncRetryJobService.schedule(this);
                    String feedback=operationId==null?"文字发送失败："+error.getMessage():"已加入待同步，联网后自动发送";
                    main.post(() -> Toast.makeText(this,
                            feedback, Toast.LENGTH_LONG).show());
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
                SyncDatabase.PendingUpload pending=null;
                boolean completed=false;
                int position = index + 1;
                try {
                    updateProgress("正在准备 " + position + "/" + count + " · " + name, 0, 0, true);
                    file=stage(Uri.parse(uris.get(index)),name);
                    pending=syncDb.addUpload(baseUrl,file.getAbsolutePath(),name,"Never");SyncRetryJobService.schedule(this);
                    if(!SyncDatabase.beginUpload(pending.id))throw new IOException("后台上传任务正在运行");
                    String savedName;try{savedName=uploadWithRetry(baseUrl,file, name, position, count,pending.id);syncDb.uploadComplete(pending.id);completed=true;}finally{SyncDatabase.endUpload(pending.id);}
                    broadcastProgress(savedName + " · 已上传到 Files", 100, 100, false, true);
                    main.post(() -> Toast.makeText(this,
                            savedName + " 已发送到 Files", Toast.LENGTH_LONG).show());
                } catch (Exception error) {
                    if(pending!=null)syncDb.uploadFailed(pending.id,error.getMessage());SyncRetryJobService.schedule(this);
                    String feedback=pending==null?name+" 准备失败："+error.getMessage():name+" 已加入待上传队列，联网后自动发送";
                    broadcastProgress(feedback, 0, 100, false, true);
                    main.post(() -> Toast.makeText(this,
                            feedback, Toast.LENGTH_LONG).show());
                } finally {
                    if(completed&&file!=null)file.delete();
                }
            }
            stopSelf(startId);
        });
        return START_NOT_STICKY;
    }

    private File stage(Uri uri,String name)throws Exception{File directory=new File(getFilesDir(),"pending_uploads");if(!directory.exists()&&!directory.mkdirs())throw new Exception("无法创建上传队列");File file=File.createTempFile("share-",".pending",directory);try(InputStream in=getContentResolver().openInputStream(uri);OutputStream out=new FileOutputStream(file)){if(in==null)throw new Exception("无法读取 "+name);byte[] buffer=new byte[65536];int count;while((count=in.read(buffer))>0)out.write(buffer,0,count);}catch(Exception error){file.delete();throw error;}return file;}

    private JSONObject uploadText(String baseUrl,String text,String itemId,String operationId) throws Exception {
        HttpURLConnection connection = (HttpURLConnection) new URL(baseUrl + "/submit").openConnection();
        connection.setConnectTimeout(30000);
        connection.setReadTimeout(60000);
        connection.setRequestMethod("POST");
        connection.setDoOutput(true);
        connection.setRequestProperty("Accept", "application/json");
        connection.setRequestProperty("Content-Type", "application/x-www-form-urlencoded; charset=utf-8");
        connection.setRequestProperty("Idempotency-Key",operationId);
        String body = "content=" + URLEncoder.encode(text, "UTF-8") + "&expiry=Never&clientId="+URLEncoder.encode(itemId,"UTF-8");
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
        JSONObject result=new JSONObject(sink.toString("UTF-8"));String title = result.optString("title").trim();
        if (title.isEmpty()) throw new Exception("服务器未返回 Snippet 标题");
        return result;
    }

    private String uploadWithRetry(String baseUrl,File file, String name, int position, int totalFiles,String idempotencyKey) throws Exception {
        Exception failure = null;
        for (int attempt = 1; attempt <= 3; attempt++) {
            try { return upload(baseUrl,file, name, position, totalFiles,idempotencyKey); }
            catch (Exception error) { failure = error; }
        }
        throw failure == null ? new Exception("未知错误") : failure;
    }

    private String upload(String baseUrl,File file, String name, int position, int totalFiles,String idempotencyKey) throws Exception {
        String boundary = "----ContentTransfer" + System.nanoTime();
        HttpURLConnection connection = (HttpURLConnection) new URL(baseUrl + "/upload-stream").openConnection();
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
        if(syncDb!=null)syncDb.close();
        super.onDestroy();
    }
}
