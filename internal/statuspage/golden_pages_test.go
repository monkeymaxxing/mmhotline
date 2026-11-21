package statuspage

import (
	"slices"
	"testing"
	"time"

	"mmhotline/internal/htmlgolden"
)

// goldenPages lists the status page's index.html, the one page this package
// renders (RenderPage, which publishArtifacts calls), with the states the golden
// test covers.
var goldenPages = []struct {
	render, page string
	states       []string
}{
	{render: "statuspage.go RenderPage (index.html, also through publishArtifacts)", page: "index",
		states: []string{"no_notices", "open_notice", "open_and_resolved_notices", "degraded_notice", "maintenance_with_a_window", "outage_and_maintenance_and_resolved_maintenance", "open_notice_without_updates",
			"links_and_components_all_operational", "components_with_open_notices_and_affected_chips", "scheduled_maintenance_leaves_components_operational", "bad_links_in_status_json_are_left_out", "edited_update_shows_a_quiet_note", "no_host_name_means_no_feed_links"}},
}

// index.html renders as its golden file says, apart from whitespace (see
// package htmlgolden). Notice messages keep their line breaks and spaces.
func TestGoldenPages(t *testing.T) {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	pointer := func(t time.Time) *time.Time { return &t }
	resolvedAt := at(29, 9, 30)
	open := Notice{NoticeID: 2, Title: "Orders are delayed", Impact: ImpactOutage, OpenedAt: at(30, 11, 0), UpdatedAt: at(30, 11, 40), NextUpdateID: 3, Updates: []Update{
		{UpdateID: 1, Stage: StageInvestigating, Message: "Orders are delayed.\nWe are looking into it.", PostedAt: at(30, 11, 0)},
		{UpdateID: 2, Stage: StageIdentified, Message: "We found the cause:  a full disk.", PostedAt: at(30, 11, 40)},
	}}
	resolved := Notice{NoticeID: 1, Title: "Sign-in failures", Impact: ImpactDegraded, OpenedAt: at(28, 22, 0), UpdatedAt: resolvedAt, ResolvedAt: &resolvedAt, NextUpdateID: 3, Updates: []Update{
		{UpdateID: 1, Stage: StageInvestigating, Message: "Sign-ins fail <for some> customers & partners.", PostedAt: at(28, 22, 0)},
		{UpdateID: 2, Stage: StageResolved, Message: "Sign-ins work again.", PostedAt: resolvedAt},
	}}
	degraded := Notice{NoticeID: 3, Title: "Slow sign-in", Impact: ImpactDegraded, OpenedAt: at(30, 10, 0), UpdatedAt: at(30, 10, 0), NextUpdateID: 2, Updates: []Update{
		{UpdateID: 1, Message: "Sign-in takes longer than usual.", PostedAt: at(30, 10, 0)},
	}}
	maintenance := Notice{NoticeID: 4, Title: "Database upgrade", Impact: ImpactMaintenance, ScheduledStartAt: pointer(at(31, 2, 0)), ScheduledEndAt: pointer(at(31, 4, 0)), OpenedAt: at(29, 12, 0), UpdatedAt: at(29, 12, 0), NextUpdateID: 2, Updates: []Update{
		{UpdateID: 1, Stage: StageScheduled, Message: "We will upgrade the database.\nOrders pause for a few minutes.", PostedAt: at(29, 12, 0)},
	}}
	completedAt := at(27, 3, 0)
	completed := Notice{NoticeID: 5, Title: "Network maintenance", Impact: ImpactMaintenance, ScheduledStartAt: pointer(at(27, 2, 0)), OpenedAt: at(26, 9, 0), UpdatedAt: completedAt, ResolvedAt: &completedAt, NextUpdateID: 3, Updates: []Update{
		{UpdateID: 1, Stage: StageScheduled, Message: "We will replace a switch.", PostedAt: at(26, 9, 0)},
		{UpdateID: 2, Stage: StageCompleted, Message: "The switch is replaced.", PostedAt: completedAt},
	}}
	noUpdates := Notice{NoticeID: 6, Title: "Notice without updates", Impact: ImpactOutage, OpenedAt: at(30, 9, 0), UpdatedAt: at(30, 9, 0)}
	components := []Component{{ComponentID: "website", Name: "Website", Description: "Main site and customer dashboard"}, {ComponentID: "api", Name: "API"}, {ComponentID: "mail", Name: "Mail"}, {ComponentID: "files", Name: "Files"}}
	linked := File{PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), WebsiteURL: "https://example.com", SupportURL: "mailto:support@example.com", Components: components}
	withChips := func(notice Notice, ids ...string) Notice {
		notice.AffectedComponentIDs = ids
		return notice
	}
	inProgress := withChips(maintenance, "files")
	inProgress.Updates = append(slices.Clone(inProgress.Updates), Update{UpdateID: 2, Stage: StageInProgress, Message: "The upgrade started.", PostedAt: at(30, 11, 30)})
	edited := open
	edited.Updates = slices.Clone(open.Updates)
	edited.Updates[0].EditedAt = pointer(at(30, 11, 5))
	rendered := map[string]bool{}
	for state, file := range map[string]File{
		"no_notices":                {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0)},
		"open_notice":               {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{open}},
		"open_and_resolved_notices": {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{resolved, open}},
		"degraded_notice":           {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{degraded}},
		"maintenance_with_a_window": {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{maintenance}},
		"outage_and_maintenance_and_resolved_maintenance": {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{maintenance, degraded, open, completed}},
		"open_notice_without_updates":                     {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{noUpdates}},
		"links_and_components_all_operational":            linked,
		"components_with_open_notices_and_affected_chips": {PageTitle: linked.PageTitle, PublishedAt: linked.PublishedAt, WebsiteURL: linked.WebsiteURL, SupportURL: linked.SupportURL, Components: components,
			Notices: []Notice{inProgress, withChips(degraded, "api"), withChips(open, "website", "mail", "retired"), withChips(resolved, "website")}},
		"scheduled_maintenance_leaves_components_operational": {PageTitle: linked.PageTitle, PublishedAt: linked.PublishedAt, Components: components, Notices: []Notice{withChips(maintenance, "website", "api")}},
		"edited_update_shows_a_quiet_note":                    {PageTitle: "Example Inc. status", PublishedAt: at(30, 12, 0), Notices: []Notice{edited}},
		"bad_links_in_status_json_are_left_out":               {PageTitle: linked.PageTitle, PublishedAt: linked.PublishedAt, WebsiteURL: "javascript:alert(1)", SupportURL: "http://example.com/support"},
		"no_host_name_means_no_feed_links":                    {PageTitle: "Example Inc.", PublishedAt: at(30, 12, 0), Notices: []Notice{open}},
	} {
		// Every page of the settings file has a host name, so every page has
		// feed links; the one state without a host name shows they are left out.
		if state != "no_host_name_means_no_feed_links" {
			file.HostName = "status.example.com"
		}
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
