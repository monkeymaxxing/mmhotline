package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"mmhotline/internal/cloudflareapi"
	"mmhotline/internal/downalarm"
)

const downAlarmSchema = `
CREATE TABLE mmhotline_down_alarm_setup (
 id INTEGER PRIMARY KEY CHECK(id=1),
 cloudflare_account_id TEXT NOT NULL,
 cloudflare_api_token TEXT NOT NULL,
 worker_name TEXT NOT NULL,
 worker_url TEXT NOT NULL,
 check_in_token TEXT NOT NULL,
 uploaded_worker_sha256 TEXT NOT NULL,
 last_handled_send_id INTEGER NOT NULL,
 check_ins_failing_alert_sent INTEGER NOT NULL,
 set_up_at INTEGER NOT NULL,
 set_up_by TEXT NOT NULL
);
`

// downAlarmSendsNotSeenYet is last_handled_send_id after a set-up that found
// no earlier one. The Worker keeps its sends for 7 days, so it may still list
// the sends of an earlier set-up, such as before a new check-in token or a
// new database, which nobody needs announced again. The first accepted
// check-in records every send it lists as handled, with no all-clear.
const downAlarmSendsNotSeenYet = -1

// The settings file's mmhotline_down_alarm wake times: how long after the last accepted
// check-in the MMHotline-down alarm wakes the person on call, and the backup. A team
// chooses by its risk appetite: a bank may want 5 minutes, a side project
// may accept 30.
const (
	// MMHotline checks in every minute and at once when it starts, so a
	// restart adds under a second and a server reboot leaves at most about
	// 2.5 minutes without a check-in (measured 2026-11-11). Below 3 minutes,
	// a reboot would wake someone.
	downAlarmWakeMinimumMinutes = 3
	// The backup waits at least 5 minutes more than the person on call, so
	// the person on call has time to answer before a second person is woken.
	downAlarmBackupWakeMinimumGapMinutes = 5
	// A longer wait would leave a stopped MMHotline, and every alert it
	// should send, unnoticed for over an hour.
	downAlarmWakeMaximumMinutes = 60
)

// downAlarmWakeTimesBeforeTheSetting are the wake times before the settings
// file has applied any, the recommended start: 5 minutes never wakes anyone
// for a restart or a reboot, and the backup's 10 gives the person on call 5
// minutes to answer first.
var downAlarmWakeTimesBeforeTheSetting = DownAlarmWakeTimes{WakeThePersonOnCallAfterMinutes: 5, WakeTheBackupAfterMinutes: 10}

const (
	// downAlarmCheckInInterval is how often MMHotline checks in when nothing
	// changed: 1,440 Worker requests a day, under 2% of the free plan.
	downAlarmCheckInInterval = time.Minute
	// downAlarmRoundInterval is how soon a change of who is on call, or of
	// their contact methods, reaches the MMHotline-down alarm.
	downAlarmRoundInterval = 5 * time.Second
	// After 10 minutes of failed check-ins, the team gets a warn alert:
	// without check-ins, the MMHotline-down alarm sends its held messages once, as for
	// an outage, and then nothing more if MMHotline itself goes down.
	downAlarmFailingAlertAfter = 10 * time.Minute
	// MMHotline stops checking in when its background loop, which sends
	// alerts, has not finished a round in 2 minutes. A round normally ends
	// every second; even a visit whose every sending service call waits for
	// its time limit (at most 30 seconds for an email) ends well within 2
	// minutes. It is well below the shortest wake time, 5 minutes: with a
	// check-in every minute, the MMHotline-down alarm wakes the person on call at most
	// 3 minutes plus the wake time after the loop stops.
	downAlarmBackgroundStaleAfter = 2 * time.Minute
	// An upgraded MMHotline tries to upload its new Worker at most once an hour.
	downAlarmUploadRetryInterval = time.Hour
	// After a day of failed uploads, the administrators get a warn alert: the
	// older Worker keeps working, so a day of Cloudflare trouble passes
	// quietly, but a stopped API token needs a person.
	downAlarmUploadFailingAlertAfter = 24 * time.Hour
)

// DownAlarmSetUpInput is the settings file's mmhotline_down_alarm section, as the
// MMHotline-down alarm part applies it.
type DownAlarmSetUpInput struct {
	CloudflareAccountID string `json:"cloudflare_account_id"`
	CloudflareAPIToken  string `json:"cloudflare_api_token"`
}

// DownAlarmHeldMessageView says who a held message reaches, and when, in
// words; it never contains the request.
type DownAlarmHeldMessageView struct {
	PersonName        string `json:"person_name"`
	ContactMethodType string `json:"contact_method_type"`
	SentTo            string `json:"sent_to"`
	DelayMinutes      int64  `json:"delay_minutes"`
}

// Set-up tries the first check-in up to 30 times, a second apart: a new
// workers.dev address took 12 seconds to go live in the real proof.
const downAlarmFirstCheckInTries = 30

var downAlarmFirstCheckInPause = time.Second

// DownAlarmSendView is one send the MMHotline-down alarm reported.
type DownAlarmSendView struct {
	SendID            int64     `json:"send_id,omitempty"`
	PersonName        string    `json:"person_name"`
	ContactMethodType string    `json:"contact_method_type"`
	DueAt             time.Time `json:"due_at,omitzero"`
	SendOutcome       string    `json:"send_outcome"`
	HTTPStatusCode    *int      `json:"http_status_code,omitempty"`
	// The three fields below are set only for a test of a Pushover
	// notification that the service accepted. PushoverReceipt is the receipt
	// in Pushover's answer. PushoverReceiptOutcome is
	// accepted_and_repeats_cancelled, no_receipt, receipt_refused_by_pushover,
	// or cancel_failed. PushoverReceiptProblem is the reason of a failure.
	PushoverReceipt        string `json:"pushover_receipt,omitempty"`
	PushoverReceiptOutcome string `json:"pushover_receipt_outcome,omitempty"`
	PushoverReceiptProblem string `json:"pushover_receipt_problem,omitempty"`
}

// DownAlarmView is what the dashboard and `mmhotline down-alarm show` show. It
// never contains the API token or the check-in token.
type DownAlarmView struct {
	IsSetUp               bool                       `json:"is_set_up"`
	CloudflareAccountID   string                     `json:"cloudflare_account_id,omitempty"`
	WorkerName            string                     `json:"worker_name,omitempty"`
	WorkerURL             string                     `json:"worker_url,omitempty"`
	SetUpAt               *time.Time                 `json:"set_up_at,omitempty"`
	SetUpBy               string                     `json:"set_up_by,omitempty"`
	LastCheckInAcceptedAt *time.Time                 `json:"last_check_in_accepted_at,omitempty"`
	CheckInsFailingSince  *time.Time                 `json:"check_ins_failing_since,omitempty"`
	LastCheckInError      string                     `json:"last_check_in_error,omitempty"`
	HeldMessages          []DownAlarmHeldMessageView `json:"held_messages,omitempty"`
	RecentSends           []DownAlarmSendView        `json:"recent_sends,omitempty"`
	TestSends             []DownAlarmSendView        `json:"test_sends,omitempty"`
}

type downAlarmSetup struct {
	accountID, apiToken, workerName, workerURL, checkInToken, uploadedWorkerSHA256 string
	lastHandledSendID                                                              int64
	failingAlertSent                                                               bool
	setUpAt                                                                        time.Time
	setUpBy                                                                        string
}

// downAlarmRuntime is the check-in loop's memory. Times of accepted
// check-ins stay out of the database: writing one every minute would make
// continuous backup upload a change forever.
type downAlarmRuntime struct {
	// checkingIn makes rounds and removal take turns, so a round that read
	// the set-up before a removal can never check in after it.
	checkingIn          sync.Mutex
	mu                  sync.Mutex
	wake                chan struct{}
	lastAttemptAt       time.Time
	lastAttemptedDigest string
	lastAcceptedAt      time.Time
	failingSince        time.Time
	lastError           string
	lastSends           []downalarm.Send
	lastUploadAttemptAt time.Time
	// uploadFailingSince is when the upload of an upgraded Worker first
	// failed, and uploadFailingAlertSent whether administrators were told.
	uploadFailingSince     time.Time
	uploadFailingAlertSent bool
	// unreachedNoticed is the list of people the MMHotline-down alarm cannot wake that
	// the administrators were last told about, or empty.
	unreachedNoticed string
}

func newDownAlarmRuntime() *downAlarmRuntime { return &downAlarmRuntime{wake: make(chan struct{}, 1)} }

// downAlarmHeld is one held message with what MMHotline knows about it.
type downAlarmHeld struct {
	message                                        downalarm.HeldMessage
	contactMethodID, personName, contactMethodType string
	sentTo                                         string
	delay                                          time.Duration
}

// downAlarmMessageBuilder is a contact method type that can wake a person
// without MMHotline: the MMHotline-down alarm sends its ready-made HTTPS request. Only
// contact method types that wake sleeping people build one.
type downAlarmMessageBuilder interface {
	downAlarmRequests(c ContactMethod, text downAlarmText) (request, testRequest downalarm.HeldRequest, err error)
}

// downAlarmText is what a held message says. It names no time, so the held
// message stays the same from one check-in to the next.
type downAlarmText struct{ TeamName, WebURL string }

func (t downAlarmText) title() string { return "MMHotline is not checking in" }
func (t downAlarmText) message() string {
	return "MMHotline for " + t.TeamName + " stopped checking in with the MMHotline-down alarm. MMHotline may be down or cut off from the internet, so MMHotline cannot send urgent alerts. Check the MMHotline server."
}
func (t downAlarmText) testTitle() string { return "Test of the MMHotline-down alarm" }
func (t downAlarmText) testMessage() string {
	return "This is a test of the MMHotline-down alarm for " + t.TeamName + ", which wakes you if MMHotline stops. When MMHotline stops checking in with the MMHotline-down alarm, the MMHotline-down alarm sends you an urgent alert like this test alert. Nothing is wrong."
}

// heldRequest copies a built request into a held request.
func heldRequest(request *http.Request, err error) (downalarm.HeldRequest, error) {
	if err != nil {
		return downalarm.HeldRequest{}, err
	}
	body := ""
	if request.Body != nil {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return downalarm.HeldRequest{}, err
		}
		body = string(data)
	}
	headers := map[string]string{}
	for name, values := range request.Header {
		headers[name] = strings.Join(values, ", ")
	}
	return downalarm.HeldRequest{Method: request.Method, URL: request.URL.String(), Headers: headers, Body: body}, nil
}

var downAlarmHeldMessageIDPattern = regexp.MustCompile(`^(primary|backup)-contact-method-([a-f0-9]{32})$`)

func downAlarmHeldMessageID(step, contactMethodID string) string {
	return step + "-contact-method-" + contactMethodID
}

// downAlarmWorkerName names the Worker after the host name of MMHotline's web
// address, so a second team's MMHotline in the same Cloudflare account gets
// its own Worker, and a set-up again after a lost database replaces the same
// Worker instead of leaving one behind that would wake people.
func downAlarmWorkerName(webURL string) string {
	host := "mmhotline"
	if u, err := url.Parse(webURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return cloudflareName("mmhotline-down-alarm", host)
}

func readDownAlarmSetup(ctx context.Context, tx *sql.Tx) (downAlarmSetup, bool, error) {
	var s downAlarmSetup
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT cloudflare_account_id,cloudflare_api_token,worker_name,worker_url,check_in_token,uploaded_worker_sha256,last_handled_send_id,check_ins_failing_alert_sent,set_up_at,set_up_by FROM mmhotline_down_alarm_setup WHERE id=1`).Scan(&s.accountID, &s.apiToken, &s.workerName, &s.workerURL, &s.checkInToken, &s.uploadedWorkerSHA256, &s.lastHandledSendID, &s.failingAlertSent, &at, &s.setUpBy)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	s.setUpAt = time.Unix(at, 0).UTC()
	return s, err == nil, err
}

func (a *App) downAlarmClient(s downAlarmSetup) *downalarm.Client {
	return &downalarm.Client{WorkerURL: s.workerURL, CheckInToken: s.checkInToken}
}

// downAlarmHeldMessages builds the held messages for whoever is on call now:
// the primary's waking contact methods, and a named backup's, after the
// settings file's mmhotline_down_alarm wake times. The full escalation stays MMHotline's own work while it
// runs. During team mute, the delays start when the mute ends, so the
// MMHotline-down alarm wakes nobody during a mute but still does after it. unreached
// names each person in those steps whom no held message would wake, with
// the reason, for the administrator notice.
func (a *App) downAlarmHeldMessages(ctx context.Context, tx *sql.Tx, now time.Time) (held []downAlarmHeld, unreached []string, err error) {
	coverage, err := CoverageAt(ctx, tx, now)
	if err != nil {
		return nil, nil, err
	}
	var team string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM team_info`).Scan(&team); err != nil {
		return nil, nil, err
	}
	wake := downAlarmWakeTimesBeforeTheSetting
	if _, err := appliedSettingsValue(ctx, tx, settingsDownAlarmWakeTimesPart.name, &wake); err != nil {
		return nil, nil, err
	}
	primary := coverage.PrimaryIDs
	if len(primary) == 0 {
		// Nobody is available: an SOS contacts every member, and so does the MMHotline-down alarm.
		primary = coverage.AllMemberIDs
	}
	// named is false for people reached only as everyone available, who
	// raise no notice when they cannot be woken, as with an SOS.
	steps := []struct {
		name, role string
		ids        []string
		delay      time.Duration
		named      bool
	}{{"primary", "on call", primary, time.Duration(wake.WakeThePersonOnCallAfterMinutes) * time.Minute, true}}
	if !coverage.PrimaryIsEveryoneAvailable {
		// The backup step escalates as an unclaimed SOS does: to the named
		// backup, or to everyone else available.
		steps = append(steps, struct {
			name, role string
			ids        []string
			delay      time.Duration
			named      bool
		}{"backup", "the backup", coverage.BackupIDs, time.Duration(wake.WakeTheBackupAfterMinutes) * time.Minute, !coverage.BackupIsEveryoneAvailable})
	}
	var muted time.Duration
	if coverage.TeamMute.Active {
		// Whole minutes, so the held messages change at most once a minute.
		muted = coverage.TeamMute.Until.Sub(now).Truncate(time.Minute) + time.Minute
	}
	text := downAlarmText{TeamName: team, WebURL: a.WebURL}
	for _, step := range steps {
		for _, memberID := range step.ids {
			var name string
			if err := tx.QueryRowContext(ctx, `SELECT name FROM members WHERE id=?`, memberID).Scan(&name); err != nil {
				return nil, nil, err
			}
			rows, err := tx.QueryContext(ctx, `SELECT c.`+strings.ReplaceAll(contactMethodColumns, ",", ",c.")+` FROM contact_methods c JOIN contact_method_subscriptions s ON s.contact_method_id=c.id WHERE c.deleted=0 AND c.member_id=? AND s.severity='sos' ORDER BY c.id`, memberID)
			if err != nil {
				return nil, nil, err
			}
			var methods []ContactMethod
			for rows.Next() {
				method, err := scanContactMethod(rows)
				if err != nil {
					rows.Close()
					return nil, nil, err
				}
				methods = append(methods, method)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, nil, err
			}
			heldBefore := len(held)
			var unbuilt []string
			for _, method := range methods {
				contactMethodType, ok := a.configuredContactMethodTypes()[method.ContactMethodType]
				builder, builds := contactMethodType.(downAlarmMessageBuilder)
				if !ok || !builds {
					continue
				}
				facts, _ := contactMethodTypeFacts(method.ContactMethodType)
				request, testRequest, err := builder.downAlarmRequests(method, text)
				if err != nil {
					unbuilt = append(unbuilt, fmt.Sprintf("%s to %s: %s", facts.CardTitle, method.SentTo, err))
					continue
				}
				delay := step.delay + muted
				held = append(held, downAlarmHeld{
					message:         downalarm.HeldMessage{HeldMessageID: downAlarmHeldMessageID(step.name, method.ID), DelaySeconds: int64(delay / time.Second), Request: request, TestRequest: testRequest},
					contactMethodID: method.ID, personName: name, contactMethodType: facts.CardTitle, sentTo: method.SentTo, delay: delay,
				})
			}
			if len(held) == heldBefore && step.named {
				reason := "no Pushover notification or automated voice call subscribed to SOS"
				if len(unbuilt) > 0 {
					reason = strings.Join(unbuilt, "; ")
				}
				unreached = append(unreached, fmt.Sprintf("- %s, %s: %s", name, step.role, reason))
			}
		}
	}
	return held, unreached, nil
}

func heldMessagesOf(held []downAlarmHeld) []downalarm.HeldMessage {
	messages := []downalarm.HeldMessage{}
	for _, h := range held {
		messages = append(messages, h.message)
	}
	return messages
}

func heldDigest(held []downAlarmHeld) string {
	data, _ := json.Marshal(heldMessagesOf(held))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newCheckInToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// uploadDownAlarmWorker deploys this build's Worker. A Worker already at the
// migration tag gets no migration again.
func uploadDownAlarmWorker(ctx context.Context, client *cloudflareapi.Client, accountID, workerName string) error {
	exists, tag, err := client.WorkerMigrationTag(ctx, accountID, workerName)
	if err != nil {
		return err
	}
	if !exists {
		tag = ""
	}
	return client.UploadWorker(ctx, accountID, workerName, cloudflareapi.WorkerUpload{ModuleName: downalarm.WorkerModuleName, ModuleSource: downalarm.WorkerSource(), Metadata: downalarm.WorkerMetadata(tag)})
}

// SetUpDownAlarm uploads the Worker, sets the check-in token as its secret,
// turns on its workers.dev address, saves the set-up with the API token,
// and checks in at once. Running it again for the same account keeps the
// Worker's name and check-in token, so a failed rerun never locks out the
// running MMHotline.
func (a *App) SetUpDownAlarm(ctx context.Context, p Principal, in DownAlarmSetUpInput) (DownAlarmView, error) {
	accountID, err := checkCloudflareAccountID(in.CloudflareAccountID)
	if err != nil {
		return DownAlarmView{}, err
	}
	token, err := checkCloudflareAPIToken(in.CloudflareAPIToken)
	if err != nil {
		return DownAlarmView{}, err
	}
	var previous downAlarmSetup
	var found bool
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		previous, found, err = readDownAlarmSetup(ctx, tx)
		return err
	})
	if err != nil {
		return DownAlarmView{}, err
	}
	setup := downAlarmSetup{accountID: accountID, apiToken: token, workerName: downAlarmWorkerName(a.WebURL), checkInToken: newCheckInToken(), uploadedWorkerSHA256: downalarm.WorkerDigest()}
	if found && previous.accountID != accountID {
		return DownAlarmView{}, fmt.Errorf("%w: the MMHotline-down alarm is set up in Cloudflare account %s. To move the MMHotline-down alarm, first apply the settings file without the mmhotline_down_alarm section, so the MMHotline-down alarm's Worker cancels all held messages and MMHotline removes the setup. Then add the mmhotline_down_alarm section with the new account and apply the settings file again", ErrConflict, previous.accountID)
	}
	if found {
		setup.workerName, setup.checkInToken, setup.lastHandledSendID = previous.workerName, previous.checkInToken, previous.lastHandledSendID
	} else {
		setup.lastHandledSendID = downAlarmSendsNotSeenYet
	}
	client := &cloudflareapi.Client{APIBaseURL: a.Cloudflare.APIBaseURL, APIToken: token}
	subdomain, err := client.WorkersDevSubdomain(ctx, accountID)
	if err != nil {
		return DownAlarmView{}, fmt.Errorf("%w: %s. Open Workers & Pages in the Cloudflare dashboard once to choose the account's workers.dev subdomain, then apply the settings file again", ErrInvalid, err)
	}
	for _, step := range []func() error{
		func() error { return uploadDownAlarmWorker(ctx, client, accountID, setup.workerName) },
		func() error {
			return client.SetWorkerSecret(ctx, accountID, setup.workerName, downalarm.CheckInTokenSecretName, setup.checkInToken)
		},
		func() error { return client.EnableWorkersDev(ctx, accountID, setup.workerName) },
	} {
		if err := step(); err != nil {
			return DownAlarmView{}, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	}
	setup.workerURL = a.Cloudflare.WorkerURL(setup.workerName, subdomain)
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		actor, err := RequireMember(ctx, tx, p, true)
		if err != nil {
			return err
		}
		setup.setUpAt, setup.setUpBy = a.Now().UTC().Truncate(time.Second), actor.Name
		if found {
			setup.failingAlertSent = previous.failingAlertSent
		}
		return saveDownAlarmSetup(ctx, tx, setup)
	})
	if err != nil {
		return DownAlarmView{}, err
	}
	// Set-up uploaded this build's Worker, so an earlier failing upload is over.
	a.downAlarm.mu.Lock()
	a.downAlarm.uploadFailingSince, a.downAlarm.uploadFailingAlertSent = time.Time{}, false
	a.downAlarm.mu.Unlock()
	// A new workers.dev address answers with Cloudflare's own 404 until the
	// Worker is live there, 12 seconds after set-up in the real proof
	// (2026-10-27), so set-up tries the first check-in again for a while
	// before it reports a failure.
	var view DownAlarmView
	for tries := 1; ; tries++ {
		a.downAlarm.mu.Lock()
		a.downAlarm.lastAttemptAt, a.downAlarm.lastAttemptedDigest = time.Time{}, ""
		a.downAlarm.mu.Unlock()
		a.downAlarmRound(ctx, func(error) {})
		view, err = a.DownAlarm(ctx, p)
		if err != nil || view.LastCheckInError == "" || tries == downAlarmFirstCheckInTries {
			break
		}
		select {
		case <-ctx.Done():
			return view, ctx.Err()
		case <-time.After(downAlarmFirstCheckInPause):
		}
	}
	if err == nil && view.LastCheckInError != "" {
		// A set-up whose first check-in fails is not applied: MMHotline puts
		// back the setup it had before, or none, so the earlier setting stays
		// and a later apply tries again.
		failure := fmt.Errorf("%w: the MMHotline-down alarm's Worker was uploaded, but MMHotline's first check-in with the MMHotline-down alarm failed for %d seconds: %s", ErrConflict, downAlarmFirstCheckInTries, strings.TrimSuffix(view.LastCheckInError, "."))
		a.downAlarm.checkingIn.Lock()
		defer a.downAlarm.checkingIn.Unlock()
		err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			if !found {
				_, err := tx.ExecContext(ctx, `DELETE FROM mmhotline_down_alarm_setup WHERE id=1`)
				return err
			}
			return saveDownAlarmSetup(ctx, tx, previous)
		})
		a.downAlarm.mu.Lock()
		a.downAlarm.lastAttemptAt, a.downAlarm.lastAttemptedDigest, a.downAlarm.lastAcceptedAt = time.Time{}, "", time.Time{}
		a.downAlarm.failingSince, a.downAlarm.lastError, a.downAlarm.lastSends = time.Time{}, "", nil
		a.downAlarm.mu.Unlock()
		return view, errors.Join(failure, err)
	}
	return view, err
}

// saveDownAlarmSetup saves setup as the one MMHotline-down alarm setup.
func saveDownAlarmSetup(ctx context.Context, tx *sql.Tx, setup downAlarmSetup) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mmhotline_down_alarm_setup(id,cloudflare_account_id,cloudflare_api_token,worker_name,worker_url,check_in_token,uploaded_worker_sha256,last_handled_send_id,check_ins_failing_alert_sent,set_up_at,set_up_by) VALUES(1,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET cloudflare_account_id=excluded.cloudflare_account_id,cloudflare_api_token=excluded.cloudflare_api_token,worker_name=excluded.worker_name,worker_url=excluded.worker_url,check_in_token=excluded.check_in_token,uploaded_worker_sha256=excluded.uploaded_worker_sha256,last_handled_send_id=excluded.last_handled_send_id,check_ins_failing_alert_sent=excluded.check_ins_failing_alert_sent,set_up_at=excluded.set_up_at,set_up_by=excluded.set_up_by`,
		setup.accountID, setup.apiToken, setup.workerName, setup.workerURL, setup.checkInToken, setup.uploadedWorkerSHA256, setup.lastHandledSendID, setup.failingAlertSent, setup.setUpAt.Unix(), setup.setUpBy)
	return err
}

// RemoveDownAlarmSetup tells the MMHotline-down alarm to hold nothing, then deletes the
// set-up. Without the first step, the Worker would wake the person on call
// once check-ins stopped. A Worker deleted in Cloudflare holds nothing.
func (a *App) RemoveDownAlarmSetup(ctx context.Context, p Principal) error {
	a.downAlarm.checkingIn.Lock()
	defer a.downAlarm.checkingIn.Unlock()
	var setup downAlarmSetup
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		var found bool
		var err error
		setup, found, err = readDownAlarmSetup(ctx, tx)
		if err == nil && !found {
			err = fmt.Errorf("%w: the MMHotline-down alarm is not set up", ErrNotFound)
		}
		return err
	})
	if err != nil {
		return err
	}
	_, err = a.downAlarmClient(setup).CheckIn(ctx, nil)
	var refusal *downalarm.Refusal
	if err != nil && !(errors.As(err, &refusal) && refusal.HTTPStatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w: MMHotline could not tell the MMHotline-down alarm to cancel all held messages (%s), so removing the setup now would let the MMHotline-down alarm wake the person on call when its wait without a check-in ends. Apply the settings file again, or delete the MMHotline-down alarm's Worker %s in the Cloudflare dashboard and then apply the settings file again", ErrConflict, err, setup.workerName)
	}
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, true); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM mmhotline_down_alarm_setup WHERE id=1`)
		return err
	})
	if err == nil {
		a.downAlarm.mu.Lock()
		a.downAlarm.lastAttemptAt, a.downAlarm.lastAttemptedDigest, a.downAlarm.lastAcceptedAt = time.Time{}, "", time.Time{}
		a.downAlarm.failingSince, a.downAlarm.lastError, a.downAlarm.lastSends = time.Time{}, "", nil
		a.downAlarm.unreachedNoticed = ""
		a.downAlarm.mu.Unlock()
	}
	return err
}

// RunDownAlarm checks in with the MMHotline-down alarm every minute, and within 5
// seconds when the held messages change, such as at a handoff, while the
// MMHotline-down alarm is set up.
func (a *App) RunDownAlarm(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(downAlarmRoundInterval)
	defer ticker.Stop()
	for {
		reportPanic(report, "the MMHotline-down alarm round", func() { a.downAlarmRound(ctx, report) })
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.downAlarm.wake:
		}
	}
}

// downAlarmRound is one visit of the check-in loop.
func (a *App) downAlarmRound(ctx context.Context, report func(error)) {
	a.downAlarm.checkingIn.Lock()
	defer a.downAlarm.checkingIn.Unlock()
	now := a.Now().UTC()
	var setup downAlarmSetup
	var found bool
	var held []downAlarmHeld
	var unreached []string
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		if setup, found, err = readDownAlarmSetup(ctx, tx); err != nil || !found {
			return err
		}
		held, unreached, err = a.downAlarmHeldMessages(ctx, tx, now)
		return err
	})
	if err != nil {
		report(fmt.Errorf("MMHotline-down alarm: %w", err))
		return
	}
	if !found {
		return
	}
	a.noticeDownAlarmUnreached(ctx, unreached, now, report)
	if setup.uploadedWorkerSHA256 != downalarm.WorkerDigest() {
		a.upgradeDownAlarmWorker(ctx, setup, now, report)
	}
	digest := heldDigest(held)
	a.downAlarm.mu.Lock()
	due := now.Sub(a.downAlarm.lastAttemptAt) >= downAlarmCheckInInterval || digest != a.downAlarm.lastAttemptedDigest
	if due {
		a.downAlarm.lastAttemptAt, a.downAlarm.lastAttemptedDigest = now, digest
	}
	a.downAlarm.mu.Unlock()
	if !due {
		return
	}
	// A check-in says that MMHotline sends its alerts. While the background
	// loop, which sends them, fails or hangs, MMHotline stops checking in on
	// purpose, so the MMHotline-down alarm wakes the person on call.
	if upToDate := a.teamDeadline.backgroundUpToDateAt(now); now.Sub(upToDate) > downAlarmBackgroundStaleAfter {
		a.downAlarmCheckInFailed(ctx, setup, fmt.Errorf("MMHotline stopped checking in on purpose: its background work, which sends alerts, has not finished a round since %s, so the MMHotline-down alarm wakes the person on call", displayTime(upToDate)), now, report)
		return
	}
	answer, err := a.downAlarmClient(setup).CheckIn(ctx, heldMessagesOf(held))
	if err != nil {
		a.downAlarmCheckInFailed(ctx, setup, err, now, report)
		return
	}
	if err := a.downAlarmCheckInAccepted(ctx, setup, answer, now, report); err != nil {
		report(fmt.Errorf("MMHotline-down alarm: %w", err))
	}
}

// noticeDownAlarmUnreached tells the administrators, with one warn alert, whom
// the MMHotline-down alarm cannot wake, and again only when that list changes. The list
// told is kept in memory, so a restart may tell them once more.
func (a *App) noticeDownAlarmUnreached(ctx context.Context, unreached []string, now time.Time, report func(error)) {
	list := strings.Join(unreached, "\n")
	a.downAlarm.mu.Lock()
	told := a.downAlarm.unreachedNoticed == list
	a.downAlarm.mu.Unlock()
	if told {
		return
	}
	if list != "" {
		message := "If MMHotline stops checking in with the MMHotline-down alarm, the MMHotline-down alarm cannot wake these people:\n" + linesWithin(unreached, 8000) + "\n\nThe MMHotline-down alarm wakes people only through a Pushover notification or an automated voice call subscribed to SOS. Give each person above one that works. The dashboard's MMHotline-down alarm panel lists whom the MMHotline-down alarm would wake."
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return RecordAdministratorNotice(ctx, tx, "warn", "The MMHotline-down alarm cannot wake everyone on call", message, now)
		})
		if err != nil {
			report(fmt.Errorf("MMHotline-down alarm: %w", err))
			return
		}
	}
	a.downAlarm.mu.Lock()
	a.downAlarm.unreachedNoticed = list
	a.downAlarm.mu.Unlock()
}

// upgradeDownAlarmWorker uploads this build's Worker with the saved API
// token, after an upgrade of MMHotline.
func (a *App) upgradeDownAlarmWorker(ctx context.Context, setup downAlarmSetup, now time.Time, report func(error)) {
	a.downAlarm.mu.Lock()
	due := now.Sub(a.downAlarm.lastUploadAttemptAt) >= downAlarmUploadRetryInterval
	if due {
		a.downAlarm.lastUploadAttemptAt = now
	}
	a.downAlarm.mu.Unlock()
	if !due {
		return
	}
	client := &cloudflareapi.Client{APIBaseURL: a.Cloudflare.APIBaseURL, APIToken: setup.apiToken}
	if uploadErr := uploadDownAlarmWorker(ctx, client, setup.accountID, setup.workerName); uploadErr != nil {
		report(fmt.Errorf("MMHotline-down alarm: the upgraded Worker could not be uploaded; MMHotline tries again in an hour: %w", uploadErr))
		a.downAlarm.mu.Lock()
		if a.downAlarm.uploadFailingSince.IsZero() {
			a.downAlarm.uploadFailingSince = now
		}
		since := a.downAlarm.uploadFailingSince
		tell := !a.downAlarm.uploadFailingAlertSent && now.Sub(since) >= downAlarmUploadFailingAlertAfter
		a.downAlarm.mu.Unlock()
		if !tell {
			return
		}
		err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			return RecordAdministratorNotice(ctx, tx, "warn", "MMHotline cannot update the MMHotline-down alarm, which still runs an older version", fmt.Sprintf("Since %s, MMHotline could not upload the new version of the MMHotline-down alarm's Worker to Cloudflare: %s\n\nWhile MMHotline's check-ins are accepted, the older version still wakes the person on call if MMHotline goes down. Often the Cloudflare API token stopped working, for example because the person who made it was removed from the account. In the Cloudflare dashboard, under Manage Account, then Account API Tokens, create a token with the permission Account › Workers Scripts › Edit, put it in mmhotline_down_alarm.cloudflare_api_token in the settings file, and apply the file, with your deploy tool or with sudo mmhotline settings apply. MMHotline tries again every hour.", displayTime(since), uploadErr), now)
		})
		if err != nil {
			report(fmt.Errorf("MMHotline-down alarm: %w", err))
			return
		}
		a.downAlarm.mu.Lock()
		a.downAlarm.uploadFailingAlertSent = true
		a.downAlarm.mu.Unlock()
		return
	}
	a.downAlarm.mu.Lock()
	told := a.downAlarm.uploadFailingAlertSent
	a.downAlarm.uploadFailingSince, a.downAlarm.uploadFailingAlertSent = time.Time{}, false
	a.downAlarm.mu.Unlock()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE mmhotline_down_alarm_setup SET uploaded_worker_sha256=? WHERE id=1 AND worker_name=?`, downalarm.WorkerDigest(), setup.workerName); err != nil || !told {
			return err
		}
		return RecordAdministratorNotice(ctx, tx, "info", "MMHotline updated the MMHotline-down alarm", "MMHotline uploaded the new version of the MMHotline-down alarm's Worker to Cloudflare. The MMHotline-down alarm runs the same version as MMHotline again.", now)
	})
	if err != nil {
		report(fmt.Errorf("MMHotline-down alarm: %w", err))
	}
}

func (a *App) downAlarmCheckInFailed(ctx context.Context, setup downAlarmSetup, failure error, now time.Time, report func(error)) {
	a.downAlarm.mu.Lock()
	first := a.downAlarm.failingSince.IsZero()
	if first {
		a.downAlarm.failingSince = now
	}
	since := a.downAlarm.failingSince
	a.downAlarm.lastError = failure.Error()
	a.downAlarm.mu.Unlock()
	if first {
		report(fmt.Errorf("MMHotline-down alarm: the check-in failed; MMHotline tries again every minute: %w", failure))
	}
	if setup.failingAlertSent || now.Sub(since) < downAlarmFailingAlertAfter {
		return
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var sent bool
		if err := tx.QueryRowContext(ctx, `SELECT check_ins_failing_alert_sent FROM mmhotline_down_alarm_setup WHERE id=1`).Scan(&sent); err != nil || sent {
			return err
		}
		in := GenericAlert{Severity: "warn", Title: "MMHotline cannot reach the MMHotline-down alarm", Message: fmt.Sprintf("MMHotline's check-ins with the MMHotline-down alarm have failed since %s: %s\n\nWithout check-ins, the MMHotline-down alarm acts as if MMHotline were down: it sends its urgent alerts once, then nothing more, even if MMHotline really goes down later. On the MMHotline server, run: sudo mmhotline down-alarm show", displayTime(since), failure)}
		if _, err := admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, nil, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE mmhotline_down_alarm_setup SET check_ins_failing_alert_sent=1 WHERE id=1`)
		return err
	})
	if err != nil {
		report(fmt.Errorf("MMHotline-down alarm: %w", err))
	}
}

func (a *App) downAlarmCheckInAccepted(ctx context.Context, setup downAlarmSetup, answer downalarm.CheckInAnswer, now time.Time, report func(error)) error {
	a.downAlarm.mu.Lock()
	recovered := !a.downAlarm.failingSince.IsZero()
	// MMHotline's own clock, so "accepted 20 seconds ago" never depends on
	// how far Cloudflare's clock is from this server's.
	a.downAlarm.lastAcceptedAt, a.downAlarm.failingSince, a.downAlarm.lastError, a.downAlarm.lastSends = now, time.Time{}, "", answer.Sends
	a.downAlarm.mu.Unlock()
	if recovered {
		report(errors.New("MMHotline-down alarm: the check-ins work again"))
	}
	newSends := func(lastHandled int64) []downalarm.Send {
		var unhandled []downalarm.Send
		for _, send := range answer.Sends {
			if send.SendID > lastHandled {
				unhandled = append(unhandled, send)
			}
		}
		return unhandled
	}
	if setup.lastHandledSendID != downAlarmSendsNotSeenYet && len(newSends(setup.lastHandledSendID)) == 0 && !setup.failingAlertSent {
		return nil
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var lastHandled int64
		var failingAlertSent bool
		if err := tx.QueryRowContext(ctx, `SELECT last_handled_send_id,check_ins_failing_alert_sent FROM mmhotline_down_alarm_setup WHERE id=1`).Scan(&lastHandled, &failingAlertSent); err != nil {
			return err
		}
		if failingAlertSent {
			in := GenericAlert{Severity: "info", Title: "MMHotline reaches the MMHotline-down alarm again: if MMHotline goes down, the person on call is woken", Message: "MMHotline's check-ins with the MMHotline-down alarm work again. If MMHotline goes down, the MMHotline-down alarm wakes the person on call."}
			if _, err := admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, nil, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE mmhotline_down_alarm_setup SET check_ins_failing_alert_sent=0 WHERE id=1`); err != nil {
				return err
			}
		}
		if lastHandled == downAlarmSendsNotSeenYet {
			var highest int64
			for _, send := range answer.Sends {
				highest = max(highest, send.SendID)
			}
			_, err := tx.ExecContext(ctx, `UPDATE mmhotline_down_alarm_setup SET last_handled_send_id=? WHERE id=1`, highest)
			return err
		}
		unhandled := newSends(lastHandled)
		if len(unhandled) == 0 {
			return nil
		}
		return sendDownAlarmAllClear(ctx, tx, unhandled, now)
	})
}

// sendDownAlarmAllClear tells each person the MMHotline-down alarm reached that
// MMHotline runs again, through MMHotline's normal path, once for the sends
// MMHotline has not handled, and records them as handled in the same
// transaction. A lost answer loses nothing: the next check-in lists the
// same sends.
func sendDownAlarmAllClear(ctx context.Context, tx *sql.Tx, sends []downalarm.Send, now time.Time) error {
	sort.Slice(sends, func(i, j int) bool { return sends[i].SendID < sends[j].SendID })
	var lines []string
	var recipients []string
	var highest int64
	for _, send := range sends {
		highest = max(highest, send.SendID)
		view := downAlarmSendView(ctx, tx, send)
		lines = append(lines, fmt.Sprintf("- %s to %s, due %s: %s", view.ContactMethodType, view.PersonName, displayTime(send.DueAt), downAlarmOutcomeLabel(send.SendOutcome, send.HTTPStatusCode)))
		if memberID := downAlarmSendMemberID(ctx, tx, send); memberID != "" && !containsMember(recipients, memberID) {
			recipients = append(recipients, memberID)
		}
	}
	message := fmt.Sprintf("MMHotline stopped checking in with the MMHotline-down alarm at %s, so the MMHotline-down alarm sent these urgent alerts:\n%s\n\nMMHotline is running again. Look at the alerts received while MMHotline was not checking in.", displayTime(sends[0].SilenceStartedAt), strings.Join(lines, "\n"))
	in := GenericAlert{Severity: "info", Title: "MMHotline runs again: it was down, and the MMHotline-down alarm woke you", Message: message}
	if _, err := admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", in, recipients, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE mmhotline_down_alarm_setup SET last_handled_send_id=? WHERE id=1 AND last_handled_send_id<?`, highest, highest)
	return err
}

func downAlarmOutcomeLabel(outcome string, statusCode *int) string {
	label := map[string]string{"delivered": "accepted by the service", "refused_by_service": "refused by the service", "not_delivered_within_10_minutes": "not delivered within 10 minutes", "stopped_by_check_in": "stopped because MMHotline checked in again", "held_message_replaced": "stopped because the held message changed", "service_unavailable": "the service was unavailable", "no_answer": "no answer from the service"}[outcome]
	if label == "" {
		label = "being sent"
	}
	if statusCode != nil {
		label += fmt.Sprintf(" (HTTP %d)", *statusCode)
	}
	return label
}

func downAlarmSendMemberID(ctx context.Context, tx *sql.Tx, send downalarm.Send) string {
	match := downAlarmHeldMessageIDPattern.FindStringSubmatch(send.HeldMessageID)
	if match == nil {
		return ""
	}
	var memberID string
	_ = tx.QueryRowContext(ctx, `SELECT member_id FROM contact_methods WHERE id=?`, match[2]).Scan(&memberID)
	return memberID
}

// downAlarmSendView names the person and contact method of a send; a
// removed person or contact method keeps its recorded name.
func downAlarmSendView(ctx context.Context, tx *sql.Tx, send downalarm.Send) DownAlarmSendView {
	view := DownAlarmSendView{SendID: send.SendID, PersonName: "an unknown person", ContactMethodType: "Contact method", DueAt: send.DueAt, SendOutcome: send.SendOutcome, HTTPStatusCode: send.HTTPStatusCode}
	if match := downAlarmHeldMessageIDPattern.FindStringSubmatch(send.HeldMessageID); match != nil {
		var contactMethodType, name string
		if tx.QueryRowContext(ctx, `SELECT c.contact_method_type,m.name FROM contact_methods c JOIN members m ON m.id=c.member_id WHERE c.id=?`, match[2]).Scan(&contactMethodType, &name) == nil {
			view.PersonName = name
			if facts, ok := contactMethodTypeFacts(contactMethodType); ok {
				view.ContactMethodType = facts.CardTitle
			}
		}
	}
	return view
}

// MMHotline-down alarm returns what the dashboard and the command show.
func (a *App) DownAlarm(ctx context.Context, p Principal) (DownAlarmView, error) {
	var view DownAlarmView
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		setup, found, err := readDownAlarmSetup(ctx, tx)
		if err != nil || !found {
			return err
		}
		at := setup.setUpAt
		view = DownAlarmView{IsSetUp: true, CloudflareAccountID: setup.accountID, WorkerName: setup.workerName, WorkerURL: setup.workerURL, SetUpAt: &at, SetUpBy: setup.setUpBy}
		held, _, err := a.downAlarmHeldMessages(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, h := range held {
			view.HeldMessages = append(view.HeldMessages, DownAlarmHeldMessageView{PersonName: h.personName, ContactMethodType: h.contactMethodType, SentTo: h.sentTo, DelayMinutes: int64(h.delay / time.Minute)})
		}
		a.downAlarm.mu.Lock()
		accepted, failing, lastError, sends := a.downAlarm.lastAcceptedAt, a.downAlarm.failingSince, a.downAlarm.lastError, a.downAlarm.lastSends
		a.downAlarm.mu.Unlock()
		if !accepted.IsZero() {
			view.LastCheckInAcceptedAt = &accepted
		}
		if !failing.IsZero() {
			view.CheckInsFailingSince, view.LastCheckInError = &failing, lastError
		}
		for _, send := range sends {
			view.RecentSends = append(view.RecentSends, downAlarmSendView(ctx, tx, send))
		}
		return nil
	})
	return view, err
}

// SendDownAlarmTest asks the MMHotline-down alarm to send each held message's test
// request now, so the team sees that the MMHotline-down alarm can reach the people on
// call. Any member may start one.
func (a *App) SendDownAlarmTest(ctx context.Context, p Principal) (DownAlarmView, error) {
	var setup downAlarmSetup
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := RequireMember(ctx, tx, p, false); err != nil {
			return err
		}
		var found bool
		var err error
		setup, found, err = readDownAlarmSetup(ctx, tx)
		if err == nil && !found {
			err = fmt.Errorf("%w: the MMHotline-down alarm is not set up", ErrConflict)
		}
		return err
	})
	if err != nil {
		return DownAlarmView{}, err
	}
	// The test goes to the held messages of the last check-in, made now.
	a.downAlarm.mu.Lock()
	a.downAlarm.lastAttemptAt = time.Time{}
	a.downAlarm.mu.Unlock()
	a.downAlarmRound(ctx, func(error) {})
	tests, err := a.downAlarmClient(setup).SendTest(ctx)
	if err != nil {
		return DownAlarmView{}, fmt.Errorf("%w: the MMHotline-down alarm could not send the test: %s", ErrConflict, err)
	}
	view, err := a.DownAlarm(ctx, p)
	if err != nil {
		return view, err
	}
	var pushoverTests []bool
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		for _, test := range tests {
			send := downAlarmSendView(ctx, tx, downalarm.Send{HeldMessageID: test.HeldMessageID, SendOutcome: test.SendOutcome, HTTPStatusCode: test.HTTPStatusCode})
			view.TestSends = append(view.TestSends, send)
			pushoverTests = append(pushoverTests, test.SendOutcome == "delivered" && downAlarmHeldMessageIsPushover(ctx, tx, test.HeldMessageID))
		}
		return nil
	})
	if err != nil {
		return view, err
	}
	// Pushover's receipt proves the notification: read it, then cancel the
	// repeats. A voice call has no receipt and keeps today's test.
	pushover, _ := a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	for index, test := range tests {
		if !pushoverTests[index] {
			continue
		}
		send := &view.TestSends[index]
		send.PushoverReceipt = test.PushoverReceipt
		if pushover == nil {
			send.PushoverReceiptOutcome, send.PushoverReceiptProblem = pushoverReceiptRefused, "Pushover is not set up"
			continue
		}
		send.PushoverReceiptOutcome, send.PushoverReceiptProblem = pushover.proveEmergencyAndCancelIt(ctx, test.PushoverReceipt)
	}
	return view, nil
}

func downAlarmHeldMessageIsPushover(ctx context.Context, tx *sql.Tx, heldMessageID string) bool {
	match := downAlarmHeldMessageIDPattern.FindStringSubmatch(heldMessageID)
	if match == nil {
		return false
	}
	var contactMethodType string
	return tx.QueryRowContext(ctx, `SELECT contact_method_type FROM contact_methods WHERE id=?`, match[2]).Scan(&contactMethodType) == nil && contactMethodType == "pushover_notification"
}

// text says what a test of the MMHotline-down alarm proved for one person,
// and for a Pushover notification what Pushover's receipt proved.
func (s DownAlarmSendView) testText() string {
	line := fmt.Sprintf("Test: %s to %s: %s.", s.ContactMethodType, s.PersonName, downAlarmOutcomeLabel(s.SendOutcome, s.HTTPStatusCode))
	problem := ""
	if s.PushoverReceiptProblem != "" {
		problem = " " + strings.TrimSuffix(s.PushoverReceiptProblem, ".") + "."
	}
	switch s.PushoverReceiptOutcome {
	case pushoverReceiptProven:
		line += fmt.Sprintf(" Pushover accepted the MMHotline-down alarm's notification (receipt %s). MMHotline cancelled its repeats.", s.PushoverReceipt)
	case pushoverReceiptMissing:
		line += " Pushover's answer had no receipt, so nothing proves that Pushover accepted the notification."
	case pushoverReceiptRefused:
		line += " Pushover refused the receipt, so nothing proves that Pushover accepted the notification." + problem
	case pushoverCancelFailed:
		line += fmt.Sprintf(" Pushover accepted the MMHotline-down alarm's notification (receipt %s), but MMHotline could not cancel its repeats.%s The phone may sound again for up to 10 minutes; acknowledge the notification in Pushover.", s.PushoverReceipt, problem)
	}
	return line
}

// Text is what the command prints about the MMHotline-down alarm.
func (v DownAlarmView) Text() string {
	if !v.IsSetUp {
		return "The MMHotline-down alarm is not set up."
	}
	lines := []string{"MMHotline-down alarm Worker " + v.WorkerName + " at " + v.WorkerURL + ".", fmt.Sprintf("Set up by %s at %s.", v.SetUpBy, commandTime(*v.SetUpAt))}
	switch {
	case v.CheckInsFailingSince != nil:
		lines = append(lines, fmt.Sprintf("Check-ins have failed since %s: %s", commandTime(*v.CheckInsFailingSince), v.LastCheckInError))
	case v.LastCheckInAcceptedAt != nil:
		lines = append(lines, "Last check-in accepted at "+commandTime(*v.LastCheckInAcceptedAt)+".")
	default:
		lines = append(lines, "No check-in yet since MMHotline started.")
	}
	if len(v.HeldMessages) == 0 {
		lines = append(lines, "If MMHotline stops checking in, the MMHotline-down alarm reaches nobody: nobody on call has a Pushover notification or automated voice call contact method for SOS.")
	}
	for _, h := range v.HeldMessages {
		lines = append(lines, fmt.Sprintf("If MMHotline stops checking in: %s to %s (%s) after %d minutes.", h.ContactMethodType, h.PersonName, h.SentTo, h.DelayMinutes))
	}
	for _, s := range v.RecentSends {
		lines = append(lines, fmt.Sprintf("Sent: %s to %s, due %s: %s.", s.ContactMethodType, s.PersonName, commandTime(s.DueAt), downAlarmOutcomeLabel(s.SendOutcome, s.HTTPStatusCode)))
	}
	for _, s := range v.TestSends {
		lines = append(lines, s.testText())
	}
	return strings.Join(lines, "\n")
}
