// Package statuspage builds and publishes a team's public status page: three
// files, index.html, feed.xml (an Atom feed), and status.json, in an R2
// bucket in the team's own Cloudflare account. status.json holds the whole
// notice history and is the page's only state. A post reads status.json,
// applies one change, and uploads all three files. MMHotline's service and the mmhotline command on a
// responder's laptop both post through Publish, so the files are always
// built the same way (docs/changing-mmhotline/design/mmhotline-down-alarm-and-status-page.md).
package statuspage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrRefused marks a post that breaks a rule of the page, such as an empty
// message or an update to a resolved notice. Callers show its text.
var ErrRefused = errors.New("refused")

// ErrNotPublished means the bucket holds no status.json yet: the page was
// never published by set-up on the MMHotline server.
var ErrNotPublished = errors.New("the status page has no status.json yet; apply the settings file with a status_page section on the MMHotline server to publish the page")

const (
	// maximumMessageCharacters keeps one update readable on a phone; it is
	// the limit people see on MMHotline's form and in the command's help.
	maximumMessageCharacters = 5000
	// maximumPageTitleCharacters fits a company or product name in a heading.
	maximumPageTitleCharacters = 100
	// maximumNoticeTitleCharacters fits a notice's title in a heading on a phone.
	maximumNoticeTitleCharacters = 140
	// recentlyResolvedOnPage is how many resolved notices index.html shows;
	// status.json keeps them all.
	recentlyResolvedOnPage = 5
	// maximumStatusJSONBytes bounds the read of status.json. A notice with 20
	// updates at their longest is about 300 KB in JSON, so 16 MiB holds years
	// of real notices.
	maximumStatusJSONBytes = 16 << 20
	// maximumComponents keeps the public page scannable and the form that
	// posts a notice usable on a phone.
	maximumComponents = 50
	// maximumComponentNameCharacters and maximumComponentDescriptionCharacters
	// fit a component's name and its one line of description on a phone.
	maximumComponentNameCharacters        = 100
	maximumComponentDescriptionCharacters = 200
	// maximumLinkCharacters is far longer than a real website or support
	// address, and keeps the page's header short.
	maximumLinkCharacters = 500
	// publishAttempts bounds the retries when another post changed
	// status.json between this post's read and write, and the uploads of
	// index.html and feed.xml when another post changed status.json meanwhile.
	publishAttempts = 3
)

// Impact is how a notice affects customers.
type Impact string

const (
	ImpactOutage      Impact = "outage"
	ImpactDegraded    Impact = "degraded"
	ImpactMaintenance Impact = "maintenance"
)

// Stage labels an update with where the notice stands. An update's stage is
// optional, and must fit its notice's impact: an outage or degraded notice
// moves from investigating to resolved, a maintenance notice from scheduled
// to completed.
type Stage string

const (
	StageInvestigating Stage = "investigating"
	StageIdentified    Stage = "identified"
	StageMonitoring    Stage = "monitoring"
	StageResolved      Stage = "resolved"
	StageScheduled     Stage = "scheduled"
	StageInProgress    Stage = "in_progress"
	StageCompleted     Stage = "completed"
)

var (
	incidentStages    = []Stage{StageInvestigating, StageIdentified, StageMonitoring, StageResolved}
	maintenanceStages = []Stage{StageScheduled, StageInProgress, StageCompleted}
)

// OverallStatus is what the page's banner and icon say: the worst impact of
// the open notices, or operational when none is open.
type OverallStatus string

const (
	StatusOperational OverallStatus = "operational"
	StatusMaintenance OverallStatus = "maintenance"
	StatusDegraded    OverallStatus = "degraded"
	StatusOutage      OverallStatus = "outage"
)

// Component is a part of the team's service that a notice can say it affects,
// such as the website or the API. The settings file lists them.
type Component struct {
	ComponentID string `json:"component_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PageConfig is what the settings file says about the public page: its title,
// its host name, the optional links in its header, and its components.
type PageConfig struct {
	PageTitle  string
	HostName   string
	WebsiteURL string
	SupportURL string
	Components []Component
}

// ComponentStatus is a component with the status the open notices give it.
type ComponentStatus struct {
	Component Component
	Status    OverallStatus
}

// File is status.json: the page's title and host name, the optional links and
// components the settings file sets, when it was last published, and every
// notice, newest first. NextNoticeID is the ID the next notice gets; it never
// goes down, so a notice ID is never used twice.
type File struct {
	PageTitle    string      `json:"page_title"`
	HostName     string      `json:"host_name,omitempty"`
	WebsiteURL   string      `json:"website_url,omitempty"`
	SupportURL   string      `json:"support_url,omitempty"`
	PublishedAt  time.Time   `json:"published_at"`
	NextNoticeID int64       `json:"next_notice_id,omitempty"`
	Components   []Component `json:"components,omitempty"`
	Notices      []Notice    `json:"notices"`
}

// Notice is one problem or planned maintenance the team tells its customers
// about. ResolvedAt is nil while the notice is open. The scheduled times are
// the planned window of a maintenance notice. UpdatedAt is when the notice
// last changed. NextUpdateID is the ID the notice's next update gets.
// AffectedComponentIDs are the components the notice says it affects.
type Notice struct {
	NoticeID             int64      `json:"notice_id"`
	Title                string     `json:"title"`
	Impact               Impact     `json:"impact"`
	AffectedComponentIDs []string   `json:"affected_component_ids,omitempty"`
	ScheduledStartAt     *time.Time `json:"scheduled_start_at,omitempty"`
	ScheduledEndAt       *time.Time `json:"scheduled_end_at,omitempty"`
	OpenedAt             time.Time  `json:"opened_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ResolvedAt           *time.Time `json:"resolved_at"`
	NextUpdateID         int64      `json:"next_update_id,omitempty"`
	Updates              []Update   `json:"updates"`
}

// Update is one message of a notice, shown as plain text with its line breaks.
type Update struct {
	UpdateID int64      `json:"update_id"`
	Stage    Stage      `json:"stage,omitempty"`
	Message  string     `json:"message"`
	PostedAt time.Time  `json:"posted_at"`
	EditedAt *time.Time `json:"edited_at,omitempty"`
}

// displayedImpact is the impact the page shows a notice with. A notice whose
// impact is none of the three, which only a file edited by hand can hold, is
// shown as an outage, so it is never mistaken for good news.
func displayedImpact(impact Impact) Impact {
	if impact == ImpactDegraded || impact == ImpactMaintenance {
		return impact
	}
	return ImpactOutage
}

// OverallStatus is outage while an outage notice is open, else degraded
// while a degraded one is open, else maintenance while a maintenance notice
// is open, else operational.
func (f File) OverallStatus() OverallStatus {
	status := StatusOperational
	for _, n := range f.OpenNotices() {
		switch displayedImpact(n.Impact) {
		case ImpactOutage:
			return StatusOutage
		case ImpactDegraded:
			status = StatusDegraded
		case ImpactMaintenance:
			if status == StatusOperational {
				status = StatusMaintenance
			}
		}
	}
	return status
}

// ComponentStatuses gives each configured component the worst status among the
// open notices that name it: an outage notice makes it an outage, a degraded
// notice degraded, and a maintenance notice maintenance. A maintenance notice
// whose latest update still has the stage scheduled is announced ahead of
// time and changes nothing yet; its first later stage, in progress, starts
// the maintenance of its components, and closing the notice ends it.
func (f File) ComponentStatuses() []ComponentStatus {
	statuses := make([]ComponentStatus, len(f.Components))
	for index, component := range f.Components {
		statuses[index] = ComponentStatus{Component: component, Status: StatusOperational}
	}
	for _, n := range f.OpenNotices() {
		var status OverallStatus
		switch displayedImpact(n.Impact) {
		case ImpactOutage:
			status = StatusOutage
		case ImpactDegraded:
			status = StatusDegraded
		case ImpactMaintenance:
			if len(n.Updates) > 0 && n.Updates[len(n.Updates)-1].Stage == StageScheduled {
				continue
			}
			status = StatusMaintenance
		}
		for index := range statuses {
			if slices.Contains(n.AffectedComponentIDs, statuses[index].Component.ComponentID) && statusSeverity[status] > statusSeverity[statuses[index].Status] {
				statuses[index].Status = status
			}
		}
	}
	return statuses
}

var statusSeverity = map[OverallStatus]int{StatusOperational: 0, StatusMaintenance: 1, StatusDegraded: 2, StatusOutage: 3}

// ComponentName is the component's name, or the ID itself for a component the
// settings file no longer lists, so an old notice that names it stays readable.
func (f File) ComponentName(componentID string) string {
	for _, component := range f.Components {
		if component.ComponentID == componentID {
			return component.Name
		}
	}
	return componentID
}

// OpenNotices returns the notices that are not resolved: outage and degraded
// notices first, then maintenance notices, each group newest first.
func (f File) OpenNotices() []Notice {
	var open []Notice
	for _, n := range f.Notices {
		if n.ResolvedAt == nil {
			open = append(open, n)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i], open[j]
		if aMaintenance, bMaintenance := displayedImpact(a.Impact) == ImpactMaintenance, displayedImpact(b.Impact) == ImpactMaintenance; aMaintenance != bMaintenance {
			return bMaintenance
		}
		if !a.OpenedAt.Equal(b.OpenedAt) {
			return a.OpenedAt.After(b.OpenedAt)
		}
		return a.NoticeID > b.NoticeID
	})
	return open
}

// ResolvedNotices returns at most limit resolved notices, most recently
// resolved first.
func (f File) ResolvedNotices(limit int) []Notice {
	var resolved []Notice
	for _, n := range f.Notices {
		if n.ResolvedAt != nil {
			resolved = append(resolved, n)
		}
	}
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].ResolvedAt.After(*resolved[j].ResolvedAt) })
	if len(resolved) > limit {
		resolved = resolved[:limit]
	}
	return resolved
}

// Change is one post: it changes the file in place at the time now, and
// returns the ID of the notice it changed.
type Change func(file *File, now time.Time) (int64, error)

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

func checkedMessage(message, label string, optional bool) (string, error) {
	message = strings.TrimSpace(message)
	length := utf8.RuneCountInString(message)
	if !utf8.ValidString(message) || length > maximumMessageCharacters || (length == 0 && !optional) {
		return "", refused("the %s must have 1 to %d characters", label, maximumMessageCharacters)
	}
	return message, nil
}

// CheckPageTitle returns the title without surrounding spaces, or why it is refused.
func CheckPageTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	length := utf8.RuneCountInString(title)
	if !utf8.ValidString(title) || length == 0 || length > maximumPageTitleCharacters || strings.ContainsAny(title, "\r\n") {
		return "", refused("the page title must have 1 to %d characters on one line", maximumPageTitleCharacters)
	}
	return title, nil
}

// CheckTitle returns a notice's title without surrounding spaces, or why it
// is refused.
func CheckTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	length := utf8.RuneCountInString(title)
	if !utf8.ValidString(title) || length == 0 || length > maximumNoticeTitleCharacters || strings.ContainsAny(title, "\r\n") {
		return "", refused("the title must have 1 to %d characters on one line", maximumNoticeTitleCharacters)
	}
	return title, nil
}

// checkedLine returns text without surrounding spaces when it is valid UTF-8
// with at most maximum characters, at least minimum, and no line break.
func checkedLine(text string, minimum, maximum int) (string, bool) {
	text = strings.TrimSpace(text)
	length := utf8.RuneCountInString(text)
	return text, utf8.ValidString(text) && length >= minimum && length <= maximum && !strings.ContainsAny(text, "\r\n")
}

// checkedLink returns raw without surrounding spaces and the URL it parses
// to, or why it is not one address of at most maximumLinkCharacters.
func checkedLink(raw, label string) (string, *url.URL, error) {
	raw = strings.TrimSpace(raw)
	length := utf8.RuneCountInString(raw)
	if !utf8.ValidString(raw) || length < 1 || length > maximumLinkCharacters || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return "", nil, refused("the %s must have 1 to %d characters, with no spaces or control characters", label, maximumLinkCharacters)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, refused("the %s is not a web address: %v", label, err)
	}
	return raw, u, nil
}

// checkedHTTPSLink checks that u is a web address that starts with https://
// and names a host, without a user name or password in it.
func checkedHTTPSLink(u *url.URL, label string) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return refused("the %s must be an https:// address with a host name and no user name or password, such as https://example.com", label)
	}
	return nil
}

// CheckWebsiteURL returns the website link of the page's header, or why it is
// refused. It is empty when the team sets none. An address that does not start
// with https:// is refused, so a link can never run a script (javascript:) or
// send a visitor to an unencrypted page.
func CheckWebsiteURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	raw, u, err := checkedLink(raw, "website URL")
	if err != nil {
		return "", err
	}
	if err := checkedHTTPSLink(u, "website URL"); err != nil {
		return "", err
	}
	return raw, nil
}

// CheckSupportURL returns the support link of the page's header, or why it is
// refused. It is empty when the team sets none. It is an https:// address, as
// for the website, or mailto: with one plain email address, such as
// mailto:support@example.com, with no display name and no query.
func CheckSupportURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	raw, u, err := checkedLink(raw, "support URL")
	if err != nil {
		return "", err
	}
	if u.Scheme == "mailto" {
		// The recipient may be percent-encoded, as in mailto:support%40example.com;
		// the decoded text is the address a mail program would use.
		decoded, unescapeErr := url.PathUnescape(u.Opaque)
		address, parseErr := mail.ParseAddress(decoded)
		if u.Opaque == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Host != "" || unescapeErr != nil ||
			strings.ContainsFunc(decoded, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) || parseErr != nil || address.Address != decoded {
			return "", refused("the support URL must have one email address after mailto:, such as mailto:support@example.com, with no name or query")
		}
		return raw, nil
	}
	if err := checkedHTTPSLink(u, "support URL"); err != nil {
		return "", refused("the support URL must be an https:// address or mailto: and one email address, such as https://example.com/support or mailto:support@example.com")
	}
	return raw, nil
}

var componentIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// CheckComponents returns the components without surrounding spaces in their
// fields, or why they are refused: at most maximumComponents, each with a
// component_id of 1 to 64 lowercase letters, digits, hyphens, and
// underscores, starting with a letter or digit, and unique; a name of 1 to
// 100 characters on one line; and an optional description of at most 200.
func CheckComponents(components []Component) ([]Component, error) {
	if len(components) > maximumComponents {
		return nil, refused("at most %d components are allowed, to keep the page short and the form usable on a phone, and there are %d", maximumComponents, len(components))
	}
	var checked []Component
	seen := map[string]bool{}
	for index, c := range components {
		c.ComponentID = strings.TrimSpace(c.ComponentID)
		if !componentIDPattern.MatchString(c.ComponentID) {
			return nil, refused("components[%d].component_id must have 1 to 64 lowercase letters, digits, hyphens, and underscores, starting with a letter or digit", index)
		}
		if seen[c.ComponentID] {
			return nil, refused("components[%d].component_id %q is used twice", index, c.ComponentID)
		}
		seen[c.ComponentID] = true
		var ok bool
		if c.Name, ok = checkedLine(c.Name, 1, maximumComponentNameCharacters); !ok {
			return nil, refused("components[%d].name must have 1 to %d characters on one line", index, maximumComponentNameCharacters)
		}
		if c.Description, ok = checkedLine(c.Description, 0, maximumComponentDescriptionCharacters); !ok {
			return nil, refused("components[%d].description must have at most %d characters on one line", index, maximumComponentDescriptionCharacters)
		}
		checked = append(checked, c)
	}
	return checked, nil
}

// checkedAffectedComponents returns the IDs of the notice's components in the
// order of the file's components, each once, or the first ID the page does not
// have.
func checkedAffectedComponents(file *File, ids []string) ([]string, error) {
	for _, id := range ids {
		if !slices.ContainsFunc(file.Components, func(c Component) bool { return c.ComponentID == id }) {
			if len(file.Components) == 0 {
				return nil, refused("the status page has no component %q; it has no components, which the settings file's status_page components key lists", id)
			}
			known := make([]string, len(file.Components))
			for index, c := range file.Components {
				known[index] = c.ComponentID
			}
			return nil, refused("the status page has no component %q; its components are %s", id, strings.Join(known, ", "))
		}
	}
	var affected []string
	for _, c := range file.Components {
		if slices.Contains(ids, c.ComponentID) {
			affected = append(affected, c.ComponentID)
		}
	}
	return affected, nil
}

// CheckImpact returns why impact is not outage, degraded, or maintenance.
func CheckImpact(impact Impact) error {
	if impact != ImpactOutage && impact != ImpactDegraded && impact != ImpactMaintenance {
		return refused("the impact must be %s, %s, or %s", ImpactOutage, ImpactDegraded, ImpactMaintenance)
	}
	return nil
}

// CheckStage returns why stage does not fit a notice with the impact. An
// empty stage always fits.
func CheckStage(impact Impact, stage Stage) error {
	if stage == "" {
		return nil
	}
	allowed := incidentStages
	if impact == ImpactMaintenance {
		allowed = maintenanceStages
	}
	if slices.Contains(allowed, stage) {
		return nil
	}
	names := make([]string, len(allowed))
	for index, a := range allowed {
		names[index] = string(a)
	}
	return refused("the stage %q is not a stage of %s notices; the stages are %s", stage, displayedImpact(impact), strings.Join(names, ", "))
}

// checkedSchedule returns the planned window in UTC to the second, or why it
// is refused. Only a maintenance notice has one.
func checkedSchedule(impact Impact, start, end *time.Time) (*time.Time, *time.Time, error) {
	if start == nil && end == nil {
		return nil, nil, nil
	}
	if impact != ImpactMaintenance {
		return nil, nil, refused("a planned start or end time is only for a %s notice", ImpactMaintenance)
	}
	normalized := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		utc := t.UTC().Truncate(time.Second)
		return &utc
	}
	start, end = normalized(start), normalized(end)
	if start != nil && end != nil && !end.After(*start) {
		return nil, nil, refused("the planned end must be after the planned start")
	}
	return start, end, nil
}

// PostNoticeInput is a new notice: its title, its impact, an optional stage
// for its first update, that first message, and for a maintenance notice an
// optional planned window. AffectedComponentIDs are the page's components the
// notice affects, none by default.
type PostNoticeInput struct {
	Title                string
	Impact               Impact
	AffectedComponentIDs []string
	Stage                Stage
	Message              string
	ScheduledStartAt     *time.Time
	ScheduledEndAt       *time.Time
}

// PostNotice opens a new notice with its first message.
func PostNotice(in PostNoticeInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		title, err := CheckTitle(in.Title)
		if err != nil {
			return 0, err
		}
		if err := CheckImpact(in.Impact); err != nil {
			return 0, err
		}
		if err := CheckStage(in.Impact, in.Stage); err != nil {
			return 0, err
		}
		message, err := checkedMessage(in.Message, "message", false)
		if err != nil {
			return 0, err
		}
		start, end, err := checkedSchedule(in.Impact, in.ScheduledStartAt, in.ScheduledEndAt)
		if err != nil {
			return 0, err
		}
		affected, err := checkedAffectedComponents(file, in.AffectedComponentIDs)
		if err != nil {
			return 0, err
		}
		id := max(file.NextNoticeID, 1)
		for _, n := range file.Notices {
			id = max(id, n.NoticeID+1)
		}
		file.NextNoticeID = id + 1
		notice := Notice{NoticeID: id, Title: title, Impact: in.Impact, AffectedComponentIDs: affected, ScheduledStartAt: start, ScheduledEndAt: end, OpenedAt: now, UpdatedAt: now, NextUpdateID: 2,
			Updates: []Update{{UpdateID: 1, Stage: in.Stage, Message: message, PostedAt: now}}}
		file.Notices = append([]Notice{notice}, file.Notices...)
		return id, nil
	}
}

// findNotice returns the one notice with the ID. A file that holds two
// notices with one ID, which only a file edited by hand can, is refused, so
// no change lands on the wrong one.
func findNotice(file *File, noticeID int64) (*Notice, error) {
	var found *Notice
	for index := range file.Notices {
		if file.Notices[index].NoticeID != noticeID {
			continue
		}
		if found != nil {
			return nil, refused("the status page has more than one notice %d; fix status.json before changing it", noticeID)
		}
		found = &file.Notices[index]
	}
	if found == nil {
		return nil, refused("the status page has no notice %d", noticeID)
	}
	return found, nil
}

// appendUpdate adds an update to the notice with the next update ID and
// marks the notice changed at now.
func (n *Notice) appendUpdate(stage Stage, message string, now time.Time) {
	id := max(n.NextUpdateID, 1)
	for _, u := range n.Updates {
		id = max(id, u.UpdateID+1)
	}
	n.NextUpdateID = id + 1
	n.Updates = append(n.Updates, Update{UpdateID: id, Stage: stage, Message: message, PostedAt: now})
	n.UpdatedAt = now
}

// PostUpdateInput is a message added to an open notice, with an optional stage.
type PostUpdateInput struct {
	NoticeID int64
	Stage    Stage
	Message  string
}

// PostUpdate adds a message to an open notice. Earlier messages stay on the
// page as the notice's history.
func PostUpdate(in PostUpdateInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		message, err := checkedMessage(in.Message, "message", false)
		if err != nil {
			return 0, err
		}
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if notice.ResolvedAt != nil {
			return 0, refused("notice %d is resolved; post a new notice instead", in.NoticeID)
		}
		if err := CheckStage(notice.Impact, in.Stage); err != nil {
			return 0, err
		}
		notice.appendUpdate(in.Stage, message, now)
		return in.NoticeID, nil
	}
}

// ResolveInput closes a notice. A closing message is added as its last
// update; its stage defaults to resolved, or completed for maintenance. A
// stage needs a closing message to go with.
type ResolveInput struct {
	NoticeID       int64
	Stage          Stage
	ClosingMessage string
}

// Resolve marks a notice resolved, with an optional closing message as its
// last update. Resolving a resolved notice changes nothing, and is refused when
// it brings a closing message, which would otherwise be dropped.
func Resolve(in ResolveInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		closingMessage, err := checkedMessage(in.ClosingMessage, "closing message", true)
		if err != nil {
			return 0, err
		}
		if closingMessage == "" && in.Stage != "" {
			return 0, refused("a stage needs a closing message to go with it")
		}
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if notice.ResolvedAt != nil {
			if closingMessage != "" || in.Stage != "" {
				return 0, refused("notice %d is already resolved; use edit to change it", in.NoticeID)
			}
			return in.NoticeID, nil
		}
		stage := in.Stage
		if closingMessage != "" && stage == "" {
			stage = StageResolved
			if notice.Impact == ImpactMaintenance {
				stage = StageCompleted
			}
		}
		if err := CheckStage(notice.Impact, stage); err != nil {
			return 0, err
		}
		resolved := now
		notice.ResolvedAt = &resolved
		notice.UpdatedAt = now
		if closingMessage != "" {
			notice.appendUpdate(stage, closingMessage, now)
		}
		return in.NoticeID, nil
	}
}

// ErrConflict marks an edit or delete made from a stale view: the notice
// changed after the person loaded the form. Callers show its text.
var ErrConflict = errors.New("conflict")

// findUpdate returns the one update of the notice with the ID, and its index.
// A notice that holds two updates with one ID, which only a file edited by
// hand can, is refused, so no change lands on the wrong one.
func findUpdate(notice *Notice, updateID int64) (*Update, int, error) {
	found := -1
	for index := range notice.Updates {
		if notice.Updates[index].UpdateID != updateID {
			continue
		}
		if found >= 0 {
			return nil, 0, refused("notice %d has more than one update %d; fix status.json before changing it", notice.NoticeID, updateID)
		}
		found = index
	}
	if found < 0 {
		return nil, 0, refused("notice %d has no update %d", notice.NoticeID, updateID)
	}
	return &notice.Updates[found], found, nil
}

// checkExpectedUpdatedAt refuses a change made from a view of the notice that
// is out of date. A nil expected time skips the check, as the command does.
func checkExpectedUpdatedAt(notice *Notice, expected *time.Time) error {
	// Both are cut to the second, as the form carries it, so a hand-edited
	// updated_at with a fraction of a second does not block every web edit.
	if expected != nil && !notice.UpdatedAt.UTC().Truncate(time.Second).Equal(expected.UTC().Truncate(time.Second)) {
		return fmt.Errorf("%w: notice %d changed while you were editing it; reload the page and try again", ErrConflict, notice.NoticeID)
	}
	return nil
}

// EditNoticeInput changes a notice's own details. A nil field is left as it
// is. ExpectedUpdatedAt, when set, is the notice's updated_at the form was
// built from. AffectedComponentIDs, when set, replaces the components: an
// empty list clears them. SetScheduledStartAt and SetScheduledEndAt say that
// the planned time is changed, to the time or, when nil, to none; ClearSchedule
// removes both.
type EditNoticeInput struct {
	NoticeID             int64
	ExpectedUpdatedAt    *time.Time
	Title                *string
	Impact               *Impact
	AffectedComponentIDs *[]string
	SetScheduledStartAt  bool
	ScheduledStartAt     *time.Time
	SetScheduledEndAt    bool
	ScheduledEndAt       *time.Time
	ClearSchedule        bool
}

// editedAffectedComponents is the new list of a notice's components: the
// page's components that ids name, in the page's order, then the retired
// components the notice already named that ids keep, in their old order. A
// component the page does not list is refused unless the notice already names
// it, so fixing a notice after the settings file dropped a component works.
func editedAffectedComponents(file *File, notice *Notice, ids []string) ([]string, error) {
	listed := func(id string) bool {
		return slices.ContainsFunc(file.Components, func(c Component) bool { return c.ComponentID == id })
	}
	var toCheck []string
	for _, id := range ids {
		if listed(id) || !slices.Contains(notice.AffectedComponentIDs, id) {
			toCheck = append(toCheck, id)
		}
	}
	affected, err := checkedAffectedComponents(file, toCheck)
	if err != nil {
		return nil, err
	}
	for _, id := range notice.AffectedComponentIDs {
		if slices.Contains(ids, id) && !listed(id) && !slices.Contains(affected, id) {
			affected = append(affected, id)
		}
	}
	return affected, nil
}

// EditNotice changes a notice's title, impact, components, or planned window,
// open or resolved. Changing the impact away from maintenance clears the
// planned window, unless the same edit sets a planned time, which is refused.
func EditNotice(in EditNoticeInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if err := checkExpectedUpdatedAt(notice, in.ExpectedUpdatedAt); err != nil {
			return 0, err
		}
		title := notice.Title
		if in.Title != nil {
			if title, err = CheckTitle(*in.Title); err != nil {
				return 0, err
			}
		}
		impact := notice.Impact
		if in.Impact != nil {
			if err := CheckImpact(*in.Impact); err != nil {
				return 0, err
			}
			impact = *in.Impact
		}
		affected := notice.AffectedComponentIDs
		if in.AffectedComponentIDs != nil {
			if affected, err = editedAffectedComponents(file, notice, *in.AffectedComponentIDs); err != nil {
				return 0, err
			}
		}
		start, end := notice.ScheduledStartAt, notice.ScheduledEndAt
		if in.ClearSchedule {
			start, end = nil, nil
		}
		if in.SetScheduledStartAt {
			start = in.ScheduledStartAt
		}
		if in.SetScheduledEndAt {
			end = in.ScheduledEndAt
		}
		explicit := (in.SetScheduledStartAt && in.ScheduledStartAt != nil) || (in.SetScheduledEndAt && in.ScheduledEndAt != nil)
		if impact != ImpactMaintenance && !explicit {
			start, end = nil, nil
		}
		if start, end, err = checkedSchedule(impact, start, end); err != nil {
			return 0, err
		}
		notice.Title, notice.Impact, notice.AffectedComponentIDs, notice.ScheduledStartAt, notice.ScheduledEndAt = title, impact, affected, start, end
		notice.UpdatedAt = now
		return in.NoticeID, nil
	}
}

// EditUpdateInput changes one update of a notice. A nil Message is left as it
// is. SetStage says that the stage is changed to Stage, which is none when
// empty.
type EditUpdateInput struct {
	NoticeID          int64
	UpdateID          int64
	ExpectedUpdatedAt *time.Time
	Message           *string
	SetStage          bool
	Stage             Stage
}

// EditUpdate changes an update's message or stage and marks it edited.
func EditUpdate(in EditUpdateInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if err := checkExpectedUpdatedAt(notice, in.ExpectedUpdatedAt); err != nil {
			return 0, err
		}
		update, _, err := findUpdate(notice, in.UpdateID)
		if err != nil {
			return 0, err
		}
		message := update.Message
		if in.Message != nil {
			if message, err = checkedMessage(*in.Message, "message", false); err != nil {
				return 0, err
			}
		}
		stage := update.Stage
		// Keeping the stage the update has is always allowed, so a notice whose
		// impact changed can still have a message fixed.
		if in.SetStage && in.Stage != update.Stage {
			if err := CheckStage(notice.Impact, in.Stage); err != nil {
				return 0, err
			}
			stage = in.Stage
		}
		if message == update.Message && stage == update.Stage {
			return in.NoticeID, nil
		}
		edited := now
		update.Message, update.Stage, update.EditedAt = message, stage, &edited
		notice.UpdatedAt = now
		return in.NoticeID, nil
	}
}

// DeleteUpdateInput names the update to remove.
type DeleteUpdateInput struct {
	NoticeID          int64
	UpdateID          int64
	ExpectedUpdatedAt *time.Time
}

// DeleteUpdate removes one update. A notice keeps at least one, and its next
// update ID never goes down, so an update ID is never used twice.
func DeleteUpdate(in DeleteUpdateInput) Change {
	return func(file *File, now time.Time) (int64, error) {
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if err := checkExpectedUpdatedAt(notice, in.ExpectedUpdatedAt); err != nil {
			return 0, err
		}
		_, index, err := findUpdate(notice, in.UpdateID)
		if err != nil {
			return 0, err
		}
		if len(notice.Updates) <= 1 {
			return 0, refused("a notice must keep at least one update; delete the notice instead")
		}
		// Keep NextUpdateID above every ID the notice ever gave.
		notice.NextUpdateID = max(notice.NextUpdateID, in.UpdateID+1)
		notice.Updates = slices.Delete(notice.Updates, index, index+1)
		notice.UpdatedAt = now
		return in.NoticeID, nil
	}
}

// DeleteNoticeInput names the notice to remove.
type DeleteNoticeInput struct {
	NoticeID          int64
	ExpectedUpdatedAt *time.Time
}

// DeleteNotice removes a notice, open or resolved, with its updates. The
// file's next notice ID never goes down, so a notice ID is never used twice.
func DeleteNotice(in DeleteNoticeInput) Change {
	return func(file *File, _ time.Time) (int64, error) {
		notice, err := findNotice(file, in.NoticeID)
		if err != nil {
			return 0, err
		}
		if err := checkExpectedUpdatedAt(notice, in.ExpectedUpdatedAt); err != nil {
			return 0, err
		}
		// Keep NextNoticeID above every ID the file ever gave.
		next := max(file.NextNoticeID, 1)
		for _, n := range file.Notices {
			next = max(next, n.NoticeID+1)
		}
		file.NextNoticeID = next
		file.Notices = slices.DeleteFunc(file.Notices, func(n Notice) bool { return n.NoticeID == in.NoticeID })
		return in.NoticeID, nil
	}
}

// ApplyPageConfig sets the page's title, host name, links, and components to
// what the settings file says; the first apply of the file and each later one
// that changed them use it. Notices keep the component IDs they name, even for
// a component the file no longer lists.
func ApplyPageConfig(cfg PageConfig) Change {
	return func(file *File, _ time.Time) (int64, error) {
		title, err := CheckPageTitle(cfg.PageTitle)
		if err != nil {
			return 0, err
		}
		websiteURL, err := CheckWebsiteURL(cfg.WebsiteURL)
		if err != nil {
			return 0, err
		}
		supportURL, err := CheckSupportURL(cfg.SupportURL)
		if err != nil {
			return 0, err
		}
		components, err := CheckComponents(cfg.Components)
		if err != nil {
			return 0, err
		}
		file.PageTitle, file.HostName, file.WebsiteURL, file.SupportURL, file.Components = title, strings.TrimSpace(cfg.HostName), websiteURL, supportURL, components
		return 0, nil
	}
}

// Read returns the published status.json, or ErrNotPublished.
func Read(ctx context.Context, bucket *R2Bucket) (File, error) {
	file, _, err := read(ctx, bucket)
	if errors.Is(err, errObjectMissing) {
		return File{}, ErrNotPublished
	}
	return file, err
}

func read(ctx context.Context, bucket *R2Bucket) (File, string, error) {
	data, etag, err := bucket.get(ctx, "status.json")
	if err != nil {
		return File{}, "", err
	}
	file, err := decodeStatusJSON(data)
	if err != nil {
		return File{}, "", err
	}
	return file, etag, nil
}

// decodeStatusJSON reads status.json as the bucket holds it.
func decodeStatusJSON(data []byte) (File, error) {
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, errors.New("status.json in the bucket is not valid JSON; MMHotline does not overwrite it")
	}
	if file.Notices == nil {
		file.Notices = []Notice{}
	}
	return file, nil
}

// Publish applies change to status.json and uploads all three files. When
// status.json is missing, newPageTitle starts a new file; without one,
// Publish refuses with ErrNotPublished, so a laptop cannot start a second
// page. Another post that changed status.json between this post's read and
// its write makes the write fail its If-Match check; Publish then reads the
// newer file and applies change again, so neither post is lost. The File it
// returns is the newest status.json it saw.
func Publish(ctx context.Context, bucket *R2Bucket, change Change, newPageTitle string, now time.Time) (File, int64, error) {
	now = now.UTC().Truncate(time.Second)
	for attempt := 1; ; attempt++ {
		file, etag, err := read(ctx, bucket)
		if errors.Is(err, errObjectMissing) {
			if newPageTitle == "" {
				return File{}, 0, ErrNotPublished
			}
			file, etag, err = File{PageTitle: newPageTitle, Notices: []Notice{}}, "", nil
		}
		if err != nil {
			return File{}, 0, err
		}
		noticeID, err := change(&file, now)
		if err != nil {
			return File{}, 0, err
		}
		file.PublishedAt = now
		content, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return File{}, 0, err
		}
		content = append(content, '\n')
		// Refuse before the upload: a status.json larger than the read limit
		// would be saved, and then no one could read it back to post again.
		if len(content) > maximumStatusJSONBytes {
			return File{}, 0, refused("status.json would be %d bytes, more than the %d bytes MMHotline reads back, so the change was not saved", len(content), maximumStatusJSONBytes)
		}
		etag, err = bucket.put(ctx, "status.json", content, "application/json", etag)
		if errors.Is(err, errPreconditionFailed) && attempt < publishAttempts {
			continue
		}
		if errors.Is(err, errPreconditionFailed) {
			return File{}, 0, errors.New("status.json kept changing, so the post was not saved; post again")
		}
		if err != nil {
			return File{}, 0, err
		}
		file, err = publishArtifacts(ctx, bucket, file, etag)
		return file, noticeID, err
	}
}

// publishArtifacts uploads index.html and feed.xml, both built from file, the
// status.json saved with the given ETag. A page without a host name has no
// feed, because the feed's links are absolute. Another post may save a newer
// status.json and upload its files before this one arrives, so after both
// uploads publishArtifacts reads status.json again; when it changed, it
// builds both files from the newer file and uploads them again. The files
// uploaded last therefore show the latest status.json. A failed upload does
// not stop the other: each file is tried, so neither is skipped silently, and
// status.json is read again even then, so an upload that did succeed cannot
// leave an older file over a newer post's. publishArtifacts returns the newest
// file it read.
func publishArtifacts(ctx context.Context, bucket *R2Bucket, file File, etag string) (File, error) {
	for upload := 1; ; upload++ {
		page, err := RenderPage(file)
		if err != nil {
			return file, err
		}
		type artifact struct {
			key, contentType string
			content          []byte
		}
		artifacts := []artifact{{"index.html", "text/html; charset=utf-8", page}}
		if file.HostName != "" {
			feed, err := RenderFeed(file)
			if err != nil {
				return file, err
			}
			artifacts = append(artifacts, artifact{"feed.xml", "application/atom+xml; charset=utf-8", feed})
		}
		var failedKeys []string
		var failures []error
		for _, a := range artifacts {
			if _, err := bucket.put(ctx, a.key, a.content, a.contentType, ""); err != nil {
				failedKeys = append(failedKeys, a.key)
				failures = append(failures, err)
			}
		}
		// A failed upload still gets the re-read: when another post saved a
		// newer status.json meanwhile, the uploads that did succeed may have
		// replaced that post's newer files with older ones, so both are built
		// again from the newer file.
		uploadFailure := func() error {
			return fmt.Errorf("the post is saved in status.json, but MMHotline could not confirm the upload of %s, so the public page may show the post only after the next post: %w", strings.Join(failedKeys, " and "), errors.Join(failures...))
		}
		latest, latestETag, err := read(ctx, bucket)
		if err != nil {
			if len(failures) > 0 {
				return file, uploadFailure()
			}
			return file, fmt.Errorf("the post is saved in status.json, but MMHotline could not check whether index.html and feed.xml show the latest notices; the public page may need another post to show the saved notices: %w", err)
		}
		if latestETag == etag {
			if len(failures) > 0 {
				return file, uploadFailure()
			}
			return file, nil
		}
		if upload == publishAttempts {
			if len(failures) > 0 {
				return latest, uploadFailure()
			}
			return latest, errors.New("the post is saved in status.json, but status.json kept changing during the uploads of index.html and feed.xml; the public page may need another post to show the latest notices")
		}
		file, etag = latest, latestETag
	}
}

// The page's times are absolute and in UTC in the file, never "5 minutes
// ago", because the files change only at the next post. Each time is the text
// of its own <time> element, and a <span class="tz">UTC</span> beside it says
// the zone. A notice's first line gives the whole date and time, such as "26
// October 2026, 08:00"; its updates below give the time of day, with the day
// when it is not the notice's first: "08:09", "27 Oct 02:00". local_time.js
// replaces the text with the visitor's local time and hides the "UTC" spans.

func publishedTime(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04") }

func sameDay(t, day time.Time) bool {
	return t.UTC().Format(time.DateOnly) == day.UTC().Format(time.DateOnly)
}

// updateTime writes the time of an update of a notice that opened on day.
func updateTime(t, day time.Time) string {
	if sameDay(t, day) {
		return t.UTC().Format("15:04")
	}
	return t.UTC().Format("2 Jan 15:04")
}

func isoTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// newestFirst returns the updates of a notice, newest first, as the page shows them.
func newestFirst(updates []Update) []Update {
	result := make([]Update, len(updates))
	for index, update := range updates {
		result[len(updates)-1-index] = update
	}
	return result
}

// pageStylesheet is the page's styles, inlined in index.html. It is
// standalone: the page cannot load MMHotline's stylesheet.
//
//go:embed status_page.css
var pageStylesheet string

// localTimeScript shows each <time> in the visitor's time zone, inlined in
// index.html. The page is complete and in UTC without it.
//
//go:embed local_time.js
var localTimeScript string

// impactPresentation is how the page shows each impact: the CSS color class
// that carries its meaning (sos red, warn amber, work blue) and the tag text.
var impactPresentation = map[Impact]struct{ class, tag string }{
	ImpactOutage:      {"sos", "Outage"},
	ImpactDegraded:    {"warn", "Degraded"},
	ImpactMaintenance: {"work", "Maintenance"},
}

func impactClass(impact Impact) string { return impactPresentation[displayedImpact(impact)].class }
func impactTag(impact Impact) string   { return impactPresentation[displayedImpact(impact)].tag }

// resolvedTag is the tag of a resolved notice: maintenance is completed.
func resolvedTag(impact Impact) string {
	if displayedImpact(impact) == ImpactMaintenance {
		return "Completed"
	}
	return "Resolved"
}

var stageLabels = map[Stage]string{
	StageInvestigating: "Investigating", StageIdentified: "Identified", StageMonitoring: "Monitoring", StageResolved: "Resolved",
	StageScheduled: "Scheduled", StageInProgress: "In progress", StageCompleted: "Completed",
}

// Stages returns the stages that fit a notice with the impact, in the order a
// notice moves through them.
func Stages(impact Impact) []Stage {
	if impact == ImpactMaintenance {
		return slices.Clone(maintenanceStages)
	}
	return slices.Clone(incidentStages)
}

// StageLabel is the stage's label, such as "In progress"; a stage that only a
// file edited by hand can hold shows as written.
func StageLabel(stage Stage) string {
	if label, known := stageLabels[stage]; known {
		return label
	}
	return string(stage)
}

// componentPresentation is how the page shows each component status: the CSS
// color class and the tag text.
var componentPresentation = map[OverallStatus]struct{ class, tag string }{
	StatusOperational: {"ok", "Operational"},
	StatusDegraded:    {"warn", "Degraded"},
	StatusOutage:      {"sos", "Outage"},
	StatusMaintenance: {"work", "Maintenance"},
}

func componentStatusClass(status OverallStatus) string { return componentPresentation[status].class }
func componentStatusLabel(status OverallStatus) string { return componentPresentation[status].tag }

// headerLinks are the page's website and support links. status.json may have
// been edited by hand, so each is checked again here, and a link that fails
// the check is left out.
func headerLinks(file File) (websiteURL, supportURL string) {
	websiteURL, _ = CheckWebsiteURL(file.WebsiteURL)
	supportURL, _ = CheckSupportURL(file.SupportURL)
	return websiteURL, supportURL
}

// banners is the overall status banner at the top of the page. The
// maintenance banner says nothing about time, so it is true both before and
// during the planned window.
var banners = map[OverallStatus]struct{ class, tag, sentence string }{
	StatusOperational: {"ok", "Working", "Everything is working."},
	StatusMaintenance: {"work", "Maintenance", "Scheduled maintenance"},
	StatusDegraded:    {"warn", "Degraded", "Some systems are experiencing issues."},
	StatusOutage:      {"sos", "Outage", "We are experiencing a service outage."},
}

// The page's icon is a colored dot as an inline SVG data: URL, so it needs no
// file in the bucket. Each <link> is fixed text with the dot's color in it:
// green #1f9d5c, amber #b86e00, red #c4231c, blue #2348b3. The # is written
// %23 because a data: URL ends at a plain #.
const (
	faviconStart = `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ccircle cx='8' cy='8' r='7' fill='%23`
	faviconEnd   = `'/%3E%3C/svg%3E"/>`
)

var faviconLinks = map[OverallStatus]string{
	StatusOperational: faviconStart + "1f9d5c" + faviconEnd,
	StatusDegraded:    faviconStart + "b86e00" + faviconEnd,
	StatusOutage:      faviconStart + "c4231c" + faviconEnd,
	StatusMaintenance: faviconStart + "2348b3" + faviconEnd,
}

// RenderPage builds index.html from status.json's content: the overall status
// ("Everything is working." when no notice is open), the open notices, then
// the most recently resolved notices.
func RenderPage(file File) ([]byte, error) {
	var page bytes.Buffer
	err := statusPage(file, file.OpenNotices(), file.ResolvedNotices(recentlyResolvedOnPage)).Render(context.Background(), &page)
	return page.Bytes(), err
}
