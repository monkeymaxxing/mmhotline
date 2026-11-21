package statuspage

import (
	"encoding/xml"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// recentlyChangedInFeed is how many notices feed.xml lists, the most
// recently changed first. Readers poll the feed, so a short list is enough:
// it holds weeks of notices for a team that posts a few a week. The page
// itself and status.json keep the full history.
const recentlyChangedInFeed = 20

// The feed's types follow RFC 4287 (Atom 1.0).
type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  atomAuthor  `xml:"author"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

type atomEntry struct {
	ID        string   `xml:"id"`
	Title     string   `xml:"title"`
	Updated   string   `xml:"updated"`
	Published string   `xml:"published"`
	Links     atomLink `xml:"link"`
	Content   atomText `xml:"content"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

// RenderFeed builds feed.xml, the page's Atom 1.0 feed, from status.json's
// content. Each entry is one notice, with its updates in the entry's text,
// so a reader sees a notice change as one entry that moves to the top again.
// The feed's links are absolute, which needs the page's host name.
func RenderFeed(file File) ([]byte, error) {
	if file.HostName == "" {
		return nil, errors.New("the Atom feed needs the page's host name, which status.json lacks; run settings apply to set it")
	}
	pageURL := "https://" + file.HostName + "/"
	feed := atomFeed{
		ID:      pageURL,
		Title:   file.PageTitle,
		Updated: isoTime(file.PublishedAt),
		Author:  atomAuthor{Name: file.PageTitle},
		Links: []atomLink{
			{Rel: "self", Type: "application/atom+xml", Href: pageURL + "feed.xml"},
			{Rel: "alternate", Type: "text/html", Href: pageURL},
		},
	}
	notices := slices.Clone(file.Notices)
	sort.SliceStable(notices, func(i, j int) bool {
		if !notices[i].UpdatedAt.Equal(notices[j].UpdatedAt) {
			return notices[i].UpdatedAt.After(notices[j].UpdatedAt)
		}
		return notices[i].NoticeID > notices[j].NoticeID
	})
	if len(notices) > recentlyChangedInFeed {
		notices = notices[:recentlyChangedInFeed]
	}
	for _, notice := range notices {
		noticeURL := pageURL + "#notice-" + strconv.FormatInt(notice.NoticeID, 10)
		feed.Entries = append(feed.Entries, atomEntry{
			ID:        noticeURL,
			Title:     "[" + entryLabel(notice) + "] " + notice.Title,
			Updated:   isoTime(notice.UpdatedAt),
			Published: isoTime(notice.OpenedAt),
			Links:     atomLink{Rel: "alternate", Type: "text/html", Href: noticeURL},
			Content:   atomText{Type: "text", Text: entryText(file, notice)},
		})
	}
	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(`<?xml version="1.0" encoding="utf-8"?>`+"\n"), append(body, '\n')...), nil
}

// entryLabel is the word in front of an entry's title: the notice's impact
// while it is open, and Completed or Resolved after.
func entryLabel(notice Notice) string {
	if notice.ResolvedAt != nil {
		return resolvedTag(notice.Impact)
	}
	return impactTag(notice.Impact)
}

// entryText is a notice as plain text: its state, the components it affects,
// the planned window of a maintenance, and its updates newest first. Every
// time is UTC, because a feed reader has no script to show local time.
func entryText(file File, notice Notice) string {
	var text strings.Builder
	state := "Open"
	if notice.ResolvedAt != nil {
		state = resolvedTag(notice.Impact)
	}
	text.WriteString("Status: " + state + "\nImpact: " + impactTag(notice.Impact) + "\n")
	if len(notice.AffectedComponentIDs) > 0 {
		names := make([]string, len(notice.AffectedComponentIDs))
		for index, id := range notice.AffectedComponentIDs {
			names[index] = file.ComponentName(id)
		}
		text.WriteString("Affected: " + strings.Join(names, ", ") + "\n")
	}
	switch {
	case notice.ScheduledStartAt != nil && notice.ScheduledEndAt != nil:
		text.WriteString("Planned: " + publishedTime(*notice.ScheduledStartAt) + " to " + publishedTime(*notice.ScheduledEndAt) + " UTC\n")
	case notice.ScheduledStartAt != nil:
		text.WriteString("Planned start: " + publishedTime(*notice.ScheduledStartAt) + " UTC\n")
	case notice.ScheduledEndAt != nil:
		text.WriteString("Planned end: " + publishedTime(*notice.ScheduledEndAt) + " UTC\n")
	}
	for _, update := range newestFirst(notice.Updates) {
		text.WriteString("\n" + update.PostedAt.UTC().Format("2006-01-02 15:04") + " UTC")
		if update.Stage != "" {
			text.WriteString(" · " + StageLabel(update.Stage))
		}
		if update.EditedAt != nil {
			text.WriteString(" (edited)")
		}
		text.WriteString("\n" + update.Message + "\n")
	}
	return text.String()
}
