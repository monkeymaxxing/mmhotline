// Package htmlgolden pins the HTML a page renders, for tests only. A test
// renders a page with fixed data, and Check compares it with the page's
// golden file under testdata/pages/<package>/. Running the test with
// -update rewrites the golden files.
//
// The comparison parses both pages with golang.org/x/net/html and compares
// their token streams, so a change of template engine that keeps the page
// passes, and any change a browser would show or a script would read fails.
// The comparison ignores exactly these differences:
//
//   - The order of a tag's attributes, and how attribute values are quoted.
//   - How a character is written: an entity (&#39;, &#x27;, &apos;) or the
//     character itself, in text and in attribute values. Script and style
//     text is never decoded, so it compares byte for byte.
//   - The letter case of tag and attribute names, a void element's closing
//     slash (<br> and <br/>), and an empty attribute's value (required and
//     required="").
//   - Runs of HTML whitespace (space, tab, line feed, form feed, carriage
//     return) in text, which compare as one space. A no-break space is not
//     whitespace here and compares as itself.
//   - Whitespace in text next to the start or end tag of an element that is
//     not inline (anything outside inlineElements), where it does not show.
//     Whitespace between two inline elements or text, such as the space in
//     "<strong>a</strong> <a>", shows in the browser and is compared.
//
// Text whose whitespace shows keeps every byte of its whitespace: text
// inside <pre>, <textarea>, <script>, and <style>, and inside the elements
// the stylesheets give white-space: pre-wrap (see keepsWhitespace).
// Attribute values are compared whole, without any whitespace change.
package htmlgolden

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var update = flag.Bool("update", false, "rewrite the golden HTML files under testdata/pages")

// Dir is where a package's golden files live, relative to its directory.
func Dir(packageName string) string {
	return filepath.Join("testdata", "pages", packageName)
}

// Check compares page with the golden file <dir>/<name>.html, or rewrites
// that file when the test runs with -update.
func Check(t testing.TB, dir, name string, page []byte) {
	t.Helper()
	path := filepath.Join(dir, name+".html")
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, page, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run the test with -update to write it)", name, err)
	}
	if difference := Compare(want, page); difference != "" {
		t.Errorf("%s differs from %s: %s", name, path, difference)
	}
}

// Updating says whether the test runs with -update.
func Updating() bool { return *update }

// Compare returns "" when the two pages have the same token stream, and
// otherwise describes the first difference.
func Compare(want, got []byte) string {
	a, b := Tokens(want), Tokens(got)
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) && i < len(b) && a[i] == b[i] {
			continue
		}
		return fmt.Sprintf("token %d\n  golden: %s\n  now:    %s\n  golden context: %s\n  now context:    %s",
			i, at(a, i), at(b, i), strings.Join(window(a, i), " "), strings.Join(window(b, i), " "))
	}
	return ""
}

func at(tokens []string, i int) string {
	if i >= len(tokens) {
		return "(end of page)"
	}
	return tokens[i]
}

func window(tokens []string, i int) []string {
	return tokens[max(0, i-3):min(len(tokens), i+4)]
}

// inlineElements are the elements whose neighboring whitespace shows on the
// page. Whitespace next to any other element's tags does not compare.
var inlineElements = map[string]bool{
	"a": true, "abbr": true, "b": true, "bdi": true, "bdo": true, "br": true, "button": true, "cite": true,
	"code": true, "data": true, "dfn": true, "em": true, "i": true, "img": true, "input": true, "kbd": true,
	"label": true, "mark": true, "output": true, "q": true, "s": true, "samp": true, "select": true,
	"small": true, "span": true, "strong": true, "sub": true, "sup": true, "time": true, "u": true,
	"var": true, "wbr": true,
}

// voidElements have no end tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

type element struct {
	name    string
	classes []string
}

// keepsWhitespace says whether text inside the element at the top of open
// shows its whitespace: <pre>, <textarea>, <script>, and <style>, and the
// elements the stylesheets (internal/app/mmhotline.css and
// internal/statuspage/status_page.css) give white-space: pre-wrap: class
// "result", class "contact-message", and ".timeline li > div".
func keepsWhitespace(open []element) bool {
	for i, e := range open {
		switch {
		case e.name == "pre" || e.name == "textarea" || e.name == "script" || e.name == "style":
			return true
		case slices.Contains(e.classes, "result") || slices.Contains(e.classes, "contact-message"):
			return true
		case e.name == "div" && i >= 2 && open[i-1].name == "li" && slices.ContainsFunc(open[:i-1], func(o element) bool { return slices.Contains(o.classes, "timeline") }):
			return true
		}
	}
	return false
}

var htmlWhitespace = regexp.MustCompile(`[ \t\n\f\r]+`)

type token struct {
	kind   html.TokenType
	name   string // tag name
	text   string // text, or a tag's attributes, written out
	exact  bool   // text inside an exactTextElements element
	inline bool   // the tag of an inline element
}

// Tokens is the page's token stream as the comparison sees it, one string
// per token.
func Tokens(page []byte) []string {
	z := html.NewTokenizer(bytes.NewReader(page))
	var tokens []token
	var open []element // the elements open around the current token
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		t := z.Token()
		switch kind {
		case html.StartTagToken, html.SelfClosingTagToken:
			attributes := make([]string, 0, len(t.Attr))
			for _, attribute := range t.Attr {
				name := attribute.Key
				if attribute.Namespace != "" {
					name = attribute.Namespace + ":" + name
				}
				attributes = append(attributes, " "+name+"="+strconv.Quote(attribute.Val))
			}
			slices.Sort(attributes)
			tokens = append(tokens, token{kind: html.StartTagToken, name: t.Data, text: strings.Join(attributes, ""), inline: inlineElements[t.Data]})
			if kind == html.StartTagToken && !voidElements[t.Data] {
				var classes []string
				for _, attribute := range t.Attr {
					if attribute.Key == "class" {
						classes = strings.Fields(attribute.Val)
					}
				}
				open = append(open, element{name: t.Data, classes: classes})
			}
		case html.EndTagToken:
			tokens = append(tokens, token{kind: kind, name: t.Data, inline: inlineElements[t.Data]})
			// Close the nearest open element of this name, and any left
			// open inside it, as an optional end tag leaves them.
			for i := len(open) - 1; i >= 0; i-- {
				if open[i].name == t.Data {
					open = open[:i]
					break
				}
			}
		case html.TextToken:
			if n := len(tokens); n > 0 && tokens[n-1].kind == html.TextToken {
				tokens[n-1].text += t.Data
			} else {
				tokens = append(tokens, token{kind: kind, text: t.Data, exact: keepsWhitespace(open)})
			}
		case html.CommentToken:
			tokens = append(tokens, token{kind: kind, text: t.Data})
		case html.DoctypeToken:
			tokens = append(tokens, token{kind: kind, text: strings.ToLower(t.Data)})
		}
	}
	var out []string
	for i, t := range tokens {
		switch t.kind {
		case html.StartTagToken:
			out = append(out, "<"+t.name+t.text+">")
		case html.EndTagToken:
			out = append(out, "</"+t.name+">")
		case html.CommentToken:
			out = append(out, "<!--"+t.text+"-->")
		case html.DoctypeToken:
			out = append(out, "<!doctype "+t.text+">")
		case html.TextToken:
			if t.exact {
				out = append(out, "text "+strconv.Quote(t.text))
				continue
			}
			text := htmlWhitespace.ReplaceAllString(t.text, " ")
			if i == 0 || !showsWhitespace(tokens[i-1]) {
				text = strings.TrimLeft(text, " ")
			}
			if i == len(tokens)-1 || !showsWhitespace(tokens[i+1]) {
				text = strings.TrimRight(text, " ")
			}
			if text != "" {
				out = append(out, "text "+strconv.Quote(text))
			}
		}
	}
	return out
}

// showsWhitespace says whether whitespace next to token t shows on the page.
func showsWhitespace(t token) bool {
	return t.inline && (t.kind == html.StartTagToken || t.kind == html.EndTagToken)
}

// Placeholders replaces each distinct match of pattern in page with
// prefix-1, prefix-2, and so on, numbered in order of first appearance, so a
// random ID gets the same placeholder wherever the page repeats it.
func Placeholders(page string, pattern *regexp.Regexp, prefix string) string {
	seen := map[string]string{}
	return pattern.ReplaceAllStringFunc(page, func(match string) string {
		if placeholder, ok := seen[match]; ok {
			return placeholder
		}
		seen[match] = prefix + "-" + strconv.Itoa(len(seen)+1)
		return seen[match]
	})
}
