package ink.yode.contenttransfer;

import android.content.Context;
import android.content.SharedPreferences;
import android.net.Uri;

final class ServerConfig {
    static final String PREFS = "content-transfer";
    static final String KEY = "server_url";
    private ServerConfig() {}

    static String get(Context context) {
        return context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY, "");
    }

    static String normalize(String value) {
        String result=value==null?"":value.trim();
        while(result.endsWith("/"))result=result.substring(0,result.length()-1);
        Uri uri=Uri.parse(result);
        String scheme=uri.getScheme();
        if(result.isEmpty()||scheme==null||uri.getHost()==null||(!scheme.equalsIgnoreCase("http")&&!scheme.equalsIgnoreCase("https")))return "";
        return result;
    }

    static boolean save(Context context,String value) {
        String normalized=normalize(value);if(normalized.isEmpty())return false;
        return context.getSharedPreferences(PREFS,Context.MODE_PRIVATE).edit().putString(KEY,normalized).commit();
    }
}
