package ink.yode.contenttransfer;

import android.app.*;
import android.content.*;
import android.database.Cursor;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.*;
import android.os.*;
import android.provider.MediaStore;
import android.view.*;
import android.widget.*;

import org.json.*;

import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.text.DateFormat;
import java.util.*;
import java.util.concurrent.*;

public class MainActivity extends Activity {
    private static final String LAN_BASE = "http://192.168.3.177:8084";
    private static final String REMOTE_BASE = "http://nas.yode.ink:8084";
    private static final int PICK_FILE = 4101;
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final ArrayList<Item> allItems = new ArrayList<>(), visibleItems = new ArrayList<>();
    private SharedPreferences prefs;
    private LinearLayout root, toolbar;
    private TextView status;
    private ListView list;
    private ItemAdapter adapter;
    private String section = "text", activeBase = REMOTE_BASE;
    private Network activeNetwork;
    private EditText notepad;
    private Button notepadSave;
    private String pendingExpiry = "Never";
    private final Map<String, TextView> tabViews = new LinkedHashMap<>();
    private final ArrayList<Uri> pendingSharedFiles = new ArrayList<>();

    static class Item {
        String id, type, filename, content, createdAt, modifiedAt;
        long size;
        static Item from(JSONObject o) {
            Item i = new Item();
            i.id=o.optString("id"); i.type=o.optString("type"); i.filename=o.optString("filename");
            i.content=o.optString("content"); i.createdAt=o.optString("createdAt"); i.modifiedAt=o.optString("modifiedAt"); i.size=o.optLong("size");
            return i;
        }
    }

    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        prefs = getSharedPreferences("content-transfer", MODE_PRIVATE);
        buildUi();
        loadCache();
        collectSharedFiles(getIntent());
        refresh();
    }

    @Override protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        collectSharedFiles(intent);
        if (activeNetwork != null) processSharedFiles(); else refresh();
    }

    private void collectSharedFiles(Intent intent) {
        if (intent == null) return;
        String action = intent.getAction();
        if (Intent.ACTION_SEND.equals(action)) {
            Uri uri = intent.getParcelableExtra(Intent.EXTRA_STREAM);
            if (uri != null) pendingSharedFiles.add(uri);
        } else if (Intent.ACTION_SEND_MULTIPLE.equals(action)) {
            ArrayList<Uri> uris = intent.getParcelableArrayListExtra(Intent.EXTRA_STREAM);
            if (uris != null) pendingSharedFiles.addAll(uris);
        }
        if (!pendingSharedFiles.isEmpty()) {
            section = "file";
            updateTabs();
            renderSection();
            setStatus("已接收 " + pendingSharedFiles.size() + " 个文件，正在连接…");
        }
        intent.setAction(null);
    }

    private void processSharedFiles() {
        if (pendingSharedFiles.isEmpty()) return;
        ArrayList<Uri> files = new ArrayList<>(pendingSharedFiles);
        pendingSharedFiles.clear();
        pendingExpiry = "Never";
        toast("正在上传 " + files.size() + " 个文件到 Files");
        for (Uri uri : files) upload(uri);
    }

    private TextView text(String value, float sp, int color) {
        TextView v = new TextView(this); v.setText(value); v.setTextSize(sp); v.setTextColor(color); v.setPadding(dp(12),dp(10),dp(12),dp(10)); return v;
    }
    private GradientDrawable rounded(int color,int radius) { GradientDrawable d=new GradientDrawable();d.setColor(color);d.setCornerRadius(dp(radius));return d; }
    private Button button(String label) { Button b=new Button(this);b.setText(label);b.setAllCaps(false);b.setTextSize(14);b.setMinHeight(0);b.setMinimumHeight(0);b.setMinWidth(0);b.setMinimumWidth(0);b.setPadding(dp(16),dp(10),dp(16),dp(10));b.setBackground(rounded(Color.rgb(235,229,239),18));return b; }
    private TextView iconButton(String symbol,String description) { TextView v=text(symbol,23,Color.rgb(73,62,80));v.setGravity(Gravity.CENTER);v.setContentDescription(description);v.setBackground(rounded(Color.rgb(235,229,239),22));v.setPadding(0,0,0,0);return v; }
    private TextView actionChip(String label,boolean primary) { TextView v=text(label,14,primary?Color.WHITE:Color.rgb(73,62,80));v.setGravity(Gravity.CENTER);v.setTypeface(Typeface.DEFAULT,Typeface.BOLD);v.setBackground(rounded(primary?Color.rgb(103,80,164):Color.rgb(235,229,239),20));v.setPadding(dp(18),dp(10),dp(18),dp(10));return v; }
    private int dp(int n) { return (int)(n*getResources().getDisplayMetrics().density+.5f); }

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
        list=new ListView(this); adapter=new ItemAdapter(); list.setAdapter(adapter); root.addView(list,new LinearLayout.LayoutParams(-1,0,1));
        setContentView(root);
    }

    private void addTab(String label,String key) {
        TextView b=actionChip(label,false);b.setTextSize(14);b.setOnClickListener(v->{section=key;updateTabs();renderSection();});LinearLayout.LayoutParams p=new LinearLayout.LayoutParams(0,dp(44),1);p.setMargins(dp(3),0,dp(3),0);toolbar.addView(b,p);tabViews.put(key,b);if(key.equals(section))updateTabs();
    }

    private void updateTabs(){for(Map.Entry<String,TextView> e:tabViews.entrySet()){boolean selected=e.getKey().equals(section);e.getValue().setTextColor(selected?Color.WHITE:Color.rgb(73,62,80));e.getValue().setBackground(rounded(selected?Color.rgb(103,80,164):Color.rgb(239,234,242),20));}}

    private void setStatus(String s) { runOnUiThread(()->status.setText(s)); }

    private void loadCache() {
        String raw=prefs.getString("items", "[]");
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
        setStatus("正在同步…");
        io.execute(()->{
            try {
                String raw=null; Network wifi=findNetwork(true);
                if(wifi!=null) try { raw=read(connection(wifi,LAN_BASE+"/api/v1/items",1200)); activeBase=LAN_BASE; activeNetwork=wifi; } catch(Exception ignored) {}
                if(raw==null) { Network net=findNetwork(false); raw=read(connection(net,REMOTE_BASE+"/api/v1/items",7000)); activeBase=REMOTE_BASE; activeNetwork=net; }
                JSONArray data=new JSONArray(raw); prefs.edit().putString("items",data.toString()).apply();
                runOnUiThread(()->{try{parseItems(data);}catch(Exception ignored){} status.setText((activeBase.equals(LAN_BASE)?"局域网直连":"异地服务器")+" · 已同步");processSharedFiles();});
            } catch(Exception e) { setStatus("离线显示缓存 · "+e.getMessage()); runOnUiThread(this::processSharedFiles); }
        });
    }

    private void renderSection() {
        if(section.equals("notepad")){ showNotepad(); return; }
        if(notepad!=null){root.removeView(notepad);notepad=null;if(notepadSave!=null){root.removeView(notepadSave);notepadSave=null;}if(list.getParent()==null)root.addView(list,new LinearLayout.LayoutParams(-1,0,1));}
        visibleItems.clear(); for(Item i:allItems) if(i.type.equals(section)) visibleItems.add(i);
        String key=prefs.getString("sort_"+section,section.equals("file")?"created_desc":"created_desc");
        Comparator<Item> cmp;
        if(key.startsWith("title")) cmp=Comparator.comparing(a->a.filename,java.text.Collator.getInstance(Locale.CHINA));
        else if(key.startsWith("modified")) cmp=Comparator.comparing(a->a.modifiedAt);
        else if(key.startsWith("size")) cmp=Comparator.comparingLong(a->a.size);
        else cmp=Comparator.comparing(a->a.createdAt);
        if(key.endsWith("desc")) cmp=cmp.reversed(); Collections.sort(visibleItems,cmp); adapter.notifyDataSetChanged();
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
            TextView name=text(i.filename,17,Color.rgb(40,35,45));name.setTypeface(null,1);box.addView(name);
            String sub=i.type.equals("text")?preview(i.content):i.type.equals("file")?formatSize(i.size):i.content;
            TextView detail=text(sub,14,Color.DKGRAY);detail.setMaxLines(i.type.equals("text")?5:2);box.addView(detail);
            box.setOnClickListener(v->openItem(i));box.setOnLongClickListener(v->{actions(i);return true;}); return box;
        }
    }

    private String preview(String s){int limit=prefs.getInt("preview",600);int[] cp=s.codePoints().toArray();return cp.length<=limit?s:new String(cp,0,limit)+"… 点击查看全文";}
    private String formatSize(long b){if(b<1024)return b+" B";if(b<1048576)return String.format(Locale.CHINA,"%.1f KB",b/1024d);if(b<1073741824)return String.format(Locale.CHINA,"%.1f MB",b/1048576d);return String.format(Locale.CHINA,"%.2f GB",b/1073741824d);}

    private void openItem(Item i) {
        if(i.type.equals("text")) new AlertDialog.Builder(this).setTitle(i.filename).setMessage(i.content).setPositiveButton("复制",(d,w)->copy(i.content)).setNeutralButton("编辑",(d,w)->editText(i)).setNegativeButton("关闭",null).show();
        else if(i.type.equals("link")) { try{startActivity(new Intent(Intent.ACTION_VIEW,Uri.parse(i.content)));}catch(Exception e){toast("无法打开链接");} }
        else download(i,true);
    }

    private void actions(Item i) {
        String[] a=i.type.equals("text")?new String[]{"查看","复制","编辑","重命名","删除"}:i.type.equals("file")?new String[]{"下载/打开","复制地址","重命名","删除"}:new String[]{"打开","复制地址","重命名","删除"};
        new AlertDialog.Builder(this).setTitle(i.filename).setItems(a,(d,w)->{
            String x=a[w]; if(x.equals("查看")||x.equals("打开")||x.startsWith("下载"))openItem(i); else if(x.startsWith("复制"))copy(i.type.equals("text")?i.content:i.type.equals("file")?activeBase+"/view/"+path(i.id):i.content); else if(x.equals("编辑"))editText(i); else if(x.equals("重命名"))rename(i); else delete(i);
        }).show();
    }

    private void addCurrent() {
        if(section.equals("file")){chooseExpiry(()->{Intent x=new Intent(Intent.ACTION_OPEN_DOCUMENT);x.setType("*/*");x.addCategory(Intent.CATEGORY_OPENABLE);x.putExtra(Intent.EXTRA_ALLOW_MULTIPLE,true);startActivityForResult(x,PICK_FILE);});}
        else if(section.equals("text")) textForm(null); else if(section.equals("link")) linkForm();
    }

    private EditText input(String hint,boolean multi){EditText e=new EditText(this);e.setHint(hint);if(multi){e.setMinLines(5);e.setGravity(Gravity.TOP);}return e;}
    private void textForm(Item existing) {
        LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);EditText name=input("标题（可选）",false),body=input("正文",true);box.addView(name);box.addView(body);
        if(existing!=null){name.setText(existing.filename);name.setEnabled(false);body.setText(existing.content);}
        new AlertDialog.Builder(this).setTitle(existing==null?"新建文字":"编辑文字").setView(box).setPositiveButton("保存",(d,w)->{if(existing==null)chooseExpiry(()->postForm("/submit",map("name",name.getText().toString(),"content",body.getText().toString(),"expiry",pendingExpiry)));else postForm("/edit/"+path(existing.id),map("content",body.getText().toString()));}).setNegativeButton("取消",null).show();
    }
    private void editText(Item i){textForm(i);}
    private void linkForm(){LinearLayout box=new LinearLayout(this);box.setPadding(dp(18),0,dp(18),0);box.setOrientation(LinearLayout.VERTICAL);EditText n=input("标题",false),u=input("https://example.com",false);box.addView(n);box.addView(u);new AlertDialog.Builder(this).setTitle("新建链接").setView(box).setPositiveButton("保存",(d,w)->postForm("/submit",map("type","link","name",n.getText().toString(),"content",u.getText().toString()))).setNegativeButton("取消",null).show();}
    private Map<String,String> map(String...x){Map<String,String>m=new LinkedHashMap<>();for(int i=0;i<x.length;i+=2)m.put(x[i],x[i+1]);return m;}

    private void rename(Item i){EditText e=input("新名称",false);e.setText(i.filename);new AlertDialog.Builder(this).setTitle("重命名").setView(e).setPositiveButton("保存",(d,w)->postForm("/rename/"+path(i.id),map("newname",e.getText().toString()))).setNegativeButton("取消",null).show();}
    private void delete(Item i){new AlertDialog.Builder(this).setTitle("确认删除").setMessage("将从 NAS 永久删除“"+i.filename+"”，无法撤销。").setPositiveButton("删除",(d,w)->postForm("/delete/"+path(i.id),Collections.emptyMap())).setNegativeButton("取消",null).show();}

    private void postForm(String endpoint,Map<String,String> values) {
        setStatus("正在保存…");io.execute(()->{try{StringBuilder b=new StringBuilder();for(Map.Entry<String,String>e:values.entrySet()){if(b.length()>0)b.append('&');b.append(URLEncoder.encode(e.getKey(),"UTF-8")).append('=').append(URLEncoder.encode(e.getValue(),"UTF-8"));}HttpURLConnection c=connection(activeNetwork,activeBase+endpoint,7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");c.getOutputStream().write(b.toString().getBytes(StandardCharsets.UTF_8));read(c);runOnUiThread(()->{toast("已保存");refresh();});}catch(Exception e){setStatus("操作失败 · "+e.getMessage());}});
    }

    @Override protected void onActivityResult(int request,int result,Intent data){super.onActivityResult(request,result,data);if(request==PICK_FILE&&result==RESULT_OK&&data!=null){if(data.getClipData()!=null){for(int n=0;n<data.getClipData().getItemCount();n++)upload(data.getClipData().getItemAt(n).getUri());}else if(data.getData()!=null)upload(data.getData());}}
    private String displayName(Uri uri){try(Cursor c=getContentResolver().query(uri,new String[]{android.provider.OpenableColumns.DISPLAY_NAME},null,null,null)){if(c!=null&&c.moveToFirst())return c.getString(0);}return "upload.bin";}
    private void upload(Uri uri){setStatus("正在上传…");io.execute(()->{String boundary="----ContentTransfer"+System.currentTimeMillis();try{HttpURLConnection c=connection(activeNetwork,activeBase+"/submit",15000);c.setReadTimeout(120000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","multipart/form-data; boundary="+boundary);OutputStream out=c.getOutputStream();String name=displayName(uri);String head="--"+boundary+"\r\nContent-Disposition: form-data; name=\"expiry\"\r\n\r\n"+pendingExpiry+"\r\n--"+boundary+"\r\nContent-Disposition: form-data; name=\"file-upload\"; filename=\""+name.replace("\"","")+"\"\r\nContent-Type: application/octet-stream\r\n\r\n";out.write(head.getBytes(StandardCharsets.UTF_8));try(InputStream in=getContentResolver().openInputStream(uri)){byte[]buf=new byte[65536];int n;while((n=in.read(buf))>0)out.write(buf,0,n);}out.write(("\r\n--"+boundary+"--\r\n").getBytes(StandardCharsets.UTF_8));out.close();read(c);runOnUiThread(()->{toast("上传完成");refresh();});}catch(Exception e){setStatus("上传失败 · "+e.getMessage());}});}

    private void download(Item item,boolean open){setStatus("正在下载…");io.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+"/download/"+path(item.id),15000);c.setReadTimeout(120000);ContentValues v=new ContentValues();v.put(MediaStore.Downloads.DISPLAY_NAME,item.filename);v.put(MediaStore.Downloads.MIME_TYPE,c.getContentType()==null?"application/octet-stream":c.getContentType());v.put(MediaStore.Downloads.IS_PENDING,1);Uri uri=getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI,v);try(InputStream in=c.getInputStream();OutputStream out=getContentResolver().openOutputStream(uri)){byte[]b=new byte[65536];int n;while((n=in.read(b))>0)out.write(b,0,n);}v.clear();v.put(MediaStore.Downloads.IS_PENDING,0);getContentResolver().update(uri,v,null,null);runOnUiThread(()->{toast("已保存到下载目录");if(open){Intent x=new Intent(Intent.ACTION_VIEW);x.setDataAndType(uri,c.getContentType());x.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);try{startActivity(x);}catch(Exception ignored){}}});}catch(Exception e){setStatus("下载失败 · "+e.getMessage());}});}

    private void showNotepad(){if(notepad!=null)return;if(list.getParent()!=null)root.removeView(list);notepad=input("记事本",true);notepad.setText(prefs.getString("notepad",""));root.addView(notepad,new LinearLayout.LayoutParams(-1,0,1));notepadSave=button("保存记事本");notepadSave.setOnClickListener(v->{String s=notepad.getText().toString();prefs.edit().putString("notepad",s).apply();postRaw("/notepad/md.file",s);});root.addView(notepadSave);io.execute(()->{try{String s=read(connection(activeNetwork,activeBase+"/notepad/md.file",7000));prefs.edit().putString("notepad",s).apply();runOnUiThread(()->{if(notepad!=null&&!notepad.hasFocus())notepad.setText(s);});}catch(Exception ignored){}});}

    private void chooseExpiry(Runnable next){String[] labels={"永不过期","1 小时","4 小时","1 天","自定义"};String[] values={"Never","1 hour","4 hours","1 day","Custom"};new AlertDialog.Builder(this).setTitle("过期时间").setItems(labels,(d,w)->{if(w==4){EditText e=input("例如 30m、2d、1w",false);new AlertDialog.Builder(this).setTitle("自定义过期时间").setView(e).setPositiveButton("继续",(a,b)->{pendingExpiry=e.getText().toString().trim();if(pendingExpiry.isEmpty())pendingExpiry="Never";next.run();}).setNegativeButton("取消",null).show();}else{pendingExpiry=values[w];next.run();}}).setNegativeButton("取消",null).show();}

    private void showSettings(){EditText e=input("1 至 100000",false);e.setInputType(android.text.InputType.TYPE_CLASS_NUMBER);e.setText(String.valueOf(prefs.getInt("preview",600)));new AlertDialog.Builder(this).setTitle("设置 Snippet 预览字数").setView(e).setPositiveButton("保存",(d,w)->{try{int n=Integer.parseInt(e.getText().toString());if(n<1||n>100000)throw new Exception();prefs.edit().putInt("preview",n).apply();renderSection();}catch(Exception x){toast("请输入 1 至 100000");}}).setNegativeButton("取消",null).show();}
    private void postRaw(String endpoint,String value){io.execute(()->{try{HttpURLConnection c=connection(activeNetwork,activeBase+endpoint,7000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Content-Type","text/plain; charset=utf-8");c.getOutputStream().write(value.getBytes(StandardCharsets.UTF_8));read(c);runOnUiThread(()->toast("已保存"));}catch(Exception e){setStatus("保存失败 · "+e.getMessage());}});}

    private String path(String id){return id.replace(" ","%20").replace("#","%23");}
    private void copy(String s){((android.content.ClipboardManager)getSystemService(CLIPBOARD_SERVICE)).setPrimaryClip(ClipData.newPlainText("内容中转",s));toast("已复制");}
    private void toast(String s){Toast.makeText(this,s,Toast.LENGTH_SHORT).show();}
    @Override protected void onDestroy(){io.shutdownNow();super.onDestroy();}
}
