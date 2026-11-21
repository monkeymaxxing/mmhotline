package app

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// XML 1.0 forbids most control characters, so a title that carries one (such
// as the ESC of a forwarded ANSI color code) must not make the call markup
// invalid.
func TestCallMarkupIsWellFormedForControlCharacterTitles(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"disk\x00full", "web \x1b[31mdown", "a\x0bb", "a￾b", "a￿b", "a\u0085b", "a\u009bb", "a\xffb", "a\x7fb"} {
		markup := callMarkup(sosCallMessage("Team", []string{title}, false))
		decoder := xml.NewDecoder(strings.NewReader(markup))
		for {
			_, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("title %q: markup is not well-formed: %v", title, err)
			}
		}
	}
}

// A control character becomes a space, so the words around it stay apart; a
// title without one is spoken exactly as before.
func TestCallMarkupKeepsWordsApartAndOrdinaryTitlesUnchanged(t *testing.T) {
	t.Parallel()
	plain := callMarkup(sosCallMessage("Team", []string{"disk full"}, false))
	if !strings.Contains(plain, "Team: Team. disk full.") {
		t.Fatalf("ordinary title changed: %s", plain)
	}
	apart := callMarkup(sosCallMessage("Team", []string{"disk\x00full"}, false))
	if !strings.Contains(apart, "Team: Team. disk full.") {
		t.Fatalf("words ran together: %s", apart)
	}
}
