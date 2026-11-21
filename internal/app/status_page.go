package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"mmhotline/internal/cloudflareapi"
	"mmhotline/internal/statuspage"
)

const statusPageSchema = `
CREATE TABLE status_page_setup (
 id INTEGER PRIMARY KEY CHECK(id=1),
 cloudflare_account_id TEXT NOT NULL,
 host_name TEXT NOT NULL,
 bucket_name TEXT NOT NULL,
 r2_bucket_url TEXT NOT NULL,
 page_title TEXT NOT NULL,
 r2_access_key_id TEXT NOT NULL,
 r2_secret_access_key TEXT NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);
` + statusPageIncidentNoticesSchema

// statusPageIncidentNoticesSchema links an incident to the notice its page
// posted, so the incident page shows that notice instead of a button that
// would post a second, identical one. notice_id is 0 while the post is
// being published. The public status.json never names an incident.
const statusPageIncidentNoticesSchema = `
CREATE TABLE status_page_incident_notices (
 incident_id TEXT PRIMARY KEY,
 notice_id INTEGER NOT NULL,
 resolved INTEGER NOT NULL DEFAULT 0
);
`

// A notice posted with one click from an incident page is an outage with this
// title and this first message, at the stage investigating. Its people add
// their own words next.
const (
	StatusPageGenericTitle   = "Service disruption"
	StatusPageGenericMessage = "We are looking into a problem."
)

// StatusPageView is what the status page's page and command show. It never
// contains the R2 secret access key.
type StatusPageView struct {
	IsSetUp             bool             `json:"is_set_up"`
	CloudflareAccountID string           `json:"cloudflare_account_id,omitempty"`
	HostName            string           `json:"host_name,omitempty"`
	PageURL             string           `json:"page_url,omitempty"`
	BucketName          string           `json:"bucket_name,omitempty"`
	R2BucketURL         string           `json:"r2_bucket_url,omitempty"`
	R2AccessKeyID       string           `json:"r2_access_key_id,omitempty"`
	SetUpAt             *time.Time       `json:"set_up_at,omitempty"`
	SetUpBy             string           `json:"set_up_by,omitempty"`
	PublishedStatusJSON *statuspage.File `json:"published_status_json,omitempty"`
	// ReadError says why status.json could not be read, such as an R2 outage.
	ReadError string `json:"read_error,omitempty"`
}

type statusPageSetup struct {
	accountID, hostName, bucketName, bucketURL, pageTitle string
	accessKeyID, secretAccessKey                          string
	setUpAt                                               time.Time
	setUpBy                                               string
}

var (
	cloudflareAccountIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hostNameLabelPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	r2AccessKeyIDPattern       = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)
)

func checkCloudflareAccountID(accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	if !cloudflareAccountIDPattern.MatchString(accountID) {
		return "", fmt.Errorf("%w: the Cloudflare account ID must be 32 lowercase hexadecimal characters, as the dashboard shows it", ErrInvalid)
	}
	return accountID, nil
}

func checkCloudflareAPIToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 1024 || strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", fmt.Errorf("%w: the Cloudflare API token must be one line of text", ErrInvalid)
	}
	return token, nil
}

func checkStatusPageHostName(hostName string) (string, error) {
	hostName = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostName)), ".")
	labels := strings.Split(hostName, ".")
	valid := len(hostName) <= 253 && len(labels) >= 2
	for _, label := range labels {
		valid = valid && hostNameLabelPattern.MatchString(label)
	}
	if !valid {
		return "", fmt.Errorf("%w: the host name must be a DNS name such as status.example.com", ErrInvalid)
	}
	return hostName, nil
}

var notLetterOrDigit = regexp.MustCompile(`[^a-z0-9]+`)

// cloudflareName names the MMHotline-down alarm's Worker or the status page's bucket
// after a host name: the prefix, the host name with hyphens for its other
// characters, and the first 8 hexadecimal characters of the SHA-256 of the
// lowercase host name, such as mmhotline-status-status-example-com-18a8ea11.
// The same host name always gives the same name, so a set-up after a lost
// database finds its own Worker and bucket. The digest gives host names that
// differ only in punctuation, such as ops.eu.example.com and
// ops-eu.example.com, different names, so a second installation in the same
// account never takes over the first one's. A workers.dev Worker name and an
// R2 bucket name both have at most 63 characters, so the readable part of a
// long host name is cut short.
func cloudflareName(prefix, hostName string) string {
	hostName = strings.ToLower(hostName)
	sum := sha256.Sum256([]byte(hostName))
	digest := hex.EncodeToString(sum[:4])
	readable := strings.Trim(notLetterOrDigit.ReplaceAllString(hostName, "-"), "-")
	readable = readable[:min(len(readable), 63-len(prefix+"--"+digest))]
	return strings.TrimRight(prefix+"-"+readable, "-") + "-" + digest
}

// statusPageBucketName names the bucket after the host name, so two teams'
// pages in one Cloudflare account never share a bucket.
func statusPageBucketName(hostName string) string {
	return cloudflareName("mmhotline-status", hostName)
}

// statusPageRewriteRule serves index.html at the host name's bare address.
// R2 does not serve index.html for "/" by itself.
func statusPageRewriteRule(hostName string) cloudflareapi.Rule {
	digest := sha256.Sum256([]byte(hostName))
	return cloudflareapi.Rule{
		Ref:              "mmhotline_status_page_" + hex.EncodeToString(digest[:6]),
		Description:      "MMHotline status page: serve index.html at https://" + hostName + "/",
		Expression:       fmt.Sprintf(`(http.host eq "%s" and http.request.uri.path eq "/")`, hostName),
		Action:           "rewrite",
		ActionParameters: map[string]any{"uri": map[string]any{"path": map[string]any{"value": "/index.html"}}},
	}
}

func readStatusPageSetup(ctx context.Context, tx *sql.Tx) (statusPageSetup, bool, error) {
	var s statusPageSetup
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT cloudflare_account_id,host_name,bucket_name,r2_bucket_url,page_title,r2_access_key_id,r2_secret_access_key,set_up_at,set_up_by FROM status_page_setup WHERE id=1`).Scan(&s.accountID, &s.hostName, &s.bucketName, &s.bucketURL, &s.pageTitle, &s.accessKeyID, &s.secretAccessKey, &at, &s.setUpBy)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	s.setUpAt = time.Unix(at, 0).UTC()
	return s, err == nil, err
}

func (a *App) statusPageBucket(s statusPageSetup) *statuspage.R2Bucket {
	return &statuspage.R2Bucket{BucketURL: s.bucketURL, AccessKeyID: s.accessKeyID, SecretAccessKey: s.secretAccessKey, Now: a.Now}
}

func (s statusPageSetup) view() StatusPageView {
	at := s.setUpAt
	return StatusPageView{IsSetUp: true, CloudflareAccountID: s.accountID, HostName: s.hostName, PageURL: "https://" + s.hostName + "/", BucketName: s.bucketName, R2BucketURL: s.bucketURL, R2AccessKeyID: s.accessKeyID, SetUpAt: &at, SetUpBy: s.setUpBy}
}

// r2BucketURLParts reads the account ID and the bucket's name from an R2
// bucket URL, such as https://ACCOUNT_ID.r2.cloudflarestorage.com/BUCKET;
// the account ID is empty for another address, such as a test's.
func r2BucketURLParts(bucketURL string) (accountID, bucketName string) {
	u, err := url.Parse(bucketURL)
	if err != nil {
		return "", ""
	}
	if id, isR2 := strings.CutSuffix(u.Hostname(), ".r2.cloudflarestorage.com"); isR2 && cloudflareAccountIDPattern.MatchString(id) {
		accountID = id
	}
	return accountID, path.Base(strings.TrimRight(u.Path, "/"))
}

// StatusPageCloudflareSetUp is what status-page set-up did in the team's
// Cloudflare account, and what the person does next.
type StatusPageCloudflareSetUp struct {
	CloudflareAccountID string `json:"cloudflare_account_id"`
	HostName            string `json:"host_name"`
	BucketName          string `json:"bucket_name"`
	// R2BucketURL goes in the settings file's status_page.r2_bucket_url.
	R2BucketURL string `json:"r2_bucket_url"`
	PageURL     string `json:"page_url"`
	NextStep    string `json:"next_step"`
}

// Text is what status-page set-up prints.
func (s StatusPageCloudflareSetUp) Text() string {
	return fmt.Sprintf("The status page is set up in Cloudflare account %s: the R2 bucket %s, the host name %s connected to it, and the rule that serves index.html at %s.\nr2_bucket_url: %s\nNext step: %s",
		s.CloudflareAccountID, s.BucketName, s.HostName, s.PageURL, s.R2BucketURL, s.NextStep)
}

// SetUpStatusPageInCloudflare creates the status page's R2 bucket,
// connects the host name to it, and adds the URL rewrite rule, in the
// team's Cloudflare account, with the person's own API token, which it
// never stores. It runs on the person's laptop, without MMHotline (CA8), so
// MMHotline never holds a token that can create other tokens. Each step
// first checks whether it is already done, so running it again changes only
// what is missing. It publishes no page: writing to the bucket needs the R2
// access key the person creates next, and the settings file's first apply
// posts the page with it.
func SetUpStatusPageInCloudflare(ctx context.Context, addresses cloudflareapi.Addresses, accountID, hostName, apiToken string) (StatusPageCloudflareSetUp, error) {
	accountID, err := checkCloudflareAccountID(accountID)
	if err != nil {
		return StatusPageCloudflareSetUp{}, errors.New(refusalText(err))
	}
	hostName, err = checkStatusPageHostName(hostName)
	if err != nil {
		return StatusPageCloudflareSetUp{}, errors.New(refusalText(err))
	}
	token, err := checkCloudflareAPIToken(apiToken)
	if err != nil {
		return StatusPageCloudflareSetUp{}, errors.New(refusalText(err))
	}
	setup := statusPageSetup{accountID: accountID, hostName: hostName, bucketName: statusPageBucketName(hostName)}
	if err := setUpStatusPageInCloudflare(ctx, &cloudflareapi.Client{APIBaseURL: addresses.APIBaseURL, APIToken: token}, setup); err != nil {
		return StatusPageCloudflareSetUp{}, err
	}
	result := StatusPageCloudflareSetUp{CloudflareAccountID: accountID, HostName: hostName, BucketName: setup.bucketName, R2BucketURL: addresses.R2BucketURL(accountID, setup.bucketName), PageURL: "https://" + hostName + "/"}
	result.NextStep = "on Cloudflare's site, open R2, then Manage API tokens, and create an API token with the permission Object Read & Write for the bucket " + setup.bucketName + " only. Put r2_bucket_url above and the token's Access Key ID and Secret Access Key in the settings file's status_page section, as r2_bucket_url, r2_access_key_id, and r2_secret_access_key (with a deploy tool, in its vault), then apply the file, with your deploy tool or with sudo mmhotline settings apply on the MMHotline server. The first apply publishes the page."
	return result, nil
}

func setUpStatusPageInCloudflare(ctx context.Context, client *cloudflareapi.Client, setup statusPageSetup) error {
	zoneID, _, err := client.ZoneForHostName(ctx, setup.accountID, setup.hostName)
	if err != nil {
		return err
	}
	exists, err := client.BucketExists(ctx, setup.accountID, setup.bucketName)
	if err != nil {
		return err
	}
	if !exists {
		if err := client.CreateBucket(ctx, setup.accountID, setup.bucketName); err != nil {
			return err
		}
	}
	domains, err := client.BucketCustomDomains(ctx, setup.accountID, setup.bucketName)
	if err != nil {
		return err
	}
	connected := false
	for _, domain := range domains {
		connected = connected || domain.Domain == setup.hostName
	}
	if !connected {
		if err := client.ConnectCustomDomain(ctx, setup.accountID, setup.bucketName, setup.hostName, zoneID); err != nil {
			return err
		}
	}
	rule := statusPageRewriteRule(setup.hostName)
	ruleset, found, err := client.TransformRuleset(ctx, zoneID)
	if err != nil {
		return err
	}
	if !found {
		return client.CreateTransformRuleset(ctx, zoneID, rule)
	}
	for _, existing := range ruleset.Rules {
		if existing.Ref != rule.Ref {
			continue
		}
		if existing.Expression == rule.Expression && existing.Action == rule.Action {
			return nil
		}
		rule.ID = existing.ID
		return client.UpdateRule(ctx, zoneID, ruleset.ID, rule)
	}
	return client.AddRule(ctx, zoneID, ruleset.ID, rule)
}

// StatusPage returns the set-up and, when MMHotline can read it, the
// published status.json.
func (a *App) StatusPage(ctx context.Context, p Principal) (StatusPageView, error) {
	var setup statusPageSetup
	var found bool
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var err error
		setup, found, err = readStatusPageSetup(ctx, tx)
		return err
	})
	if err != nil || !found {
		return StatusPageView{}, err
	}
	view := setup.view()
	file, err := statuspage.Read(ctx, a.statusPageBucket(setup))
	if err != nil {
		view.ReadError = err.Error()
		return view, nil
	}
	view.PublishedStatusJSON = &file
	return view, a.markResolvedIncidentNotices(ctx, file)
}

// PostToStatusPage opens a new notice. Any member may post; nothing is
// published without a person's action, because an alert can be a false
// alarm and the team decides what its customers read.
func (a *App) PostToStatusPage(ctx context.Context, p Principal, in statuspage.PostNoticeInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.PostNotice(in))
}

// PostStatusPageUpdate adds an update to an open notice.
func (a *App) PostStatusPageUpdate(ctx context.Context, p Principal, in statuspage.PostUpdateInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.PostUpdate(in))
}

// EditStatusPageNotice changes a notice's title, impact, components, or
// planned window. Any member may.
func (a *App) EditStatusPageNotice(ctx context.Context, p Principal, in statuspage.EditNoticeInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.EditNotice(in))
}

// EditStatusPageUpdate changes an update's message or stage.
func (a *App) EditStatusPageUpdate(ctx context.Context, p Principal, in statuspage.EditUpdateInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.EditUpdate(in))
}

// DeleteStatusPageUpdate removes one update of a notice that keeps another.
func (a *App) DeleteStatusPageUpdate(ctx context.Context, p Principal, in statuspage.DeleteUpdateInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.DeleteUpdate(in))
}

// DeleteStatusPageNotice removes a notice and its updates from the page.
func (a *App) DeleteStatusPageNotice(ctx context.Context, p Principal, in statuspage.DeleteNoticeInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.DeleteNotice(in))
}

// PostIncidentToStatusPage opens an outage notice with the generic title and
// message for an incident, unless the notice that incident posted is still open. The row
// is reserved before publishing, so two quick presses post once.
func (a *App) PostIncidentToStatusPage(ctx context.Context, p Principal, incidentID string) (StatusPageView, int64, error) {
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var found bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM incidents WHERE id=?)`, incidentID).Scan(&found); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		var noticeID int64
		err := tx.QueryRowContext(ctx, `SELECT notice_id FROM status_page_incident_notices WHERE incident_id=? AND resolved=0`, incidentID).Scan(&noticeID)
		switch {
		case err == nil && noticeID == 0:
			return fmt.Errorf("%w: a notice from this incident is being posted now", ErrConflict)
		case err == nil:
			return fmt.Errorf("%w: notice %d, posted from this incident, is still open; update or resolve it on the Status page", ErrConflict, noticeID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO status_page_incident_notices(incident_id,notice_id,resolved) VALUES(?,0,0)`, incidentID)
		return err
	})
	if err != nil {
		return StatusPageView{}, 0, err
	}
	// The reservation is committed. Publishing and recording its outcome finish
	// even when the browser disconnects, or notice_id 0 would block the
	// incident's button for good. The minute bounds a stuck R2 request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	view, noticeID, err := a.publishStatusPage(ctx, p, statuspage.PostNotice(statuspage.PostNoticeInput{Title: StatusPageGenericTitle, Impact: statuspage.ImpactOutage, Stage: statuspage.StageInvestigating, Message: StatusPageGenericMessage}))
	record := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		// Free the button only when no notice was published. A notice that went
		// out stays linked even when a step after the publish failed.
		if noticeID == 0 {
			_, e := tx.ExecContext(ctx, `DELETE FROM status_page_incident_notices WHERE incident_id=? AND notice_id=0`, incidentID)
			return e
		}
		_, e := tx.ExecContext(ctx, `UPDATE status_page_incident_notices SET notice_id=? WHERE incident_id=?`, noticeID, incidentID)
		return e
	})
	if err == nil {
		err = record
	}
	return view, noticeID, err
}

// markResolvedIncidentNotices records which incident notices the page no
// longer shows as open, such as one resolved or deleted from a laptop,
// whenever MMHotline reads or publishes the page. A notice ID at or above the
// file's next notice ID is not marked: a read that began before another
// responder's post saved that notice does not know it yet, and must not free
// its incident's button. A reservation that is still being published
// (notice_id 0) is left alone for the same reason.
func (a *App) markResolvedIncidentNotices(ctx context.Context, file statuspage.File) error {
	open := map[int64]bool{}
	for _, notice := range file.OpenNotices() {
		open[notice.NoticeID] = true
	}
	nextNoticeID := max(file.NextNoticeID, 1)
	for _, notice := range file.Notices {
		nextNoticeID = max(nextNoticeID, notice.NoticeID+1)
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT notice_id FROM status_page_incident_notices WHERE resolved=0 AND notice_id!=0`)
		if err != nil {
			return err
		}
		var done []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !open[id] && id < nextNoticeID {
				done = append(done, id)
			}
		}
		rows.Close()
		for _, id := range done {
			if _, err := tx.ExecContext(ctx, `UPDATE status_page_incident_notices SET resolved=1 WHERE notice_id=?`, id); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// incidentOpenStatusPageNotice is the open notice an incident posted, or 0.
func incidentOpenStatusPageNotice(ctx context.Context, tx *sql.Tx, incidentID string) (int64, error) {
	var noticeID int64
	err := tx.QueryRowContext(ctx, `SELECT notice_id FROM status_page_incident_notices WHERE incident_id=? AND resolved=0 AND notice_id!=0`, incidentID).Scan(&noticeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return noticeID, err
}

// ResolveStatusPageNotice resolves a notice, with an optional closing message.
func (a *App) ResolveStatusPageNotice(ctx context.Context, p Principal, in statuspage.ResolveInput) (StatusPageView, int64, error) {
	return a.publishStatusPage(ctx, p, statuspage.Resolve(in))
}

func (a *App) publishStatusPage(ctx context.Context, p Principal, change statuspage.Change) (StatusPageView, int64, error) {
	var setup statusPageSetup
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var found bool
		var err error
		setup, found, err = readStatusPageSetup(ctx, tx)
		if err == nil && !found {
			err = fmt.Errorf("%w: the status page is not set up; an administrator adds the status_page section to the settings file %s and applies it", ErrConflict, a.settingsFile.path)
		}
		return err
	})
	if err != nil {
		return StatusPageView{}, 0, err
	}
	// The R2 requests run outside any transaction; the page itself is the record.
	// An ordinary post never creates a missing status.json, which would lack
	// the host name, links, and components the settings file sets; applying
	// the settings file does.
	file, noticeID, err := statuspage.Publish(ctx, a.statusPageBucket(setup), change, "", a.Now())
	if errors.Is(err, statuspage.ErrConflict) {
		return StatusPageView{}, 0, fmt.Errorf("%w: %s", ErrConflict, strings.TrimPrefix(err.Error(), statuspage.ErrConflict.Error()+": "))
	}
	if errors.Is(err, statuspage.ErrRefused) {
		return StatusPageView{}, 0, fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), statuspage.ErrRefused.Error()+": "))
	}
	if err != nil && file.PageTitle == "" {
		return StatusPageView{}, 0, fmt.Errorf("%w: %s", ErrConflict, err)
	}
	view := setup.view()
	view.PublishedStatusJSON = &file
	if err != nil {
		view.ReadError = err.Error()
		return view, noticeID, nil
	}
	return view, noticeID, a.markResolvedIncidentNotices(ctx, file)
}

// Text is what the command prints about the status page.
func (v StatusPageView) Text() string {
	if !v.IsSetUp && v.PublishedStatusJSON == nil {
		return "The status page is not set up."
	}
	var lines []string
	if v.PageURL != "" {
		lines = append(lines, "Status page: "+v.PageURL, "R2 bucket: "+v.BucketName+" ("+v.R2BucketURL+")")
	} else if v.R2BucketURL != "" {
		lines = append(lines, "Status page in R2 bucket "+v.R2BucketURL)
	}
	if v.SetUpBy != "" && v.SetUpAt != nil {
		lines = append(lines, fmt.Sprintf("Set up by %s at %s.", v.SetUpBy, commandTime(*v.SetUpAt)))
	}
	if v.ReadError != "" {
		lines = append(lines, "Problem: "+v.ReadError)
	}
	if f := v.PublishedStatusJSON; f != nil {
		lines = append(lines, fmt.Sprintf("%s, published at %s.", f.PageTitle, commandTime(f.PublishedAt)))
		if f.WebsiteURL != "" {
			lines = append(lines, "Website link: "+f.WebsiteURL)
		}
		if f.SupportURL != "" {
			lines = append(lines, "Support link: "+f.SupportURL)
		}
		for _, c := range f.ComponentStatuses() {
			lines = append(lines, fmt.Sprintf("Component %s (%s): %s", c.Component.Name, c.Component.ComponentID, c.Status))
		}
		open := f.OpenNotices()
		if len(open) == 0 {
			lines = append(lines, "No open notices: the page says that everything is working.")
		}
		for _, notice := range open {
			lines = append(lines, openNoticeText(*f, notice))
		}
		for _, notice := range f.ResolvedNotices(3) {
			lines = append(lines, fmt.Sprintf("Resolved notice %d, %s, resolved at %s: %s%s", notice.NoticeID, notice.Impact, commandTime(*notice.ResolvedAt), notice.Title, affectedComponentsText(*f, notice, " (affects ", ")")))
		}
	}
	return strings.Join(lines, "\n")
}

// affectedComponentsText is the names of the components a notice affects
// between before and after, empty when it names none.
func affectedComponentsText(file statuspage.File, notice statuspage.Notice, before, after string) string {
	if len(notice.AffectedComponentIDs) == 0 {
		return ""
	}
	names := make([]string, len(notice.AffectedComponentIDs))
	for index, id := range notice.AffectedComponentIDs {
		names[index] = file.ComponentName(id)
	}
	return before + strings.Join(names, ", ") + after
}

// openNoticeText is an open notice for the command: its title, impact, and
// the components it affects, its planned window, then each update with its ID
// and stage. A notice without updates, which only a file edited by hand
// holds, shows none.
func openNoticeText(file statuspage.File, notice statuspage.Notice) string {
	lines := []string{fmt.Sprintf("Open notice %d, %s: %s", notice.NoticeID, notice.Impact, notice.Title)}
	if affects := affectedComponentsText(file, notice, "Affects ", "."); affects != "" {
		lines = append(lines, "  "+affects)
	}
	opened := "Opened at " + commandTime(notice.OpenedAt) + "."
	if notice.ScheduledStartAt != nil {
		opened += " Planned start " + commandTime(*notice.ScheduledStartAt) + "."
	}
	if notice.ScheduledEndAt != nil {
		opened += " Planned end " + commandTime(*notice.ScheduledEndAt) + "."
	}
	lines = append(lines, "  "+opened)
	for _, update := range notice.Updates {
		stage := ""
		if update.Stage != "" {
			stage = ", " + string(update.Stage)
		}
		lines = append(lines, fmt.Sprintf("  Update %d%s, at %s: %s", update.UpdateID, stage, commandTime(update.PostedAt), update.Message))
	}
	return strings.Join(lines, "\n")
}
