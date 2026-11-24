package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"mmhotline/internal/systemsrepository"
)

// The dashboard's parts below the incidents: the servers, what changed in
// the systems repository, and the checklist's items that need a person
// (docs/changing-mmhotline/design/interface.md#dashboard). None has a setting of its own; a
// missing source makes its part say so in one line.

// checklistFileName is the checklist at the root of the systems repository,
// in the Monkeymaxxing Stack's format.
const checklistFileName = "monkeymaxxing_stack_checklist.yml"

// latestCommitCount: ten commits fit one screen; the Git host keeps the rest.
const latestCommitCount = 10

// systemPanelsReadEvery is how long one read of the three sources serves
// every viewer. It equals the page's refresh, so any number of open
// dashboards start the fleet view command, which runs as root, and Git at
// most once in 10 seconds, and nothing runs while nobody looks.
const systemPanelsReadEvery = 10 * time.Second

// systemPanels is one read of the three sources. Each part holds its rows,
// or why it has none.
type systemPanels struct {
	ServersNeedingAttention, HealthyServers []fleetServer
	ServersUnavailable                      unavailablePart
	// ServersSetAside is the line about the findings the fleet hub sets
	// aside, or "" (fleetView.setAsideLine).
	ServersSetAside    string
	Changes            []changeView
	ChangesUnavailable unavailablePart
	// ChecklistItems are the setup items that need a person, of
	// ChecklistTotal setup items. ChecklistLater is every item for later,
	// when it happens, such as a drill or a new team member, which is never
	// unfinished setup.
	ChecklistItems       []checklistItemView
	ChecklistTotal       int
	ChecklistLater       []checklistItemView
	ChecklistUnavailable unavailablePart
}

// unavailablePart says why a part has nothing to show: Reason in one plain
// line, and Detail, the technical cause, such as an error or the directory
// MMHotline looked in, which the page folds away. Both are "" while the
// part shows its rows.
type unavailablePart struct{ Reason, Detail string }

type changeView struct {
	ShortID, Subject string
	CommittedAt      time.Time
}

// checklistItemView is one item that needs a person as the dashboard shows it.
type checklistItemView struct {
	Title, Status, Detail, ProtectsAgainst string
	// A done item whose verification is due again was last verified at
	// LastVerifiedAt, and is due again since DueAgainSince, every
	// VerifyAgainEvery. Another done item has only LastVerifiedAt, when
	// verified_at reads. Zero times for every other item.
	LastVerifiedAt, DueAgainSince time.Time
	VerifyAgainEvery              string
	// Question is the prompt that "Copy a question for your coding agent"
	// copies.
	Question string
}

// ServersSummary counts the servers and those that need attention, for the
// part's heading: "1 of 5 needs attention", "All 5 healthy".
func (p systemPanels) ServersSummary() string {
	failing, all := len(p.ServersNeedingAttention), len(p.ServersNeedingAttention)+len(p.HealthyServers)
	switch {
	case failing == 1:
		return "1 of " + strconv.Itoa(all) + " needs attention"
	case failing > 1:
		return strconv.Itoa(failing) + " of " + strconv.Itoa(all) + " need attention"
	case all == 1:
		return "Healthy"
	}
	return "All " + strconv.Itoa(all) + " healthy"
}

// ChecklistDone counts the checklist's items that need nobody now.
func (p systemPanels) ChecklistDone() int { return p.ChecklistTotal - len(p.ChecklistItems) }

// ChecklistDeviated counts the items done another way than the stack's.
func (p systemPanels) ChecklistDeviated() int {
	deviated := 0
	for _, item := range p.ChecklistItems {
		if item.Status == "Deviated" {
			deviated++
		}
	}
	return deviated
}

// checklistNext is the index of the item to do next: the first, in the
// file's order, that is not done another way, or else the first; -1 when no
// item needs a person.
func (p systemPanels) checklistNext() int {
	for index, item := range p.ChecklistItems {
		if item.Status != "Deviated" {
			return index
		}
	}
	if len(p.ChecklistItems) == 0 {
		return -1
	}
	return 0
}

// ChecklistNext is the item to do next, which the page shows; nil when no
// item needs a person.
func (p systemPanels) ChecklistNext() *checklistItemView {
	if next := p.checklistNext(); next >= 0 {
		return &p.ChecklistItems[next]
	}
	return nil
}

// ChecklistOthers are the other items that need a person, which the page
// folds.
func (p systemPanels) ChecklistOthers() []checklistItemView {
	next := p.checklistNext()
	if next < 0 {
		return nil
	}
	return slices.Concat(p.ChecklistItems[:next], p.ChecklistItems[next+1:])
}

// ChecklistLaterDueAgain counts the items for later whose verification is
// due again.
func (p systemPanels) ChecklistLaterDueAgain() int {
	due := 0
	for _, item := range p.ChecklistLater {
		if item.Status == "Open again" {
			due++
		}
	}
	return due
}

// ChecklistOpen counts the items still to do: open ones, done ones due to be
// verified again, and ones MMHotline cannot read.
func (p systemPanels) ChecklistOpen() int { return len(p.ChecklistItems) - p.ChecklistDeviated() }

// systemPanelsCache holds the latest read; its lock also makes concurrent
// viewers wait for one read rather than start one each.
type systemPanelsCache struct {
	mu     sync.Mutex
	readAt time.Time
	panels systemPanels
}

// readSystemPanels returns the latest read of the three sources, reading
// them again when it is older than systemPanelsReadEvery.
func (a *App) readSystemPanels(ctx context.Context) systemPanels {
	cache := &a.systemPanelsCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := a.Now()
	if age := now.Sub(cache.readAt); !cache.readAt.IsZero() && age >= 0 && age < systemPanelsReadEvery {
		return cache.panels
	}
	// Every viewer shares this read, so one viewer leaving must not cut it short.
	ctx = context.WithoutCancel(ctx)
	var panels systemPanels
	panels.ServersNeedingAttention, panels.HealthyServers, panels.ServersSetAside, panels.ServersUnavailable = a.readServers(ctx)
	a.readSystemsRepositoryPanels(ctx, now, &panels)
	cache.readAt, cache.panels = now, panels
	return panels
}

func (a *App) readServers(ctx context.Context) (needAttention, healthy []fleetServer, setAside string, unavailable unavailablePart) {
	if len(a.fleetViewCommand()) == 0 {
		return nil, nil, "", unavailablePart{Reason: "The settings file has no fleet_view_command, so MMHotline shows no servers."}
	}
	if a.FleetViewSocket == "" {
		return nil, nil, "", unavailablePart{Reason: "MMHotline cannot show the servers: mmhotline serve was started without --fleet-view-socket."}
	}
	view, err := readFleetView(ctx, a.FleetViewSocket)
	if err != nil {
		return nil, nil, "", unavailablePart{Reason: "MMHotline cannot show the servers.", Detail: err.Error() + "."}
	}
	if len(view.Nodes) == 0 {
		return nil, nil, "", unavailablePart{Reason: "The fleet view lists no servers yet."}
	}
	needAttention, healthy = fleetServers(view)
	return needAttention, healthy, view.setAsideLine(), unavailablePart{}
}

// readSystemsRepositoryPanels reads what changed and the checklist from the
// same commit, the newest of the branch MMHotline's copy has checked out,
// into panels.
func (a *App) readSystemsRepositoryPanels(ctx context.Context, now time.Time, panels *systemPanels) {
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	if _, err := os.Stat(filepath.Join(directory, ".git")); errors.Is(err, os.ErrNotExist) {
		where := "MMHotline looks for the systems repository copy at " + directory + "."
		panels.ChangesUnavailable = unavailablePart{"MMHotline has no copy of the systems repository.", where}
		panels.ChecklistUnavailable = unavailablePart{"MMHotline has no copy of the systems repository, so there is no checklist.", where}
		return
	}
	commits, err := systemsrepository.LatestCommits(ctx, directory, latestCommitCount)
	if err != nil {
		unreadable := unavailablePart{"MMHotline cannot read its copy of the systems repository.", err.Error() + "."}
		panels.ChangesUnavailable, panels.ChecklistUnavailable = unreadable, unreadable
		return
	}
	for _, commit := range commits {
		panels.Changes = append(panels.Changes, changeView{ShortID: commit.ID[:12], CommittedAt: commit.CommittedAt, Subject: commit.Subject})
	}
	cannotRead := "MMHotline cannot read " + checklistFileName + "."
	text, found, err := systemsrepository.FileAtCommitIfPresent(ctx, directory, commits[0].ID, checklistFileName) //nolint:nilaway // LatestCommits returns at least one commit, HEAD, or an error.
	switch {
	case err != nil:
		panels.ChecklistUnavailable = unavailablePart{cannotRead, err.Error() + "."}
		return
	case !found:
		panels.ChecklistUnavailable = unavailablePart{Reason: "The systems repository has no " + checklistFileName + " at its root."}
		return
	}
	all, err := checklistItems([]byte(text))
	if err != nil {
		panels.ChecklistUnavailable = unavailablePart{cannotRead, err.Error() + "."}
		return
	}
	var setup []checklistItem
	for _, item := range all {
		if item.Part != checklistPartLater {
			setup = append(setup, item)
			continue
		}
		view, _ := checklistItemViewAt(item, now)
		if item.State == "open" {
			view.Status = "When it happens"
		}
		panels.ChecklistLater = append(panels.ChecklistLater, view)
	}
	panels.ChecklistItems, panels.ChecklistTotal = openChecklistItems(setup, now), len(setup)
}

// The checklist's two parts. An item without part is a setup item, as every
// item was before the checklist had parts, and so is each app's item.
const (
	checklistPartSetup = "setup"
	checklistPartLater = "later_when_it_happens"
)

// checklistItem is one item of the checklist, with the fields the dashboard
// reads; it ignores the others.
type checklistItem struct {
	ID               string `yaml:"id"`
	Title            string `yaml:"title"`
	Part             string `yaml:"part"`
	State            string `yaml:"state"`
	ProtectsAgainst  string `yaml:"protects_against"`
	WaitsFor         string `yaml:"waits_for"`
	Deviation        string `yaml:"deviation"`
	VerifiedAt       string `yaml:"verified_at"`
	VerifyAgainEvery string `yaml:"verify_again_every"`
}

// checklistItems returns the checklist's items in the file's order, which is
// the order the system is built in: the top-level items list and each app's
// items, wherever each stands in the file. Every other section, such as
// what_this_system_does_not_survive, holds no items.
func checklistItems(text []byte) ([]checklistItem, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(text, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("monkeymaxxing_stack_checklist.yml must contain a YAML mapping with items and apps")
	}
	root := document.Content[0]
	var items []checklistItem
	for index := 0; index+1 < len(root.Content); index += 2 {
		value := root.Content[index+1]
		switch root.Content[index].Value {
		case "items":
			var list []checklistItem
			if err := value.Decode(&list); err != nil {
				return nil, fmt.Errorf("items: %w", err)
			}
			items = append(items, list...)
		case "apps":
			if value.Kind != yaml.MappingNode {
				return nil, errors.New("apps is not a mapping from each app's name to its items")
			}
			for app := 0; app+1 < len(value.Content); app += 2 {
				var appItems struct {
					Items []checklistItem `yaml:"items"`
				}
				if err := value.Content[app+1].Decode(&appItems); err != nil {
					return nil, fmt.Errorf("apps, %s: %w", value.Content[app].Value, err)
				}
				items = append(items, appItems.Items...)
			}
		}
	}
	return items, nil
}

// openChecklistItems keeps the items that need a person, as
// checklistItemViewAt judges each.
func openChecklistItems(items []checklistItem, now time.Time) []checklistItemView {
	var views []checklistItemView
	for _, item := range items {
		if view, needsPerson := checklistItemViewAt(item, now); needsPerson {
			views = append(views, view)
		}
	}
	return views
}

// checklistItemViewAt shows one item, and says whether it needs a person:
// an open or deviated item, and a done one whose verified_at is older than
// its verify_again_every, which counts as open again. An item whose state,
// part, or times MMHotline cannot read needs a person too, with the reason,
// so that a typing mistake never hides an item.
func checklistItemViewAt(item checklistItem, now time.Time) (checklistItemView, bool) {
	view := checklistItemView{Title: item.Title, ProtectsAgainst: item.ProtectsAgainst, Status: "Open"}
	if view.Title == "" {
		view.Title = item.ID
	}
	view.Question = "Explain checklist item '" + view.Title + "'"
	if item.ID != "" {
		view.Question += " (" + item.ID + " in " + checklistFileName + ")"
	}
	view.Question += " of my system and what it protects against."
	if item.Part != "" && item.Part != checklistPartSetup && item.Part != checklistPartLater {
		view.Detail = fmt.Sprintf("The checklist item has part %q. A checklist item's part must be %s or %s.", item.Part, checklistPartSetup, checklistPartLater)
		return view, true
	}
	switch item.State {
	case "done":
		view.Status = "Done"
		if verifiedAt, err := time.Parse(time.RFC3339, item.VerifiedAt); err == nil {
			view.LastVerifiedAt = verifiedAt
		}
		if item.VerifyAgainEvery == "" {
			return view, false
		}
		verifiedAt, due, problem := verificationDue(item)
		switch {
		case problem != "":
			view.Detail = problem
		case now.Before(due):
			return view, false
		default:
			view.LastVerifiedAt, view.DueAgainSince, view.VerifyAgainEvery = verifiedAt, due, item.VerifyAgainEvery
		}
		view.Status = "Open again"
	case "deviated":
		view.Status = "Deviated"
		if item.Deviation != "" {
			view.Detail = "Deviation: " + item.Deviation
		}
	case "open":
		if item.WaitsFor != "" {
			view.Detail = "Waits for: " + item.WaitsFor
		}
	default:
		view.Detail = fmt.Sprintf("The checklist item has state %q. A checklist item must be open, done, or deviated.", item.State)
	}
	return view, true
}

// verificationDue reads when a done item was last verified and when it is
// due again, or says why MMHotline cannot tell.
func verificationDue(item checklistItem) (verifiedAt, due time.Time, problem string) {
	verifiedAt, err := time.Parse(time.RFC3339, item.VerifiedAt)
	if err != nil {
		return verifiedAt, due, fmt.Sprintf("The checklist item has verified_at %q, which is not a time such as 2026-11-03T04:12:00Z. MMHotline cannot determine the last verification time.", item.VerifiedAt)
	}
	due, ok := addVerificationInterval(verifiedAt, item.VerifyAgainEvery)
	if !ok {
		return verifiedAt, due, fmt.Sprintf("The checklist item has verify_again_every %q, which is not an interval such as 7d or 6 months. MMHotline cannot determine the next verification time.", item.VerifyAgainEvery)
	}
	return verifiedAt, due, ""
}

// verificationIntervalPattern reads verify_again_every, such as 7d, 2 weeks,
// 6 months, or 1 year. A bare m is refused: it could mean minutes or months.
var verificationIntervalPattern = regexp.MustCompile(`^([1-9][0-9]{0,3}) ?(h|hours?|d|days?|w|weeks?|months?|years?)$`)

func addVerificationInterval(t time.Time, interval string) (time.Time, bool) {
	match := verificationIntervalPattern.FindStringSubmatch(strings.TrimSpace(interval))
	if match == nil {
		return time.Time{}, false
	}
	count, _ := strconv.Atoi(match[1])
	switch match[2][0] {
	case 'h':
		return t.Add(time.Duration(count) * time.Hour), true
	case 'd':
		return t.AddDate(0, 0, count), true
	case 'w':
		return t.AddDate(0, 0, 7*count), true
	case 'm':
		return t.AddDate(0, count, 0), true
	default: // 'y'
		return t.AddDate(count, 0, 0), true
	}
}

// attentionLine says what one check of a server that needs attention found.
// clock writes a time as the page shows it.
func attentionLine(serverSilent bool, check fleetViewAttention, clock func(time.Time) string) string {
	var line string
	if serverSilent {
		line = "Last known: "
	}
	line += check.Check
	if check.Finding != "" {
		line += ", " + check.Finding
	}
	line += " " + check.State
	if !check.Since.IsZero() {
		line += " since " + clock(check.Since)
	}
	if check.Message != "" {
		line += ": " + check.Message
	}
	return line
}

// copyQuestionScript copies an item's question. It listens on the document,
// because the refresh replaces the sections and their buttons. Without the
// clipboard, such as on a page opened over plain HTTP from another address,
// it shows the question to copy by hand.
const copyQuestionScript = `"use strict";document.addEventListener("click",async event=>{const button=event.target.closest("[data-copy-question]");if(!button)return;const status=button.nextElementSibling;try{await navigator.clipboard.writeText(button.dataset.copyQuestion);status.textContent="Question copied. Paste the question into your coding agent.";}catch(_){status.textContent="Your browser did not allow copying. The question: "+button.dataset.copyQuestion;}});`
