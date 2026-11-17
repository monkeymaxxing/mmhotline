package htmlgolden

import (
	"regexp"
	"testing"
)

// The comparison ignores only how the same page is written, and catches any
// change a browser shows or a script reads.
func TestCompareIgnoresOnlyHowThePageIsWritten(t *testing.T) {
	for _, tc := range []struct{ golden, now string }{
		{`<p class="a" id="b">x</p>`, `<p id='b' class=a>x</p>`},
		{`<p title="it&#39;s">it&#39;s</p>`, `<p title="it&#x27;s">it's</p>`},
		{`<input required=""><br/>`, `<INPUT REQUIRED><br>`},
		{`<div><p>a  b</p>   <p>c</p></div>`, "<div>\n<p>a\tb</p><p>c</p>\n</div>"},
		{`<p> <strong>a</strong> <a href="/x">b</a></p>`, "<p><strong>a</strong>\n<a href=\"/x\">b</a></p>"},
	} {
		if difference := Compare([]byte(tc.golden), []byte(tc.now)); difference != "" {
			t.Errorf("%q and %q differ: %s", tc.golden, tc.now, difference)
		}
	}
	for _, tc := range []struct{ golden, now string }{
		{`<p>a b</p>`, `<p>a c</p>`},
		{`<p>ab</p>`, `<p>a b</p>`},
		{`<p class="a b">x</p>`, `<p class="a  b">x</p>`},
		{`<a href="/x?a=1&amp;b=2">x</a>`, `<a href="/x?a=1&amp;b=3">x</a>`},
		{`<strong>a</strong> <a>b</a>`, `<strong>a</strong><a>b</a>`},
		{`<p>a&nbsp;b</p>`, `<p>a b</p>`},
		{"<pre>a\n b</pre>", "<pre>a b</pre>"},
		{"<pre><code>a\n b</code></pre>", "<pre><code>a b</code></pre>"},
		{"<textarea>a\n</textarea>", "<textarea>a</textarea>"},
		{`<script>let a = 1;</script>`, `<script>let a =  1;</script>`},
		{`<style>p{margin:0}</style>`, `<style>p{margin: 0}</style>`},
		{`<script>a&amp;b</script>`, `<script>a&b</script>`},
		{"<div class=\"result\">a\nb</div>", "<div class=\"result\">a b</div>"},
		{"<p class=\"contact-message\">a\n<b>b</b></p>", "<p class=\"contact-message\">a <b>b</b></p>"},
		{"<ol class=\"timeline\"><li><div>a\nb</div></li></ol>", "<ol class=\"timeline\"><li><div>a b</div></li></ol>"},
		{`<p>x</p><!-- a -->`, `<p>x</p><!-- b -->`},
		{`<p>x</p>`, `<p>x</p><p></p>`},
		{`<p data-x="1">x</p>`, `<p data-x="1" hidden>x</p>`},
	} {
		if Compare([]byte(tc.golden), []byte(tc.now)) == "" {
			t.Errorf("%q and %q compare the same", tc.golden, tc.now)
		}
	}
}

func TestPlaceholdersNumberEachDistinctMatchOnce(t *testing.T) {
	id := regexp.MustCompile(`\b[0-9a-f]{4}\b`)
	if got := Placeholders("beef cafe beef", id, "ID"); got != "ID-1 ID-2 ID-1" {
		t.Fatal(got)
	}
}
