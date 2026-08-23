package ink.yode.contenttransfer;

import android.graphics.Color;
import android.graphics.Typeface;
import android.text.SpannableStringBuilder;
import android.text.Spanned;
import android.text.style.BackgroundColorSpan;
import android.text.style.RelativeSizeSpan;
import android.text.style.StyleSpan;
import android.text.style.TypefaceSpan;
import android.text.style.URLSpan;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

final class MarkdownRenderer {
    private static final Pattern INLINE = Pattern.compile("\\[([^]]+)]\\((https?://[^ )]+)\\)|\\*\\*([^*]+)\\*\\*|(?<!\\*)\\*([^*]+)\\*(?!\\*)");

    private MarkdownRenderer() {}

    static CharSequence render(String source) {
        SpannableStringBuilder out = new SpannableStringBuilder();
        boolean code = false;
        String[] lines = source.split("\n", -1);
        for (int index = 0; index < lines.length; index++) {
            String line = lines[index];
            if (line.trim().startsWith("```")) { code = !code; continue; }
            int start = out.length();
            if (code) {
                out.append(line);
                if (!line.isEmpty()) {
                    out.setSpan(new TypefaceSpan("monospace"), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
                    out.setSpan(new BackgroundColorSpan(Color.rgb(240, 236, 242)), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
                }
            } else {
                int heading = 0;
                while (heading < line.length() && heading < 6 && line.charAt(heading) == '#') heading++;
                if (heading > 0 && heading < line.length() && line.charAt(heading) == ' ') {
                    appendInline(out, line.substring(heading + 1));
                    out.setSpan(new StyleSpan(Typeface.BOLD), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
                    out.setSpan(new RelativeSizeSpan(Math.max(1.1f, 1.55f - heading * .08f)), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
                } else if (line.matches("^\\s*[-*+]\\s+.*")) {
                    out.append("•  ");
                    appendInline(out, line.replaceFirst("^\\s*[-*+]\\s+", ""));
                } else appendInline(out, line);
            }
            if (index < lines.length - 1) out.append('\n');
        }
        return out;
    }

    private static void appendInline(SpannableStringBuilder out, String line) {
        Matcher matcher = INLINE.matcher(line);
        int cursor = 0;
        while (matcher.find()) {
            out.append(line, cursor, matcher.start());
            int start = out.length();
            if (matcher.group(1) != null) {
                out.append(matcher.group(1));
                out.setSpan(new URLSpan(matcher.group(2)), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
            } else if (matcher.group(3) != null) {
                out.append(matcher.group(3));
                out.setSpan(new StyleSpan(Typeface.BOLD), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
            } else {
                out.append(matcher.group(4));
                out.setSpan(new StyleSpan(Typeface.ITALIC), start, out.length(), Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
            }
            cursor = matcher.end();
        }
        out.append(line, cursor, line.length());
    }
}
