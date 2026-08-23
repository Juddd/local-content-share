package ink.yode.contenttransfer;

import android.app.job.*;
import android.content.*;
import org.json.*;
import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.*;

/** Network-constrained background drain for the durable mutation and file outboxes. */
public class SyncRetryJobService extends JobService {
    private static final int JOB_ID=6059;
    private final ExecutorService io=Executors.newSingleThreadExecutor();

    public static void schedule(Context context){
        JobScheduler scheduler=context.getSystemService(JobScheduler.class);
        JobInfo info=new JobInfo.Builder(JOB_ID,new ComponentName(context,SyncRetryJobService.class))
                .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY).setPersisted(true)
                .setBackoffCriteria(15000,JobInfo.BACKOFF_POLICY_EXPONENTIAL).build();
        scheduler.schedule(info);
    }

    @Override public boolean onStartJob(JobParameters params){io.execute(()->{boolean retry=false;SyncDatabase db=new SyncDatabase(this);try{for(String server:db.pendingServers())retry|=!drain(db,server);}finally{db.close();jobFinished(params,retry);}});return true;}
    @Override public boolean onStopJob(JobParameters params){return true;}

    private boolean drain(SyncDatabase db,String server){
        if(SyncDatabase.beginOperations())try{while(true){SyncDatabase.Operation op=db.next(server);if(op==null)break;db.syncing(server,op.itemId);try{
            JSONObject payload=new JSONObject(op.payload),values=payload.getJSONObject("values");values.put("expectedRevision",op.baseRevision);
            Result result=post(server+payload.getString("endpoint"),values,op.id);
            if(result.code==409){JSONObject conflict=new JSONObject(result.body);db.conflict(op.id,server,op.itemId,conflict.optJSONObject("item"));continue;}
            if(result.code<200||result.code>=300)throw new IOException("HTTP "+result.code+" "+result.body);
            JSONObject item=null;if(!result.body.trim().isEmpty()){JSONObject response=new JSONObject(result.body);item=response.optJSONObject("item");if(item==null){JSONArray items=response.optJSONArray("items");if(items!=null&&items.length()>0)item=items.optJSONObject(0);}}
            db.complete(op.id,server,op.itemId,item);
        }catch(Exception error){db.retry(op.id,error.getMessage());return false;}}}finally{SyncDatabase.endOperations();}
        for(SyncDatabase.PendingUpload pending:db.uploads(server)){if(!SyncDatabase.beginUpload(pending.id))continue;try{upload(server,pending);db.uploadComplete(pending.id);new File(pending.path).delete();}catch(Exception error){db.uploadFailed(pending.id,error.getMessage());return false;}finally{SyncDatabase.endUpload(pending.id);}}
        return true;
    }

    private static final class Result{int code;String body;Result(int c,String b){code=c;body=b;}}
    private Result post(String endpoint,JSONObject values,String key)throws Exception{StringBuilder body=new StringBuilder();Iterator<String> keys=values.keys();while(keys.hasNext()){String name=keys.next();if(body.length()>0)body.append('&');body.append(URLEncoder.encode(name,"UTF-8")).append('=').append(URLEncoder.encode(values.optString(name),"UTF-8"));}HttpURLConnection c=(HttpURLConnection)new URL(endpoint).openConnection();c.setConnectTimeout(15000);c.setReadTimeout(60000);c.setRequestMethod("POST");c.setDoOutput(true);c.setRequestProperty("Accept","application/json");c.setRequestProperty("Content-Type","application/x-www-form-urlencoded; charset=utf-8");c.setRequestProperty("Idempotency-Key",key);try(OutputStream out=c.getOutputStream()){out.write(body.toString().getBytes(StandardCharsets.UTF_8));}return response(c);}
    private Result response(HttpURLConnection c)throws Exception{int code=c.getResponseCode();InputStream source=code>=200&&code<300?c.getInputStream():c.getErrorStream();ByteArrayOutputStream sink=new ByteArrayOutputStream();if(source!=null)try(InputStream in=source){byte[] b=new byte[8192];int n;while((n=in.read(b))>0)sink.write(b,0,n);}return new Result(code,sink.toString("UTF-8"));}
    private void upload(String server,SyncDatabase.PendingUpload task)throws Exception{File file=new File(task.path);if(!file.isFile())throw new FileNotFoundException(task.path);String boundary="----ContentTransfer"+System.nanoTime();HttpURLConnection c=(HttpURLConnection)new URL(server+"/upload-stream").openConnection();c.setConnectTimeout(30000);c.setReadTimeout(300000);c.setRequestMethod("POST");c.setDoOutput(true);c.setChunkedStreamingMode(65536);c.setRequestProperty("Content-Type","multipart/form-data; boundary="+boundary);c.setRequestProperty("Idempotency-Key",task.id);String safe=task.name.replace("\"","").replace("\r"," ").replace("\n"," ");try(OutputStream out=c.getOutputStream()){out.write(("--"+boundary+"\r\nContent-Disposition: form-data; name=\"expiry\"\r\n\r\n"+task.expiry+"\r\n--"+boundary+"\r\nContent-Disposition: form-data; name=\"file-upload\"; filename=\""+safe+"\"\r\nContent-Type: application/octet-stream\r\n\r\n").getBytes(StandardCharsets.UTF_8));try(InputStream in=new FileInputStream(file)){byte[] b=new byte[65536];int n;while((n=in.read(b))>0)out.write(b,0,n);}out.write(("\r\n--"+boundary+"--\r\n").getBytes(StandardCharsets.UTF_8));}Result result=response(c);if(result.code<200||result.code>=300)throw new IOException("HTTP "+result.code+" "+result.body);}
}
