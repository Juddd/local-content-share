package ink.yode.contenttransfer;

import android.app.*;
import android.content.*;
import android.database.Cursor;
import android.graphics.Color;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.util.LruCache;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.Drawable;
import android.net.*;
import android.os.*;
import android.provider.MediaStore;
import android.provider.Settings;
import android.text.Editable;
import android.text.TextWatcher;
import android.text.method.LinkMovementMethod;
import android.view.*;
import android.webkit.MimeTypeMap;
import android.widget.*;

import org.json.*;

import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.text.DateFormat;
import java.util.*;
import java.util.concurrent.*;

import androidx.swiperefreshlayout.widget.SwipeRefreshLayout;

public class MainActivity extends Activity {
    private static final int PICK_FILE = 4101;
    private final ExecutorService metadataIo = Executors.newSingleThreadExecutor();
    private final ExecutorService transferIo = Executors.newFixedThreadPool(2);
    private final ExecutorService thumbnailIo = Executors.newFixedThreadPool(2);
    private final ExecutorService realtimeIo = Executors.newSingleThreadExecutor();
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private final LruCache<String, Bitmap> thumbnailCache = new LruCache<String, Bitmap>(12 * 1024) { @Override protected int sizeOf(String key, Bitmap value) { return value.getByteCount() / 1024; } };
    private final ArrayList<Item> allItems = new ArrayList<>(), visibleItems = new ArrayList<>();
    private final Map<String,Integer> downloadProgress = new ConcurrentHashMap<>();
    private final Set<String> deletingItems = Collections.newSetFromMap(new ConcurrentHashMap<>());
    private final Set<String> favoritePending = Collections.newSetFromMap(new ConcurrentHashMap<>());
    private SharedPreferences prefs;
    private LinearLayout root, toolbar, uploadPanel, uploadTasks;
    private TextView status;
    private ListView list;
    private ItemAdapter adapter;
    private String section = "text", activeBase = "";
    private Network activeNetwork;
    private EditText notepad;
    private ScrollView notepadPreviewScroll;
    private TextView notepadPreview, notepadRead, notepadSave;
    private LinearLayout notepadActions;
    private boolean notepadReading, notepadDirty, updatingNotepad;
    private String notepadSavedText = "";
    private String pendingExpiry = "Never";
    private final Map<String, TextView> tabViews = new LinkedHashMap<>();
    private SwipeRefreshLayout swipeRefresh;
    private AlertDialog uploadProgressDialog;
    private TextView uploadProgressMessage;
    private ProgressBar uploadProgressBar;
    private int activeUploadCount;
    private boolean uploadDialogSuppressed;
    private boolean shareProgressReceiverRegistered;
    private final BroadcastReceiver shareProgressReceiver=new BroadcastReceiver(){@Override public void onReceive(Context context,Intent intent){String message=intent.getStringExtra("message");if(message==null)return;boolean finished=intent.getBooleanExtra("finished",false),indeterminate=intent.getBooleanExtra("indeterminate",false);long sent=intent.getLongExtra("sent",0),total=intent.getLongExtra("total",0);if(!finished&&uploadProgressDialog==null)uploadDialogSuppressed=false;showUploadProgress(message,sent,indeterminate?-1:total);if(finished)mainHandler.postDelayed(()->{if(uploadProgressDialog!=null){uploadProgressDialog.dismiss();uploadProgressDialog=null;}},2500);}};
    private volatile boolean realtimeEnabled;
    private volatile int realtimeGeneration;
    private volatile HttpURLConnection realtimeConnection;
    private volatile long lastEventSequence = -1;
    private volatile int serverGeneration;
    private final Runnable realtimeRefresh = this::refresh;

    private class TransferUi {
        final ProgressBar progress;
        final TextView message;
        final AlertDialog dialog;
        volatile boolean cancelled;
        volatile HttpURLConnection connection;
        volatile String serverTaskId;
        TransferUi(String title){
            LinearLayout box=new LinearLayout(MainActivity.this);box.setPadding(dp(24),dp(8),dp(24),0);box.setOrientation(LinearLayout.VERTICAL);
            message=text("准备中…",14,Color.DKGRAY);box.addView(message);
            progress=new ProgressBar(MainActivity.this,null,android.R.attr.progressBarStyleHorizontal);progress.setMax(100);progress.setIndeterminate(true);box.addView(progress,new LinearLayout.LayoutParams(-1,dp(18)));
            dialog=new AlertDialog.Builder(MainActivity.this).setTitle(title).setView(box).setNegativeButton("取消",null).setCancelable(false).create();
            dialog.setOnShowListener(x->dialog.getButton(AlertDialog.BUTTON_NEGATIVE).setOnClickListener(v->cancel()));dialog.show();
        }
        void update(String label,long received,long total){runOnUiThread(()->{if(cancelled)return;if(total>0){progress.setIndeterminate(false);progress.setProgress((int)Math.min(100,received*100/total));message.setText(label+" · "+(received*100/total)+"%（"+formatSize(received)+" / "+formatSize(total)+"）");}else{progress.setIndeterminate(true);message.setText(label+" · "+formatSize(received));}});}
        void cancel(){cancelled=true;HttpURLConnection c=connection;if(c!=null)c.disconnect();String task=serverTaskId;if(task!=null)metadataIo.execute(()->{try{HttpURLConnection x=MainActivity.this.connection(activeNetwork,activeBase+"/api/v1/download-tasks/"+task,7000);x.setRequestMethod("DELETE");read(x);}catch(Exception ignored){}});dialog.dismiss();setStatus("下载已取消");}
        void success(String text){runOnUiThread(()->{dialog.dismiss();toast(text);});}
        void fail(Exception e){runOnUiThread(()->{dialog.dismiss();setStatus((cancelled?"下载已取消":"下载失败 · "+e.getMessage()));});}
    }

    private class UploadTaskUi {
        final LinearLayout row;
        final TextView label;
        final ProgressBar progress;
        UploadTaskUi(String name) {
            if(activeUploadCount==0)uploadDialogSuppressed=false;
            activeUploadCount++;
            showUploadProgress(name+" · 等待处理",0,-1);
            row=new LinearLayout(MainActivity.this);row.setOrientation(LinearLayout.VERTICAL);row.setPadding(dp(12),dp(7),dp(12),dp(7));row.setBackground(rounded(Color.rgb(245,241,247),12));
            label=text(name+" · 等待处理",13,Color.rgb(73,62,80));label.setPadding(0,0,0,dp(4));row.addView(label);
            progress=new ProgressBar(MainActivity.this,null,android.R.attr.progressBarStyleHorizontal);progress.setMax(100);progress.setIndeterminate(true);row.addView(progress,new LinearLayout.LayoutParams(-1,dp(8)));
            LinearLayout.LayoutParams params=new LinearLayout.LayoutParams(-1,-2);params.setMargins(0,0,0,dp(6));uploadTasks.addView(row,params);showUploadPanel();
        }
        void preparing(){runOnUiThread(()->{String value=labelName()+" · 正在准备";label.setText(value);progress.setIndeterminate(true);showUploadProgress(value,0,-1);});}
        void uploading(String name,long sent,long total){runOnUiThread(()->{long percent=total>0?Math.min(99,sent*100/total):0;progress.setIndeterminate(total<=0);if(total>0)progress.setProgress((int)percent);String value=name+" · "+percent+"%（"+formatSize(sent)+" / "+formatSize(total)+"）";label.setText(value);showUploadProgress(value,sent,total);});}
        void waiting(String name){runOnUiThread(()->{String value=name+" · 等待 NAS 保存";progress.setIndeterminate(true);label.setText(value);showUploadProgress(value,0,-1);});}
        void complete(String name){finish(name+" · 已上传到 Files",Color.rgb(34,139,70),100);}
        void failed(String name,String error){finish(name+" · 失败："+error,Color.rgb(180,40,40),0);}
        private String labelName(){String value=label.getText().toString();int split=value.indexOf(" · ");return split<0?value:value.substring(0,split);}
        private void finish(String textValue,int color,int value){runOnUiThread(()->{progress.setIndeterminate(false);progress.setProgress(value);label.setText(textValue);label.setTextColor(color);showUploadProgress(textValue,value,100);activeUploadCount=Math.max(0,activeUploadCount-1);if(activeUploadCount==0)mainHandler.postDelayed(()->{if(activeUploadCount==0&&uploadProgressDialog!=null){uploadProgressDialog.dismiss();uploadProgressDialog=null;}},2500);mainHandler.postDelayed(()->{uploadTasks.removeView(row);if(uploadTasks.getChildCount()==0)uploadPanel.setVisibility(View.GONE);},10000);});}
    }

    static class Item {
        String id, type, filename, content, createdAt, modifiedAt;
        long size; boolean favorite;
        static Item from(JSONObject o) {
            Item i = new Item();
            i.id=o.optString("id"); i.type=o.optString("type"); i.filename=o.optString("filename");
            i.content=o.optString("content"); i.createdAt=o.optString("createdAt"); i.modifiedAt=o.optString("modifiedAt"); i.size=o.optLong("size"); i.favorite=o.optBoolean("favorite");
            return i;
        }
    }

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        if(Build.VERSION.SDK_INT>=33&&checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)!=android.content.pm.PackageManager.PERMISSION_GRANTED)requestPermissions(new String[]{android.Manifest.permission.POST_NOTIFICATIONS},4201);
        prefs = getSharedPreferences("content-transfer", MODE_PRIVATE);
        activeBase=ServerConfig.get(this);
        prefs.edit().remove("clipboard_diagnostics").apply();
        buildUi();
        loadCache();
        if(activeBase.isEmpty())mainHandler.post(()->showServerConfig(true));
    }

    @Override protected void onResume(){super.onResume();if(adapter!=null)renderSection();if(!activeBase.isEmpty()){refresh();startRealtimeUpdates();}}

    @Override protected void onStart(){super.onStart();if(!shareProgressReceiverRegistered){IntentFilter filter=new IntentFilter(ShareUploadService.ACTION_UPLOAD_PROGRESS);if(Build.VERSION.SDK_INT>=33)registerReceiver(shareProgressReceiver,filter,Context.RECEIVER_NOT_EXPORTED);else registerReceiver(shareProgressReceiver,filter);shareProgressReceiverRegistered=true;}}

    @Override protected void onStop(){if(shareProgressReceiverRegistered){unregisterReceiver(shareProgressReceiver);shareProgressReceiverRegistered=false;}super.onStop();}

    @Override protected void onPause(){stopRealtimeUpdates();super.onPause();}

    private void takePersistable(Uri uri,Intent intent){try{int flags=intent.getFlags()&(Intent.FLAG_GRANT_READ_URI_PERMISSION|Intent.FLAG_GRANT_WRITE_URI_PERMISSION);getContentResolver().takePersistableUriPermission(uri,flags&Intent.FLAG_GRANT_READ_URI_PERMISSION);}catch(Exception ignored){}}

    private TextView text(String value, float sp, int color) {
        if (value.startsWith("1.0.24\n")) value = "1.0.36\n• App 内为每个文件独立显示上传进度和状态\n• 新增“删除手机本地副本”，不影响 NAS 文件\n• 无本地副本时删除本地按钮自动置灰\n• 原删除操作明确标注为删除 NAS 内容\n\n1.0.35\n• 移除不稳定的剪贴板监听、快捷圆钮和诊断功能\n• 分享文字时后台发送到 Snippets\n• 分享文件时后台上传到 Files，完成提示显示文件名\n• 大文件使用分块上传，NAS 确认前不再提前显示 100%\n\n" + value;
        if (value.startsWith("1.0.36\n")) value = "1.0.38\n• 分享文字上传成功后显示明确回执\n• 回执显示 NAS 实际保存的 Snippet 标题\n\n1.0.37\n• 修复 App 内上传任务进度不明显、容易看不到的问题\n• 上传任务改为固定醒目的面板，选中文件后立即显示\n• 新增文件加入上传队列的即时提示\n\n" + value;
        if (value.startsWith("1.0.38\n")) value = "1.0.40\n• 系统分享文件时在通知中显示实时上传进度\n• 通知显示文件名、任务数量、已传大小和 NAS 保存状态\n• App 内上传与系统分享上传使用一致的进度阶段\n\n1.0.39\n• App 内选择文件后立即弹出上传进度窗口\n• 显示当前文件百分比及等待 NAS 保存状态\n• 可选择后台运行，页面任务面板仍会保留\n\n" + value;
        if (value.startsWith("1.0.40\n") && !value.contains("App 已打开时同步在界面显示分享上传进度")) value=value.replace("• 系统分享文件时在通知中显示实时上传进度\n","• 系统分享文件时在通知中显示实时上传进度\n• App 已打开时同步在界面显示分享上传进度\n");
        if (value.startsWith("1.0.40\n")) value="1.0.44\n• NAS 删除成功后只移除对应卡片，不再全量刷新列表\n• 手机下载进度改为卡片底部绿色进度条\n• 下载完成后进度条自动变为本地副本状态线\n\n1.0.43\n• 删除手机本地副本和 NAS 文件时不再弹出确认窗口\n• 点击两个删除按钮后立即执行对应删除操作\n\n1.0.42\n• 缩小 Files 卡片操作按钮和图标\n• 按钮之间增加留白，文件大小获得更多显示空间\n• 保留六个直达操作及带相同图标的长按菜单\n\n1.0.41\n• Files 卡片新增六个常用操作图标按钮\n• 文件大小与操作按钮同一行均匀排列\n• 长按菜单保留，并显示与按钮一致的图标\n• 无本地副本时打开和删除本地按钮自动置灰\n\n"+value;
        if (value.startsWith("1.0.44\n")) value="1.0.45\n• 记事本新增 Markdown 阅读模式和阅读/保存状态按钮\n• 修复上传重试重复文件、实时同步竞态和分享缓存残留风险\n• 上传、缩略图、同步任务分离执行，避免互相阻塞\n• 扩大常用操作触控区域并增强服务端稳定性\n\n"+value;
        if (value.startsWith("1.0.45\n")) value="1.0.46\n• 修复 Markdown 阅读模式仍显示语法标记的问题\n• 标题、粗斜体、链接、列表和代码块改为真正解析显示\n• 缩窄 Files 操作按钮，完整显示文件大小单位\n\n"+value;
        if (value.startsWith("1.0.46\n")) value="1.0.47\n• 下载、打开和重命名改为与网页一致的矢量图标\n• 长按文件菜单同步使用同一套图标\n\n"+value;
        if (value.startsWith("1.0.47\n")) value="1.0.48\n• 三个矢量操作改为网页 Font Awesome 同款图形\n• 修复图标在按钮背景中偏左的问题\n• 可用操作统一为蓝绿色，不可用操作显示为灰色\n\n"+value;
        if (value.startsWith("1.0.48\n")) value="1.0.49\n• 重命名改为与网页视觉一致的简洁 I 形光标\n• 六个文件操作按钮整体再缩小少许\n• 直达按钮和长按菜单统一使用蓝绿色矢量图标\n• 不可用操作统一显示为灰色\n\n"+value;
        if (value.startsWith("1.0.49\n")) value="1.0.50\n• 恢复复制地址、删除本地副本和删除 NAS 的原始图标\n• 三个原始图标只随按钮整体缩小，不改变样式\n\n"+value;
        if (value.startsWith("1.0.50\n")) value="1.0.51\n• 文字和链接长按菜单补充查看、复制、重命名和删除对应图标\n• 文字和链接区仍保持只有长按菜单，不增加直达按钮\n\n"+value;
        if (value.startsWith("1.0.51\n")) value="1.0.52\n• 链接打开改为网页端跳转图标\n• 文字编辑改为网页端编辑图标\n\n"+value;
        if (value.startsWith("1.0.52\n")) value="1.0.53\n• 文字复制改为网页 fa-copy 同款图标\n• 收藏的 Snippet 始终排在普通项目之前\n\n"+value;
        if (value.startsWith("1.0.53\n")) value="1.0.54\n• Snippet 新增可同步的星标收藏按钮和收藏优先排序\n• 首次启动必须配置服务器地址，设置中可随时更改\n• 服务器地址永久保存，系统分享与主界面统一使用该配置\n• 移除源码中写死的个人 NAS 地址\n\n"+value;
        TextView v = new TextView(this); v.setText(value); v.setTextSize(sp); v.setTextColor(color); v.setPadding(dp(12),dp(10),dp(12),dp(10)); return v;
    }
    private GradientDrawable rounded(int color,int radius) { GradientDrawable d=new GradientDrawable();d.setColor(color);d.setCornerRadius(dp(radius));return d; }
    private Button button(String label) { Button b=new Button(this);b.setText(label);b.setAllCaps(false);b.setTextSize(14);b.setMinHeight(0);b.setMinimumHeight(0);b.setMinWidth(0);b.setMinimumWidth(0);b.setPadding(dp(16),dp(10),dp(16),dp(10));b.setBackground(rounded(Color.rgb(235,229,239),18));return b; }
    private TextView iconButton(String symbol,String description) { TextView v=text(symbol,23,Color.rgb(73,62,80));v.setGravity(Gravity.CENTER);v.setContentDescription(description);v.setBackground(rounded(Color.rgb(235,229,239),22));v.setPadding(0,0,0,0);return v; }
    private TextView fileActionButton(String symbol,String description,boolean enabled,View.OnClickListener action) { TextView v=text(symbol,16,enabled?Color.rgb(75,151,174):Color.LTGRAY);v.setGravity(Gravity.CENTER);v.setContentDescription(description);v.setEnabled(enabled);v.setAlpha(enabled?1f:.38f);v.setBackground(rounded(Color.rgb(244,240,246),13));v.setPadding(0,0,0,0);if(enabled)v.setOnClickListener(action);v.setOnLongClickListener(x->{toast(description);return true;});return v; }
    private ImageView fileActionButton(int drawableId,String description,boolean enabled,View.OnClickListener action) { ImageView v=new ImageView(this);v.setImageResource(drawableId);v.setImageTintList(android.content.res.ColorStateList.valueOf(enabled?Color.rgb(75,151,174):Color.LTGRAY));v.setScaleType(ImageView.ScaleType.CENTER);v.setContentDescription(description);v.setEnabled(enabled);v.setBackground(rounded(Color.rgb(244,240,246),12));v.setPadding(dp(8),dp(8),dp(8),dp(8));if(enabled)v.setOnClickListener(action);v.setOnLongClickListener(x->{toast(description);return true;});return v; }
    private TextView actionChip(String label,boolean primary) { TextView v=text(label,14,primary?Color.WHITE:Color.rgb(73,62,80));v.setGravity(Gravity.CENTER);v.setTypeface(Typeface.DEFAULT,Typeface.BOLD);v.setBackground(rounded(primary?Color.rgb(103,80,164):Color.rgb(235,229,239),20));v.setPadding(dp(18),dp(10),dp(18),dp(10));return v; }
    private int dp(int n) { return (int)(n*getResources().getDisplayMetrics().density+.5f); }

    private void showUploadPanel(){uploadPanel.setVisibility(View.VISIBLE);uploadPanel.requestLayout();}

    private void showUploadProgress(String message,long sent,long total){
        if(uploadDialogSuppressed)return;
        if(uploadProgressDialog==null){
            LinearLayout box=new LinearLayout(this);box.setOrientation(LinearLayout.VERTICAL);box.setPadding(dp(24),dp(8),dp(24),0);
            uploadProgressMessage=text(message,14,Color.DKGRAY);uploadProgressMessage.setPadding(0,0,0,dp(8));box.addView(uploadProgressMessage);
            uploadProgressBar=new ProgressBar(this,null,android.R.attr.progressBarStyleHorizontal);uploadProgressBar.setMax(100);box.addView(uploadProgressBar,new LinearLayout.LayoutParams(-1,dp(18)));
            uploadProgressDialog=new AlertDialog.Builder(this).setTitle("正在上传到 Files").setView(box).setNegativeButton("后台运行",(d,w)->uploadDialogSuppressed=true).create();
            uploadProgressDialog.setOnDismissListener(d->{uploadProgressDialog=null;uploadProgressMessage=null;uploadProgressBar=null;});uploadProgressDialog.show();
        }
        if(uploadProgressMessage!=null)uploadProgressMessage.setText(message);
        if(uploadProgressBar!=null){boolean known=total>0;uploadProgressBar.setIndeterminate(!known);if(known)uploadProgressBar.setProgress((int)Math.min(100,sent*100/total));}
    }

    private void buildUi() {
        root = new LinearLayout(this); root.setOrientation(LinearLayout.VERTICAL); root.setPadding(dp(12),dp(10),dp(12),0); root.setBackgroundColor(Color.rgb(250,247,252));
        root.setOnApplyWindowInsetsListener((view,insets)->{int top=Build.VERSION.SDK_INT>=30?insets.getInsets(WindowInsets.Type.statusBars()).top:insets.getSystemWindowInsetTop();view.setPadding(dp(12),top+dp(10),dp(12),0);return insets;});
        LinearLayout title = new LinearLayout(this); title.setGravity(Gravity.CENTER_VERTICAL);title.setPadding(dp(4),0,dp(4),dp(4));
        TextView heading=text("内容中转",23,Color.rgb(45,39,49)); heading.setTypeface(Typeface.DEFAULT,Typeface.BOLD);heading.setPadding(dp(4),dp(6),dp(8),dp(6)); title.addView(heading,new LinearLayout.LayoutParams(0,-2,1));
        TextView settings=iconButton("⚙","设置"); settings.setOnClickListener(v->showSettings());LinearLayout.LayoutParams iconParams=new LinearLayout.LayoutParams(dp(44),dp(44));iconParams.setMarginStart(dp(8));title.addView(settings,iconParams);
        TextView refresh=iconButton("↻","刷新"); refresh.setOnClickListener(v->refresh());LinearLayout.LayoutParams refreshParams=new LinearLayout.LayoutParams(dp(44),dp(44));refreshParams.setMarginStart(dp(8));title.addView(refresh,refreshParams); root.addView(title);
        status=text("正在连接…",12,Color.rgb(96,87,101)); status.setPadding(dp(8),0,dp(8),dp(10)); root.addView(status);
        toolbar=new LinearLayout(this); toolbar.setGravity(Gravity.CENTER); toolbar.setWeightSum(4);toolbar.setPadding(0,dp(2),0,dp(8));
        addTab("文字","text"); addTab("文件","file"); addTab("链接","link"); addTab("记事本","notepad"); root.addView(toolbar);
        LinearLayout actions=new LinearLayout(this); actions.setGravity(Gravity.END|Gravity.CENTER_VERTICAL);actions.setPadding(0,0,dp(2),dp(8));
        TextView sort=actionChip("⇅  排序",false); sort.setOnClickListener(v->showSort()); actions.addView(sort);
        TextView add=actionChip("＋  新增",true); add.setOnClickListener(v->addCurrent());LinearLayout.LayoutParams addParams=new LinearLayout.LayoutParams(-2,-2);addParams.setMarginStart(dp(8));actions.addView(add,addParams); root.addView(actions);
        uploadPanel=new LinearLayout(this);uploadPanel.setOrientation(LinearLayout.VERTICAL);uploadPanel.setPadding(dp(10),dp(8),dp(10),dp(4));uploadPanel.setBackground(rounded(Color.rgb(229,218,240),16));uploadPanel.setVisibility(View.GONE);
        TextView uploadHeading=text("上传任务",15,Color.rgb(62,43,78));uploadHeading.setTypeface(Typeface.DEFAULT,Typeface.BOLD);uploadHeading.setPadding(dp(2),0,dp(2),dp(6));uploadPanel.addView(uploadHeading);
        uploadTasks=new LinearLayout(this);uploadTasks.setOrientation(LinearLayout.VERTICAL);uploadPanel.addView(uploadTasks,new LinearLayout.LayoutParams(-1,-2));
        LinearLayout.LayoutParams uploadPanelParams=new LinearLayout.LayoutParams(-1,-2);uploadPanelParams.setMargins(0,0,0,dp(8));root.addView(uploadPanel,uploadPanelParams);
        list=new ListView(this); adapter=new ItemAdapter(); list.setAdapter(adapter); root.addView(list,new LinearLayout.LayoutParams(-1,0,1));
        swipeRefresh = new SwipeRefreshLayout(this);
        swipeRefresh.setColorSchemeColors(Color.rgb(103,80,164));
        swipeRefresh.setProgressBackgroundColorSchemeColor(Color.WHITE);
        swipeRefresh.setOnRefreshListener(this::refresh);
        swipeRefresh.setOnChildScrollUpCallback((parent, child) -> section.equals("notepad") ? notepad != null && notepad.canScrollVertically(-1) : list.canScrollVertically(-1));
        swipeRefresh.addView(root,new SwipeRefreshLayout.LayoutParams(-1,-1));
        setContentView(swipeRefresh);
    }

    private void addTab(String label,String key) {
        TextView b=actionChip(label,false);b.setTextSize(14);b.setOnClickListener(v->{section=key;updateTabs();renderSection();});LinearLayout.LayoutParams p=new LinearLayout.LayoutParams(0,dp(44),1);p.setMargins(dp(3),0,dp(3),0);toolbar.addView(b,p);tabViews.put(key,b);if(key.equals(section))updateTabs();
    }

    private void updateTabs(){for(Map.Entry<String,TextView> e:tabViews.entrySet()){boolean selected=e.getKey().equals(section);e.getValue().setTextColor(selected?Color.WHITE:Color.rgb(73,62,80));e.getValue().setBackground(rounded(selected?Color.rgb(103,80,164):Color.rgb(239,234,242),20));}}

    private void setStatus(String s) { runOnUiThread(()->status.setText(s)); }

    private void loadCache() {
        String raw=activeBase.isEmpty()?"[]":prefs.getString("items:"+activeBase, "[]");
        try { parseItems(new JSONArray(raw)); status.setText("已显示本地缓存，正在同步…"); } catch(Exception ignored) {}
    }

    private void parseItems(JSONArray array) throws JSONException {
        allItems.clear(); for(int n=0;n<array.length();n++) allItems.add(Item.from(array.getJSONObject(n))); renderSection();
    }

    private Network findNetwork(boolean wifiOnly) {
        ConnectivityManager cm=getSystemService(ConnectivityManager.class);
        Network fallback=null;
        for(Network n:cm.getAllNetworks()) {
            NetworkCapabilities c=cm.getNetworkCapabilities(n); if(c==null || c.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) continue;
            if(c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) return n;
            if(!wifiOnly && c.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) fallback=n;
        }
        return fallback;
    }

    private HttpURLConnection connection(Network n,String url,int timeout) throws IOException {
        URL u=new URL(url); HttpURLConnection c=(HttpURLConnection)(n!=null?n.openConnection(u):u.openConnection());
        c.setConnectTimeout(timeout); c.setReadTimeout(Math.max(timeout,8000)); c.setUseCaches(false); c.setRequestProperty("Accept","application/json"); return c;
    }

    private String read(HttpURLConnection c) throws IOException {
        int code=c.getResponseCode(); InputStream in=code>=200&&code<300?c.getInputStream():c.getErrorStream();
        ByteArrayOutputStream out=new ByteArrayOutputStream(); if(in!=null){byte[] b=new byte[8192];int n;while((n=in.read(b))>0)out.write(b,0,n);}
        String s=out.toString(StandardCharsets.UTF_8.name()); if(code<200||code>=300)throw new IOException("HTTP "+code+" "+s); return s;
    }

    private void refresh() {
        if(activeBase.isEmpty()){if(swipeRefresh!=null)swipeRefresh.setRefreshing(false);setStatus("请先配置服务器地址");return;}
        final String requestBase=activeBase;final int requestGeneration=serverGeneration;
        if (swipeRefresh != null && !swipeRefresh.isRefreshing()) swipeRefresh.setRefreshing(true);
        setStatus("正在同步…");
        metadataIo.execute(()->{
            try {
                Network net=findNetwork(false);
                HttpURLConnection request=connection(net,requestBase+"/api/v1/items",7000);
                String raw=read(request);
                long snapshotSequence;
                try{snapshotSequence=Long.parseLong(request.getHeaderField("X-Content-Sequence"));}catch(Exception ignored){snapshotSequence=-1;}
                activeNetwork=net;
                String route=syncRoute(net);
                if(requestGeneration!=serverGeneration||!requestBase.equals(activeBase))return;
                JSONArray data=new JSONArray(raw); prefs.edit().putString("items:"+requestBase,data.toString()).apply();
                long finalSnapshotSequence=snapshotSequence;
                runOnUiThread(()->{if(finalSnapshotSequence>=0&&lastEventSequence>finalSnapshotSequence){swipeRefresh.setRefreshing(false);mainHandler.postDelayed(this::refresh,150);return;}try{parseItems(data);}catch(Exception ignored){}if(finalSnapshotSequence>=0)lastEventSequence=Math.max(lastEventSequence,finalSnapshotSequence);status.setText("已同步（"+route+"）");swipeRefresh.setRefreshing(false);});
            } catch(Exception e) { if(requestGeneration==serverGeneration&&requestBase.equals(activeBase)){setStatus("离线显示缓存 · "+e.getMessage());runOnUiThread(()->swipeRefresh.setRefreshing(false));} }
        });
    }

    private String syncRoute(Network net){try{String host=Uri.parse(activeBase).getHost();if(host==null)return "网络类型未知";InetAddress[] addresses=net!=null?net.getAllByName(host):InetAddress.getAllByName(host);for(InetAddress address:addresses){if(address.isSiteLocalAddress()||address.isLinkLocalAddress()||address.isLoopbackAddress())return "局域网";byte[] raw=address.getAddress();if(raw.length==16&&(raw[0]&0xfe)==0xfc)return "局域网";}return "外网";}catch(Exception ignored){return "网络类型未知";}}

    private synchronized void startRealtimeUpdates() {
        if(realtimeEnabled||activeBase.isEmpty())return;
        realtimeEnabled=true;
        final int generation=++realtimeGeneration;
        realtimeIo.execute(()->{
            while(realtimeEnabled&&realtimeGeneration==generation){
                HttpURLConnection c=null;
                try{
                    Network net=findNetwork(false);
                    c=connection(net,activeBase+"/api/updates"+(lastEventSequence>=0?"?since="+lastEventSequence:""),10000);
                    c.setReadTimeout(45000);
                    c.setRequestProperty("Accept","text/event-stream");
                    realtimeConnection=c;
                    if(c.getResponseCode()!=HttpURLConnection.HTTP_OK)throw new IOException("HTTP "+c.getResponseCode());
                    try(BufferedReader reader=new BufferedReader(new InputStreamReader(c.getInputStream(),StandardCharsets.UTF_8))){
                        String line;
                        while(realtimeEnabled&&realtimeGeneration==generation&&(line=reader.readLine())!=null){
                            if(line.equals("data: content_updated")){
                                mainHandler.removeCallbacks(realtimeRefresh);
                                mainHandler.postDelayed(realtimeRefresh,250);
                            }else if(line.startsWith("data: {")&&line.endsWith("}")){
                                try{JSONObject change=new JSONObject(line.substring(6));long sequence=change.optLong("sequence",-1);String type=change.optString("type");if(type.equals("connected")){lastEventSequence=sequence;mainHandler.removeCallbacks(realtimeRefresh);mainHandler.postDelayed(realtimeRefresh,250);}else if(lastEventSequence<0||sequence!=lastEventSequence+1){lastEventSequence=sequence;mainHandler.removeCallbacks(realtimeRefresh);mainHandler.postDelayed(realtimeRefresh,250);}else{lastEventSequence=sequence;if(type.equals("deleted")){String id=change.optString("id");mainHandler.post(()->applyRemoteDelete(id));}else if(type.equals("created")||type.equals("updated")||type.equals("renamed")){JSONObject itemJson=change.optJSONObject("item");String oldId=change.optString("oldId",change.optString("id"));if(itemJson!=null){Item item=Item.from(itemJson);mainHandler.post(()->applyRemoteItem(oldId,item));}}else{mainHandler.removeCallbacks(realtimeRefresh);mainHandler.postDelayed(realtimeRefresh,250);}}}
                                catch(Exception ignored){}
                            }
                        }
                    }
                }catch(Exception ignored){}
                finally{if(c!=null)c.disconnect();if(realtimeConnection==c)realtimeConnection=null;}
                if(realtimeEnabled&&realtimeGeneration==generation){try{Thread.sleep(1500);}catch(InterruptedException e){Thread.currentThread().interrupt();return;}}
            }
        });
    }

    private synchronized void stopRealtimeUpdates(){
        realtimeEnabled=false;
        realtimeGeneration++;
        mainHandler.removeCallbacks(realtimeRefresh);
        HttpURLConnection c=realtimeConnection;
        realtimeConnection=null;
        if(c!=null)c.disconnect();
    }

    private void applyRemoteDelete(String id){if(id==null||id.isEmpty())return;deletingItems.remove(id);downloadProgress.remove(id);allItems.removeIf(item->item.id.equals(id));visibleItems.removeIf(item->item.id.equals(id));adapter.notifyDataSetChanged();}
    private void applyRemoteItem(String oldId,Item updated){if(updated==null)return;allItems.removeIf(item->item.id.equals(oldId)||item.id.equals(updated.id));allItems.add(updated);renderSection();}

    private void renderSection() {
        if(section.equals("notepad")){ showNotepad(); return; }
        if(notepad!=null){root.removeView(notepad);notepad=null;if(notepadPreviewScroll!=null){root.removeView(notepadPreviewScroll);notepadPreviewScroll=null;notepadPreview=null;}if(notepadActions!=null){root.removeView(notepadActions);notepadActions=null;notepadRead=null;notepadSave=null;}if(list.getParent()==null)root.addView(list,new LinearLayout.LayoutParams(-1,0,1));}
        visibleItems.clear(); for(Item i:allItems) if(i.type.equals(section)) visibleItems.add(i);
        String key=prefs.getString("sort_"+section,section.equals("file")?"created_desc":"created_desc");
        Comparator<Item> cmp;
        if(key.startsWith("title")) cmp=Comparator.comparing(a->a.filename,java.text.Collator.getInstance(Locale.CHINA));
        else if(key.startsWith("modified")) cmp=Comparator.comparing(a->a.modifiedAt);
        else if(key.startsWith("size")) cmp=Comparator.comparingLong(a->a.size);
        else cmp=Comparator.comparing(a->a.createdAt);
        if(key.endsWith("desc")) cmp=cmp.reversed(); if(section.equals("text"))cmp=Comparator.<Item,Boolean>comparing(a->a.favorite).reversed().thenComparing(cmp); Collections.sort(visibleItems,cmp); adapter.notifyDataSetChanged();
    }

    private void showSort() {
        if(section.equals("notepad")) return;
        ArrayList<String> labels=new ArrayList<>(Arrays.asList("创建时间（新到旧）","创建时间（旧到新）","修改时间（新到旧）","修改时间（旧到新）","标题（正序）","标题（倒序）"));
        ArrayList<String> keys=new ArrayList<>(Arrays.asList("created_desc","created_asc","modified_desc","modified_asc","title_asc","title_desc"));
        if(section.equals("file")){labels.add("文件大小（大到小）");labels.add("文件大小（小到大）");keys.add("size_desc");keys.add("size_asc");}
        new AlertDialog.Builder(this).setTitle("排序方式").setItems(labels.toArray(new String[0]),(d,w)->{prefs.edit().putString("sort_"+section,keys.get(w)).apply();renderSection();}).show();
    }

    private class ItemAdapter extends BaseAdapter {
        public int getCount(){return visibleItems.size();} public Object getItem(int p){return visibleItems.get(p);} public long getItemId(int p){return p;}
        public View getView(int p,View old,android.view.ViewGroup parent){
            Item i=visibleItems.get(p); LinearLayout box=new LinearLayout(MainActivity.this);box.setOrientation(LinearLayout.VERTICAL);box.setPadding(dp(14),dp(8),dp(14),dp(8));
            LinearLayout heading=new LinearLayout(MainActivity.this);heading.setGravity(Gravity.CENTER_VERTICAL); ImageView thumb=null;
            if(i.type.equals("file")&&isImageName(i.filename)){thumb=new ImageView(MainActivity.this);thumb.setScaleType(ImageView.ScaleType.CENTER_INSIDE);thumb.setTag(i.id);Bitmap cached=thumbnailCache.get(i.id);if(cached!=null)thumb.setImageBitmap(cached);heading.addView(thumb,new LinearLayout.LayoutParams(dp(70),dp(56)));if(cached==null)loadThumbnail(i,thumb);}
            TextView name=text(i.filename,17,Color.rgb(40,35,45));name.setTypeface(null,1);name.setMaxLines(2);name.setEllipsize(android.text.TextUtils.TruncateAt.END);heading.addView(name,new LinearLayout.LayoutParams(0,-2,1));box.addView(heading);
            String sub=i.type.equals("text")?preview(i.content):i.type.equals("file")?formatSize(i.size):i.content;
            TextView detail=text(sub,14,Color.DKGRAY);detail.setMaxLines(i.type.equals("text")?5:2);
            if(i.type.equals("file")) {
                boolean local=isLocalAvailable(i);LinearLayout actionRow=new LinearLayout(MainActivity.this);actionRow.setGravity(Gravity.CENTER_VERTICAL);
                detail.setGravity(Gravity.CENTER_VERTICAL);detail.setSingleLine(true);detail.setPadding(dp(4),0,dp(4),0);actionRow.addView(detail,new LinearLayout.LayoutParams(0,dp(40),1));
                View[] buttons={fileActionButton(R.drawable.ic_action_download,"下载",true,v->download(i,false)),fileActionButton(R.drawable.ic_action_view,"打开",local,v->openLocal(i)),fileActionButton("🔗","复制地址",true,v->copy(activeBase+"/view/"+path(i.id))),fileActionButton(R.drawable.ic_action_rename,"重命名",true,v->rename(i)),fileActionButton("⌫","删除手机本地副本",local,v->deleteLocalCopy(i)),fileActionButton("🗑","删除 NAS 文件",true,v->delete(i))};
                for(View button:buttons){LinearLayout.LayoutParams buttonParams=new LinearLayout.LayoutParams(dp(36),dp(38));buttonParams.setMargins(dp(1),0,dp(1),0);actionRow.addView(button,buttonParams);}box.addView(actionRow);
                Integer progress=downloadProgress.get(i.id);box.setPadding(dp(14),dp(8),dp(14),dp(local||progress!=null?4:8));box.setBackground(rounded(Color.WHITE,16));box.setForeground(null);box.setAlpha(deletingItems.contains(i.id)?.45f:1f);if(progress!=null){ProgressBar line=new ProgressBar(MainActivity.this,null,android.R.attr.progressBarStyleHorizontal);line.setMax(100);line.setProgress(progress);line.setProgressTintList(android.content.res.ColorStateList.valueOf(Color.rgb(34,139,70)));line.setProgressBackgroundTintList(android.content.res.ColorStateList.valueOf(Color.rgb(226,235,228)));box.addView(line,new LinearLayout.LayoutParams(-1,dp(4)));}else if(local){View line=new View(MainActivity.this);line.setBackgroundColor(Color.rgb(34,139,70));box.addView(line,new LinearLayout.LayoutParams(-1,dp(3)));}
            } else box.addView(detail);
            box.setOnClickListener(v->{if(i.type.equals("file")){if(!prefs.getString("downloaded_"+i.id,"").isEmpty())openLocal(i);}else openItem(i);});box.setOnLongClickListener(v->{actions(i);return true;});
            if(!i.type.equals("text"))return box;
            FrameLayout frame=new FrameLayout(MainActivity.this);frame.addView(box,new FrameLayout.LayoutParams(-1,-2));
            TextView star=text(i.favorite?"★":"☆",18,i.favorite?Color.rgb(245,183,0):Color.rgb(120,115,122));star.setGravity(Gravity.CENTER);star.setPadding(0,0,0,0);star.setContentDescription(i.favorite?"取消收藏":"收藏");boolean pending=favoritePending.contains(i.id);star.setEnabled(!pending);star.setAlpha(pending?.45f:1f);star.setOnClickListener(v->{v.setEnabled(false);toggleFavorite(i);});FrameLayout.LayoutParams sp=new FrameLayout.LayoutParams(dp(36),dp(36),Gravity.END|Gravity.BOTTOM);frame.addView(star,sp);return frame;
        }
    }

    private void toggleFavorite(Item item){if(!favoritePending.add(item.id))return;boolean desired=!item.favorite;String requestBase=activeBase;int requestGeneration=serverGeneration;metadataIo.execute(()->{try{HttpURLConnection c=connection(activeNetwork,requestBase+"/favorite/"+path(item.id),7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");c.getOutputStream().write(("favorite="+desired).getBytes(StandardCharsets.UTF_8));JSONObject result=new JSONObject(read(c));Item updated=Item.from(result.getJSONObject("item"));runOnUiThread(()->{favoritePending.remove(item.id);if(requestGeneration!=serverGeneration)return;applyRemoteItem(item.id,updated);list.setAlpha(.72f);list.animate().alpha(1f).setDuration(230).start();});}catch(Exception e){runOnUiThread(()->{favoritePending.remove(item.id);renderSection();toast("收藏操作失败："+e.getMessage());});}});}

    private String preview(String s){int limit=prefs.getInt("preview",600);int[] cp=s.codePoints().toArray();return cp.length<=limit?s:new String(cp,0,limit)+"… 点击查看全文";}
    private String formatSize(long b){if(b<1024)return b+" B";if(b<1048576)return String.format(Locale.CHINA,"%.1f KB",b/1024d);if(b<1073741824)return String.format(Locale.CHINA,"%.1f MB",b/1048576d);return String.format(Locale.CHINA,"%.2f GB",b/1073741824d);}
    private boolean isImageName(String name){String n=name==null?"":name.toLowerCase(Locale.ROOT);return n.endsWith(".jpg")||n.endsWith(".jpeg")||n.endsWith(".png")||n.endsWith(".gif")||n.endsWith(".webp")||n.endsWith(".bmp");}
    private void loadThumbnail(Item item,ImageView view){thumbnailIo.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+"/thumbnail/"+path(item.id),7000);try(InputStream in=c.getInputStream()){Bitmap b=BitmapFactory.decodeStream(in);if(b!=null){thumbnailCache.put(item.id,b);runOnUiThread(()->{if(item.id.equals(view.getTag()))view.setImageBitmap(b);});}}}catch(Exception ignored){}});}

    private void openItem(Item i) {
        if(i.type.equals("text")) new AlertDialog.Builder(this).setTitle(i.filename).setMessage(i.content).setPositiveButton("复制",(d,w)->copy(i.content)).setNeutralButton("编辑",(d,w)->editText(i)).setNegativeButton("关闭",null).show();
        else if(i.type.equals("link")) { try{startActivity(new Intent(Intent.ACTION_VIEW,Uri.parse(i.content)));}catch(Exception e){toast("无法打开链接");} }
        else download(i,true);
    }

    private void actions(Item i) {
        String[] a=i.type.equals("text")?new String[]{"查看","复制","编辑","重命名","删除 NAS 内容"}:i.type.equals("file")?new String[]{"下载","打开","🔗  复制地址","重命名","⌫  删除手机本地副本","🗑  删除 NAS 文件"}:new String[]{"打开","复制地址","重命名","删除 NAS 内容"};
        boolean fileOpenEnabled=!i.type.equals("file")||isLocalAvailable(i);
        ArrayAdapter<String> menuAdapter=new ArrayAdapter<String>(this,android.R.layout.simple_list_item_1,a){@Override public boolean isEnabled(int position){String action=a[position];return !((action.contains("打开")||action.contains("删除手机本地副本"))&&!fileOpenEnabled);}@Override public View getView(int position,View convert,android.view.ViewGroup parent){TextView view=(TextView)super.getView(position,convert,parent);boolean enabled=isEnabled(position);int color=enabled?Color.rgb(75,151,174):Color.LTGRAY;view.setEnabled(enabled);view.setTextColor(color);view.setCompoundDrawablePadding(dp(16));view.setCompoundDrawables(null,null,null,null);if(i.type.equals("file")&&(position==0||position==1||position==3)){int iconId=position==0?R.drawable.ic_action_download:position==1?R.drawable.ic_action_view:R.drawable.ic_action_rename;Drawable icon=getDrawable(iconId);icon.setBounds(0,0,dp(18),dp(18));icon.setTint(color);view.setCompoundDrawables(icon,null,null,null);}else if(i.type.equals("text")||i.type.equals("link")){boolean first=i.type.equals("text")&&position==0;boolean open=i.type.equals("link")&&position==0;boolean copy=(i.type.equals("text")&&position==1)||(i.type.equals("link")&&position==1);boolean edit=i.type.equals("text")&&position==2;boolean rename=(i.type.equals("text")&&position==3)||(i.type.equals("link")&&position==2);boolean remove=(i.type.equals("text")&&position==4)||(i.type.equals("link")&&position==3);int iconId=(open?R.drawable.ic_action_external_link:(first?R.drawable.ic_action_view:(i.type.equals("text")&&copy?R.drawable.ic_action_copy:(edit?R.drawable.ic_action_edit:(rename?R.drawable.ic_action_rename:0)))));if((i.type.equals("link")&&copy)||remove){view.setText((copy?"🔗  ":"🗑  ")+view.getText());}else if(iconId!=0){Drawable icon=getDrawable(iconId);icon.setBounds(0,0,dp(18),dp(18));icon.setTint(color);view.setCompoundDrawables(icon,null,null,null);}}return view;}};
        new AlertDialog.Builder(this).setTitle(i.filename).setAdapter(menuAdapter,(d,w)->{
            String x=a[w]; if(x.contains("查看"))openItem(i); else if(x.contains("下载")){download(i,false);} else if(x.contains("打开")){if(i.type.equals("file"))openLocal(i);else openItem(i);} else if(x.contains("复制"))copy(i.type.equals("text")?i.content:i.type.equals("file")?activeBase+"/view/"+path(i.id):i.content); else if(x.contains("编辑"))editText(i); else if(x.contains("重命名"))rename(i); else if(x.contains("删除手机本地副本"))deleteLocalCopy(i); else delete(i);
        }).show();
    }

    private void addCurrent() {
        if(section.equals("file")){fileUploadForm();}
        else if(section.equals("text")) textForm(null); else if(section.equals("link")) linkForm();
    }

    private EditText input(String hint,boolean multi){EditText e=new EditText(this);e.setHint(hint);if(multi){e.setMinLines(5);e.setGravity(Gravity.TOP);}return e;}
    private void textForm(Item existing) {
        LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);EditText name=input("标题（可选）",false),body=input("正文",true);box.addView(name);box.addView(body);
        Spinner expiry=new Spinner(this);String[] expiryLabels={"永不过期","1 小时","4 小时","1 天"};String[] expiryValues={"Never","1 hour","4 hours","1 day"};ArrayAdapter<String> expiryAdapter=new ArrayAdapter<>(this,android.R.layout.simple_spinner_dropdown_item,expiryLabels);expiry.setAdapter(expiryAdapter);LinearLayout expiryRow=new LinearLayout(this);expiryRow.setGravity(Gravity.CENTER_VERTICAL);TextView expiryHint=text("保存时间",14,Color.DKGRAY);expiryRow.addView(expiryHint,new LinearLayout.LayoutParams(0,-2,1));expiryRow.addView(expiry,new LinearLayout.LayoutParams(dp(150),-2));box.addView(expiryRow);
        if(existing!=null){name.setText(existing.filename);name.setEnabled(false);body.setText(existing.content);}
        new AlertDialog.Builder(this).setTitle(existing==null?"新建文字":"编辑文字").setView(box).setPositiveButton("保存",(d,w)->{if(existing==null)postForm("/submit",map("name",name.getText().toString(),"content",body.getText().toString(),"expiry",expiryValues[expiry.getSelectedItemPosition()]));else postForm("/edit/"+path(existing.id),map("content",body.getText().toString()));}).setNegativeButton("取消",null).show();
    }
    private void editText(Item i){textForm(i);}
    private void linkForm(){LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);EditText n=input("标题",false),u=input("https://example.com",false);box.addView(n);box.addView(u);new AlertDialog.Builder(this).setTitle("新建链接").setView(box).setPositiveButton("保存",(d,w)->postForm("/submit",map("type","link","name",n.getText().toString(),"content",u.getText().toString()))).setNegativeButton("取消",null).show();}
    private void fileUploadForm(){LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);Spinner expiry=new Spinner(this);String[] labels={"永不过期","1 小时","4 小时","1 天"};String[] values={"Never","1 hour","4 hours","1 day"};expiry.setAdapter(new ArrayAdapter<>(this,android.R.layout.simple_spinner_dropdown_item,labels));box.addView(text("保存时间",14,Color.DKGRAY));box.addView(expiry);new AlertDialog.Builder(this).setTitle("添加文件").setView(box).setPositiveButton("选择文件",(d,w)->{pendingExpiry=values[expiry.getSelectedItemPosition()];Intent x=new Intent(Intent.ACTION_OPEN_DOCUMENT);x.setType("*/*");x.addCategory(Intent.CATEGORY_OPENABLE);x.putExtra(Intent.EXTRA_ALLOW_MULTIPLE,true);startActivityForResult(x,PICK_FILE);}).setNeutralButton("从 URL 下载",(d,w)->urlDownloadForm(values[expiry.getSelectedItemPosition()])).setNegativeButton("取消",null).show();}
    private void urlDownloadForm(String expiry){LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);EditText u=input("https://example.com/file.zip",false),n=input("保存名称（可选）",false);box.addView(u);box.addView(n);new AlertDialog.Builder(this).setTitle("从 URL 下载").setView(box).setPositiveButton("下载",(d,w)->downloadURLToNAS(u.getText().toString(),n.getText().toString(),expiry)).setNegativeButton("取消",null).show();}
    private Map<String,String> map(String...x){Map<String,String>m=new LinkedHashMap<>();for(int i=0;i<x.length;i+=2)m.put(x[i],x[i+1]);return m;}

    private void rename(Item i){EditText e=input("新名称",false);e.setText(i.filename);new AlertDialog.Builder(this).setTitle("重命名").setView(e).setPositiveButton("保存",(d,w)->postForm("/rename/"+path(i.id),map("newname",e.getText().toString()))).setNegativeButton("取消",null).show();}
    private void delete(Item i){if(!deletingItems.add(i.id))return;renderSection();metadataIo.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+"/delete/"+path(i.id),7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");c.getOutputStream().write(new byte[0]);read(c);runOnUiThread(()->{applyRemoteDelete(i.id);toast("已删除 NAS 文件");});}catch(Exception e){runOnUiThread(()->{deletingItems.remove(i.id);renderSection();setStatus("删除失败 · "+e.getMessage());});}});}
    private void deleteLocalCopy(Item item){String key="downloaded_"+item.id;String saved=prefs.getString(key,"");if(saved.isEmpty()){renderSection();return;}try{Uri uri=Uri.parse(saved);int deleted=getContentResolver().delete(uri,null,null);if(deleted<=0){try(android.os.ParcelFileDescriptor fd=getContentResolver().openFileDescriptor(uri,"r")){if(fd!=null)throw new IOException("系统未允许删除该文件");}}prefs.edit().remove(key).apply();toast("已删除手机本地副本");renderSection();}catch(Exception error){toast("删除本地副本失败："+error.getMessage());}}

    private void postForm(String endpoint,Map<String,String> values) {
        setStatus("正在保存…");metadataIo.execute(()->{try{StringBuilder b=new StringBuilder();for(Map.Entry<String,String>e:values.entrySet()){if(b.length()>0)b.append('&');b.append(URLEncoder.encode(e.getKey(),"UTF-8")).append('=').append(URLEncoder.encode(e.getValue(),"UTF-8"));}HttpURLConnection c=connection(activeNetwork,activeBase+endpoint,7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");c.getOutputStream().write(b.toString().getBytes(StandardCharsets.UTF_8));read(c);runOnUiThread(()->{toast("已保存");refresh();});}catch(Exception e){setStatus("操作失败 · "+e.getMessage());}});
    }

    private String formBody(Map<String,String> values)throws Exception{StringBuilder b=new StringBuilder();for(Map.Entry<String,String>e:values.entrySet()){if(b.length()>0)b.append('&');b.append(URLEncoder.encode(e.getKey(),"UTF-8")).append('=').append(URLEncoder.encode(e.getValue(),"UTF-8"));}return b.toString();}

    private void downloadURLToNAS(String url,String name,String expiry){
        TransferUi ui=new TransferUi("NAS 从 URL 下载");
        transferIo.execute(()->{try{
            HttpURLConnection create=connection(activeNetwork,activeBase+"/api/v1/download-tasks",10000);ui.connection=create;create.setRequestMethod("POST");create.setDoOutput(true);create.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");create.getOutputStream().write(formBody(map("url",url,"name",name,"expiry",expiry)).getBytes(StandardCharsets.UTF_8));
            JSONObject task=new JSONObject(read(create));ui.serverTaskId=task.getString("id");
            while(!ui.cancelled){
                HttpURLConnection poll=connection(activeNetwork,activeBase+"/api/v1/download-tasks/"+ui.serverTaskId,7000);ui.connection=poll;task=new JSONObject(read(poll));String state=task.optString("status");ui.update("NAS 正在下载",task.optLong("received"),task.optLong("total",-1));
                if(state.equals("completed")){ui.connection=null;ui.serverTaskId=null;ui.success("下载完成，已加入 Files");refresh();return;}
                if(state.equals("failed"))throw new IOException(task.optString("error","NAS 下载失败"));
                if(state.equals("cancelled"))throw new InterruptedIOException("下载已取消");
                Thread.sleep(500);
            }
            throw new InterruptedIOException("下载已取消");
        }catch(Exception e){ui.fail(e);}});
    }

    @Override protected void onActivityResult(int request,int result,Intent data){super.onActivityResult(request,result,data);if(request==PICK_FILE&&result==RESULT_OK&&data!=null){if(data.getData()!=null)takePersistable(data.getData(),data);if(data.getClipData()!=null){for(int n=0;n<data.getClipData().getItemCount();n++){Uri u=data.getClipData().getItemAt(n).getUri();takePersistable(u,data);upload(u);}}else if(data.getData()!=null)upload(data.getData());}}
    private String displayName(Uri uri){String name=null;try(Cursor c=getContentResolver().query(uri,new String[]{android.provider.OpenableColumns.DISPLAY_NAME},null,null,null)){if(c!=null&&c.moveToFirst())name=c.getString(0);}catch(Exception ignored){}if(name==null||name.trim().isEmpty())name="upload";if(name.lastIndexOf('.')<=0){String type=getContentResolver().getType(uri);String extension=type==null?null:MimeTypeMap.getSingleton().getExtensionFromMimeType(type.toLowerCase(Locale.ROOT));if(extension!=null&&!extension.isEmpty())name+="."+extension;}return name;}
    private void upload(Uri uri){stageAndUpload(uri);}
    private void stageAndUpload(Uri uri){String name=displayName(uri);UploadTaskUi task=new UploadTaskUi(name);toast("已加入上传任务："+name);transferIo.execute(()->{File temp=null;try{task.preparing();File dir=new File(getCacheDir(),"uploads");dir.mkdirs();temp=File.createTempFile("upload-",".bin",dir);try(InputStream in=getContentResolver().openInputStream(uri);OutputStream out=new FileOutputStream(temp)){if(in==null)throw new IOException("无法读取文件");byte[]b=new byte[65536];int n;while((n=in.read(b))>0)out.write(b,0,n);}uploadFile(temp,name,task);}catch(Exception e){if(temp!=null)temp.delete();task.failed(name,e.getMessage());}});}
    private void uploadFile(File file,String name,UploadTaskUi task){String idempotencyKey=UUID.randomUUID().toString();for(int attempt=1;attempt<=3;attempt++){try{String boundary="----ContentTransfer"+System.currentTimeMillis();HttpURLConnection c=connection(activeNetwork,activeBase+"/upload-stream",30000);c.setReadTimeout(300000);c.setRequestMethod("POST");c.setDoOutput(true);c.setChunkedStreamingMode(65536);c.setRequestProperty("Content-Type","multipart/form-data; boundary="+boundary);c.setRequestProperty("Idempotency-Key",idempotencyKey);OutputStream out=c.getOutputStream();String safeName=name.replace("\"","").replace("\r"," ").replace("\n"," ");String head="--"+boundary+"\r\nContent-Disposition: form-data; name=\"expiry\"\r\n\r\n"+pendingExpiry+"\r\n--"+boundary+"\r\nContent-Disposition: form-data; name=\"file-upload\"; filename=\""+safeName+"\"\r\nContent-Type: application/octet-stream\r\n\r\n";out.write(head.getBytes(StandardCharsets.UTF_8));long total=file.length(),sent=0;try(InputStream in=new FileInputStream(file)){byte[]buf=new byte[65536];int n;while((n=in.read(buf))>0){out.write(buf,0,n);sent+=n;task.uploading(name,sent,total);}}out.write(("\r\n--"+boundary+"--\r\n").getBytes(StandardCharsets.UTF_8));out.close();task.waiting(name);read(c);file.delete();task.complete(name);runOnUiThread(this::refresh);return;}catch(Exception e){if(attempt==3){file.delete();task.failed(name,e.getMessage());}}}}

    private String downloadName(String filename){
        if(filename==null||filename.trim().isEmpty())return "download.bin";
        String name=filename;
        while(name.toLowerCase(Locale.ROOT).endsWith(".apk.zip"))name=name.substring(0,name.length()-4);
        return name;
    }
    private String mimeType(String filename,String fallback){
        String lower=filename==null?"":filename.toLowerCase(Locale.ROOT);
        if(lower.endsWith(".apk"))return "application/vnd.android.package-archive";
        int dot=lower.lastIndexOf('.');
        if(dot>=0&&dot<lower.length()-1){String detected=MimeTypeMap.getSingleton().getMimeTypeFromExtension(lower.substring(dot+1));if(detected!=null)return detected;}
        return fallback==null||fallback.isEmpty()?"application/octet-stream":fallback;
    }
    private void download(Item item,boolean open){if(downloadProgress.putIfAbsent(item.id,0)!=null){toast("该文件正在下载");return;}renderSection();transferIo.execute(()->{Uri uri=null;try{HttpURLConnection c=connection(activeNetwork,activeBase+"/download/"+path(item.id),15000);c.setReadTimeout(120000);long total=c.getContentLengthLong();String name=downloadName(item.filename);String mime=mimeType(name,null);ContentValues v=new ContentValues();v.put(MediaStore.Downloads.DISPLAY_NAME,name);v.put(MediaStore.Downloads.MIME_TYPE,mime);v.put(MediaStore.Downloads.IS_PENDING,1);uri=getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI,v);if(uri==null)throw new IOException("无法创建下载文件");long received=0;int shown=0;try(InputStream in=c.getInputStream();OutputStream out=getContentResolver().openOutputStream(uri)){if(out==null)throw new IOException("无法写入下载文件");byte[]b=new byte[65536];int n;while((n=in.read(b))>0){out.write(b,0,n);received+=n;int percent=total>0?(int)Math.min(99,received*100/total):Math.min(99,shown+1);if(percent!=shown){shown=percent;downloadProgress.put(item.id,percent);runOnUiThread(()->adapter.notifyDataSetChanged());}}}v.clear();v.put(MediaStore.Downloads.DISPLAY_NAME,name);v.put(MediaStore.Downloads.MIME_TYPE,mime);v.put(MediaStore.Downloads.IS_PENDING,0);getContentResolver().update(uri,v,null,null);prefs.edit().putString("downloaded_"+item.id,uri.toString()).apply();downloadProgress.remove(item.id);runOnUiThread(()->{renderSection();toast("已保存到下载目录");if(open)openLocal(item);});}catch(Exception e){if(uri!=null)getContentResolver().delete(uri,null,null);downloadProgress.remove(item.id);runOnUiThread(()->{renderSection();setStatus("下载失败 · "+e.getMessage());});}});}
    private boolean isLocalAvailable(Item item){String key="downloaded_"+item.id;String saved=prefs.getString(key,"");if(saved.isEmpty())return false;try(android.os.ParcelFileDescriptor fd=getContentResolver().openFileDescriptor(Uri.parse(saved),"r")){if(fd!=null)return true;}catch(Exception ignored){}prefs.edit().remove(key).apply();return false;}
    private void openLocal(Item item){String saved=prefs.getString("downloaded_"+item.id,"");if(!isLocalAvailable(item)){toast("本地文件已不存在，请重新下载");renderSection();return;}String mime=mimeType(downloadName(item.filename),"*/*");try{if(mime.equals("application/vnd.android.package-archive")&&Build.VERSION.SDK_INT>=26&&!getPackageManager().canRequestPackageInstalls()){toast("请允许内容中转安装未知应用，然后再次点击打开");startActivity(new Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,Uri.parse("package:"+getPackageName())));return;}Intent x=new Intent(Intent.ACTION_VIEW);x.setDataAndType(Uri.parse(saved),mime);x.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION|Intent.FLAG_ACTIVITY_NEW_TASK);startActivity(x);}catch(Exception e){toast("无法打开文件 · "+e.getMessage());}}

    private void showNotepad(){
        if(notepad!=null)return;
        if(list.getParent()!=null)root.removeView(list);
        notepad=input("记事本",true);
        notepadSavedText=prefs.getString("notepad","");
        updatingNotepad=true;notepad.setText(notepadSavedText);updatingNotepad=false;
        notepad.addTextChangedListener(new TextWatcher(){public void beforeTextChanged(CharSequence s,int a,int b,int c){}public void onTextChanged(CharSequence s,int a,int b,int c){if(!updatingNotepad){notepadDirty=!s.toString().equals(notepadSavedText);updateNotepadButtons();}}public void afterTextChanged(Editable e){}});
        root.addView(notepad,new LinearLayout.LayoutParams(-1,0,1));
        notepadPreview=text("",16,Color.rgb(45,39,49));notepadPreview.setTextIsSelectable(true);notepadPreview.setMovementMethod(LinkMovementMethod.getInstance());notepadPreview.setPadding(dp(18),dp(16),dp(18),dp(20));
        notepadPreviewScroll=new ScrollView(this);notepadPreviewScroll.addView(notepadPreview);notepadPreviewScroll.setVisibility(View.GONE);root.addView(notepadPreviewScroll,new LinearLayout.LayoutParams(-1,0,1));
        notepadActions=new LinearLayout(this);notepadActions.setWeightSum(2);notepadActions.setPadding(0,dp(8),0,dp(8));
        notepadRead=actionChip("阅读",false);notepadRead.setOnClickListener(v->setNotepadReading(!notepadReading));notepadActions.addView(notepadRead,new LinearLayout.LayoutParams(0,dp(48),1));
        notepadSave=actionChip("保存",true);notepadSave.setOnClickListener(v->saveNotepad());LinearLayout.LayoutParams saveParams=new LinearLayout.LayoutParams(0,dp(48),1);saveParams.setMarginStart(dp(10));notepadActions.addView(notepadSave,saveParams);root.addView(notepadActions);
        notepadReading=false;notepadDirty=false;updateNotepadButtons();
        metadataIo.execute(()->{try{String s=read(connection(activeNetwork,activeBase+"/notepad/md.file",7000));prefs.edit().putString("notepad",s).apply();runOnUiThread(()->{if(notepad!=null&&!notepad.hasFocus()&&!notepadDirty){notepadSavedText=s;updatingNotepad=true;notepad.setText(s);updatingNotepad=false;updateNotepadButtons();}});}catch(Exception ignored){}});
    }

    private void setNotepadReading(boolean reading){notepadReading=reading;if(reading){notepadPreview.setText(MarkdownRenderer.render(notepad.getText().toString()));notepad.setVisibility(View.GONE);notepadPreviewScroll.setVisibility(View.VISIBLE);}else{notepadPreviewScroll.setVisibility(View.GONE);notepad.setVisibility(View.VISIBLE);notepad.requestFocus();}updateNotepadButtons();}
    private void updateNotepadButtons(){if(notepadRead==null)return;styleStateButton(notepadRead,notepadReading);styleStateButton(notepadSave,!notepadDirty);notepadSave.setContentDescription(notepadDirty?"保存，存在未保存修改":"保存，所有修改已保存");}
    private void styleStateButton(TextView view,boolean pressed){view.setTextColor(pressed?Color.WHITE:Color.rgb(73,62,80));view.setBackground(rounded(pressed?Color.rgb(103,80,164):Color.rgb(235,229,239),20));}
    private void saveNotepad(){if(!notepadDirty){toast("所有修改均已保存");return;}String value=notepad.getText().toString();metadataIo.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+"/notepad/md.file",7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","text/plain; charset=utf-8");c.getOutputStream().write(value.getBytes(StandardCharsets.UTF_8));read(c);prefs.edit().putString("notepad",value).apply();runOnUiThread(()->{notepadSavedText=value;notepadDirty=!notepad.getText().toString().equals(value);updateNotepadButtons();toast("记事本已保存");});}catch(Exception e){setStatus("保存失败 · "+e.getMessage());}});}


    private void chooseExpiry(Runnable next){String[] labels={"永不过期","1 小时","4 小时","1 天","自定义"};String[] values={"Never","1 hour","4 hours","1 day","Custom"};new AlertDialog.Builder(this).setTitle("过期时间").setItems(labels,(d,w)->{if(w==4){EditText e=input("例如 30m、2d、1w",false);new AlertDialog.Builder(this).setTitle("自定义过期时间").setView(e).setPositiveButton("继续",(a,b)->{pendingExpiry=e.getText().toString().trim();if(pendingExpiry.isEmpty())pendingExpiry="Never";next.run();}).setNegativeButton("取消",null).show();}else{pendingExpiry=values[w];next.run();}}).setNegativeButton("取消",null).show();}

    private void showSettings(){LinearLayout box=new LinearLayout(this);box.setOrientation(LinearLayout.VERTICAL);box.setPadding(dp(24),0,dp(24),0);TextView server=button("配置服务器地址");server.setOnClickListener(v->showServerConfig(false));box.addView(server);box.addView(text("Snippet 预览字数",14,Color.DKGRAY));EditText e=input("1 至 100000",false);e.setInputType(android.text.InputType.TYPE_CLASS_NUMBER);e.setText(String.valueOf(prefs.getInt("preview",600)));box.addView(e);new AlertDialog.Builder(this).setTitle("设置").setView(box).setPositiveButton("保存",(d,w)->{try{int n=Integer.parseInt(e.getText().toString());if(n<1||n>100000)throw new Exception();prefs.edit().putInt("preview",n).apply();renderSection();}catch(Exception x){toast("请输入 1 至 100000");}}).setNeutralButton("关于",(d,w)->showAbout()).setNegativeButton("取消",null).show();}
    private void showServerConfig(boolean required){EditText field=input("例如 http://192.168.1.10:8084/",false);field.setInputType(android.text.InputType.TYPE_CLASS_TEXT|android.text.InputType.TYPE_TEXT_VARIATION_URI);field.setText(activeBase);AlertDialog dialog=new AlertDialog.Builder(this).setTitle("配置服务器地址").setMessage("请输入包含 http:// 或 https:// 的服务地址").setView(field).setPositiveButton("保存",null).setNegativeButton(required?null:"取消",null).setCancelable(!required).create();dialog.setOnShowListener(x->dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener(v->{String normalized=ServerConfig.normalize(field.getText().toString());if(normalized.isEmpty()){field.setError("请输入有效的 http:// 或 https:// 地址");return;}if(!ServerConfig.save(this,normalized)){field.setError("保存失败");return;}stopRealtimeUpdates();serverGeneration++;lastEventSequence=-1;mainHandler.removeCallbacks(realtimeRefresh);allItems.clear();visibleItems.clear();adapter.notifyDataSetChanged();thumbnailCache.evictAll();activeBase=normalized;status.setText("正在连接新服务器…");dialog.dismiss();refresh();startRealtimeUpdates();}));dialog.show();}
    private void showAbout(){String changes="1.0.24\n• 修复特殊字符文件名的访问与上传编码\n• 限制超大剪贴板文本，避免辅助服务阻塞\n\n1.0.23\n• 复制纯文本后显示右侧快捷上传圆钮\n• 点击圆钮发送到 Snippets，1.5 秒未点击自动消失\n• 微信分享的无后缀图片会按 MIME 类型补充扩展名\n\n1.0.22\n• 手机下载实时显示进度，可取消并自动清理失败文件\n• NAS 从 URL 下载显示真实进度，支持取消和失败状态\n\n1.0.21\n• 手机端即时同步其它设备上的新增、修改和删除\n• 返回前台时自动同步最新内容\n\n1.0.20\n• 上传失败自动重试三次并显示具体原因\n\n1.0.19\n• 分享上传和 App 内上传统一先复制到本地缓存\n• 大文件上传显示文件名和实时百分比\n\n1.0.18\n• 修复系统分享大文件时来源 URI 过早失效的问题\n\n1.0.17\n• 文件来源按钮统一改名为从 URL 下载\n\n1.0.16\n• Files 新增添加 URL，由 NAS 直接下载公网文件\n\n1.0.15\n• 缩略图改由 NAS 生成并传输小图\n\n1.0.14\n• 修复列表滚动后图片缩略图反复消失并重新下载的问题\n\n1.0.13\n• Files 中的图片文件显示按比例缩放的缩略图\n\n1.0.12\n• 同步状态仅显示局域网或外网\n\n1.0.11\n• 同步网址后显示当前使用局域网还是外网\n• 关于页面补齐版本更新记录\n\n1.0.10\n• 未下载或本地已删除的文件灰显打开选项\n• 删除本地文件后自动清除绿色状态线\n• 增加 APK 安装权限检查与授权引导\n\n1.0.9\n• Files 操作恢复到长按菜单，下载与打开拆分\n• 强制纠正 APK 的文件名和 MIME 类型\n\n1.0.8\n• 修复 APK 下载后被追加 .zip 后缀\n• 设置中增加关于与版本记录\n\n1.0.7\n• 统一使用 配置的服务器域名，由 DNS 完成内外网分流\n\n1.0.6\n• Files 上传增加保存时间\n• 本地文件使用底部绿色粗线标记\n\n1.0.5\n• 新建文字默认永不过期，可直接选择保存时间\n• Files 分离下载和打开按钮，下载后才可打开\n\n1.0.4\n• 根据 本地网关判断家庭网络\n\n1.0.3\n• 增加下拉刷新并保留刷新按钮\n\n1.0.2\n• 支持从系统分享面板上传单个或多个文件\n\n1.0.1\n• 修复 Android 16 状态栏遮挡并优化顶部界面\n\n1.0.0\n• 首个 Android 原生版本，支持文字、文件、链接和记事本";TextView content=text(changes,14,Color.rgb(45,39,49));content.setTextIsSelectable(true);ScrollView scroll=new ScrollView(this);scroll.setPadding(dp(12),0,dp(12),0);scroll.addView(content);new AlertDialog.Builder(this).setTitle("内容中转  " + appVersion()).setView(scroll).setPositiveButton("关闭",null).show();}
    private String appVersion(){try{return getPackageManager().getPackageInfo(getPackageName(),0).versionName;}catch(Exception ignored){return "";}}
    private void postRaw(String endpoint,String value){metadataIo.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+endpoint,7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","text/plain; charset=utf-8");c.getOutputStream().write(value.getBytes(StandardCharsets.UTF_8));read(c);runOnUiThread(()->toast("已保存"));}catch(Exception e){setStatus("保存失败 · "+e.getMessage());}});}

    private String path(String id){return Uri.encode(id,"/");}
    private void copy(String s){((android.content.ClipboardManager)getSystemService(CLIPBOARD_SERVICE)).setPrimaryClip(ClipData.newPlainText("内容中转",s));toast("已复制");}
    private void toast(String s){Toast.makeText(this,s,Toast.LENGTH_SHORT).show();}
    @Override protected void onDestroy(){stopRealtimeUpdates();metadataIo.shutdownNow();transferIo.shutdownNow();thumbnailIo.shutdownNow();realtimeIo.shutdownNow();super.onDestroy();}
}
