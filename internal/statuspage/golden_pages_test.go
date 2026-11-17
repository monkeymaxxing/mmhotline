package statuspage

import (
	"testing"
	"time"

	"mmhotline/internal/htmlgolden"
)

// goldenPages lists the status page's index.html, the one page this package
// renders (RenderPage, which publishPage calls), with the states the golden
// test covers.
var goldenPages = []struct {
	render, page string
	states       []string
}{
	{render: "statuspage.go RenderPage (index.html, also through publishPage)", page: "index", states: []string{"no_notices", "open_notice", "open_and_resolved_notices"}},
}

// index.html renders as its golden file says, apart from whitespace (see
// package htmlgolden). Notice messages keep their line breaks and spaces.
func TestGoldenPages(t *testing.T) {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	resolvedAt := at(29, 9, 30)
	open := Notice{NoticeID: 2, OpenedAt: at(30, 11, 0), Updates: []Update{
		{Message: "Orders are delayed.\nWe are looking into it.", PostedAt: at(30, 11, 0)},
		{Message: "We found the cause:  a full disk.", PostedAt: at(30, 11, 40)},
	}}
	resolved := Notice{NoticeID: 1, OpenedAt: at(28, 22, 0), ResolvedAt: &resolvedAt, Updates: []Update{
		{Message: "Sign-ins fail <for some> customers & partners.", PostedAt: at(28, 22, 0)},
		{Message: "Sign-ins work again.", PostedAt: resolvedAt},
	}}
	rendered := map[string]bool{}
	for state, file := range map[string]File{
		"no_notices":                {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0)},
		"open_notice":               {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{open}},
		"open_and_resolved_notices": {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{resolved, open}},
	} {
		page, err := RenderPage(file)
		if err != nil {
			t.Fatal(err)
		}
		htmlgolden.Check(t, htmlgolden.Dir("statuspage"), "index__"+state, page)
		rendered["index__"+state] = true
	}
	for _, page := range goldenPages {
		for _, state := range page.states {
			if !rendered[page.page+"__"+state] {
				t.Errorf("%s: the state %s is listed as covered but not rendered", page.render, state)
			}
		}
	}
}
