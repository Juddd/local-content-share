warning: /bin/sh: setlocale: LC_ALL: cannot change locale (C.UTF-8)
package ink.yode.contenttransfer;

import android.app.Activity;
import android.content.Intent;
import android.database.Cursor;
import android.net.Uri;
import android.os.Bundle;
import android.provider.OpenableColumns;
import android.widget.Toast;

import java.util.ArrayList;

public class ShareUploadActivity extends Activity {
    @Override protected void onCreate(Bundle state) {
        super.onCreate(state);
        overridePendingTransition(0, 0);
        Intent source = getIntent();
        ArrayList<Uri> uris = new ArrayList<>();
        if (Intent.ACTION_SEND.equals(source.getAction())) {
            Uri uri = source.getParcelableExtra(Intent.EXTRA_STREAM);
            if (uri != null) uris.add(uri);
        } else if (Intent.ACTION_SEND_MULTIPLE.equals(source.getAction())) {
            ArrayList<Uri> values = source.getParcelableArrayListExtra(Intent.EXTRA_STREAM);
            if (values != null) uris.addAll(values);
        }
        if (uris.isEmpty()) {
            CharSequence sharedText = source.getCharSequenceExtra(Intent.EXTRA_TEXT);
            if (Intent.ACTION_SEND.equals(source.getAction())
                    && sharedText != null && !sharedText.toString().trim().isEmpty()) {
                Intent upload = new Intent(this, ShareUploadService.class);
                upload.putExtra(ShareUploadService.EXTRA_TEXT, sharedText.toString());
                startForegroundService(upload);
                finishQuietly();
                return;
            }
            Toast.makeText(this, "没有可发送的文字或文件", Toast.LENGTH_SHORT).show();
            finishQuietly();
            return;
        }
        ArrayList<String> names = new ArrayList<>();
        ArrayList<String> uriValues = new ArrayList<>();
        for(Uri uri:uris){uriValues.add(uri.toString());names.add(displayName(uri));}
        Intent upload = new Intent(this, ShareUploadService.class);
        upload.putStringArrayListExtra(ShareUploadService.EXTRA_URIS, uriValues);
        upload.putStringArrayListExtra(ShareUploadService.EXTRA_NAMES, names);
        upload.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
        if(source.getClipData()!=null)upload.setClipData(source.getClipData());
        startForegroundService(upload);
        Toast.makeText(this, "已交由后台上传 " + uris.size() + " 个文件", Toast.LENGTH_SHORT).show();
        finishQuietly();
    }

    private String displayName(Uri uri) {
        try (Cursor cursor = getContentResolver().query(uri,
                new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (cursor != null && cursor.moveToFirst() && !cursor.isNull(0)) return cursor.getString(0);
        } catch (Exception ignored) {}
        String segment = uri.getLastPathSegment();
        return segment == null || segment.trim().isEmpty() ? "shared-file" : segment;
    }

    private void finishQuietly() {
        finish();
        overridePendingTransition(0, 0);
    }
}
