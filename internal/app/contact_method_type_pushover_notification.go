package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mmhotline/internal/downalarm"
)

// PushoverNotificationContactMethodType uses only outbound requests. Emergency receipts are scheduled
// by the shared team scheduler, never by an adapter-owned polling loop.
type PushoverNotificationContactMethodType struct {
	ApplicationToken, APIBaseURL string
	Client                       *http.Client
}

const pushoverReceiptInterval = 5 * time.Second

// Pushover repeats an emergency at most 50 times, so at a 30-second retry it
// stops sounding after 1,500 seconds. The receipt expires then too: while a
// receipt is open MMHotline sends no new emergency, so a later expiry would
// leave a waiting SOS silent until it came.
const pushoverEmergencyExpirySeconds = 50 * 30
const pushoverTestExpirySeconds = 600

var pushoverKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)

// pushoverDeviceNamePattern is Pushover's own rule for a device name
// (https://pushover.net/api#identifiers): up to 25 characters of letters,
// digits, underscore, and hyphen.
var pushoverDeviceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,25}$`)

func (*PushoverNotificationContactMethodType) Name() string { return "pushover_notification" }
func (*PushoverNotificationContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{CardTitle: "Pushover notification", CardTitleIsAProductName: true,
		ValueLabel: "Pushover user key", ValueField: "pushover_user_key", ValueInputType: "text", SettingsFields: []string{"pushover_user_key", "pushover_device_name"},
		WakesPeople: true, WakesSleepingPeople: true, ClaimMethod: "acknowledgement", CombinesMessages: true, CombinesSOSEscalation: true,
		RetryUncertain: true, MaximumPerPerson: 3, AcceptedLabel: "Accepted by Pushover"}
}
func (*PushoverNotificationContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	key := strings.TrimSpace(input.Fields["pushover_user_key"])
	if !pushoverKeyPattern.MatchString(key) {
		return ContactMethodSettings{}, fmt.Errorf("%w: Pushover user key must contain exactly 30 letters and digits", ErrInvalid)
	}
	device := strings.TrimSpace(input.Fields["pushover_device_name"])
	if device != "" && !pushoverDeviceNamePattern.MatchString(device) {
		return ContactMethodSettings{}, fmt.Errorf("%w: Pushover device name must be 1 to 25 characters: letters, digits, _ and -", ErrInvalid)
	}
	settings, err := settingsJSON(struct {
		UserKey    string `json:"pushover_user_key"`
		DeviceName string `json:"pushover_device_name,omitempty"`
	}{key, device})
	return ContactMethodSettings{JSON: settings, SentTo: key}, err
}
func (*PushoverNotificationContactMethodType) Setup() SendingServiceSetupDefinition {
	return SendingServiceSetupDefinition{
		Name: "pushover", Title: "Pushover",
		Fields: []SendingServiceSetupField{{Name: "pushover_application_token", SettingsKey: "application_token", Label: "Pushover application token", Secret: true, Help: "The API token of your team's MMHotline application. A new token replaces the current application token and keeps every Pushover contact method."}},
		Guide: []SendingServiceSetupStep{
			{Text: "Create a Pushover application named MMHotline.", URL: "https://pushover.net/apps/build", LinkLabel: "Create the application"},
			{Text: "Put the MMHotline application's API token in the settings file, as shown below, and apply the settings file. MMHotline checks the token and the current month's message limit with Pushover before saving the token."},
			{Text: "Each person adds a personal Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification."}},
		NextStep:        "Add your own Pushover user key under Contact methods. Saving the Pushover contact method sends a quiet test notification.",
		IdentitySetting: "pushover_application_token", IdentitySecret: true, IdentityLabel: "Pushover application",
		ReplacingKeepsContactMethods: true,
		DescribeCheck: func(settings map[string]any) string {
			return fmt.Sprintf("Checked with Pushover: monthly message limit %v", settings["pushover_monthly_message_limit"])
		},
		Check: checkPushoverSetup, Build: buildPushoverSetup,
		ClearConnections: func(ctx context.Context, tx *sql.Tx) error {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pushover_receipts r JOIN deliveries d ON d.id=r.delivery_id WHERE r.state IN ('open','cancelling') OR (r.state='sending' AND d.state='sending')`).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("%w: wait for MMHotline to close the open Pushover emergency messages before replacing or removing the application", ErrConflict)
			}
			return nil
		},
	}
}
func buildPushoverSetup(_ map[string]any, secrets map[string]string, fakeURL string) ContactMethodType {
	c := &PushoverNotificationContactMethodType{ApplicationToken: secrets["pushover_application_token"]}
	if fakeURL != "" {
		c.APIBaseURL = fakeURL + "/pushover"
	}
	return c
}
func checkPushoverSetup(ctx context.Context, fields map[string]string, fakeURL string) (sendingServiceSetupCheck, error) {
	token := fields["pushover_application_token"]
	if !pushoverKeyPattern.MatchString(token) {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Pushover application token must contain exactly 30 letters and digits", ErrInvalid)
	}
	c := buildPushoverSetup(nil, fields, fakeURL).(*PushoverNotificationContactMethodType)
	answer, err := c.call(ctx, http.MethodGet, "apps/limits", nil)
	if err != nil {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Checked with Pushover: %s", ErrInvalid, err)
	}
	if answer.Limit <= 0 {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Pushover did not report a monthly message limit", ErrInvalid)
	}
	return sendingServiceSetupCheck{Settings: map[string]any{"pushover_monthly_message_limit": answer.Limit}, SecretSettings: map[string]string{"pushover_application_token": token}}, nil
}

// These are the documented fields of https://pushover.net/api and
// https://pushover.net/api/receipts. No callback or sound is supplied; `device` is supplied only when the person named one.
type pushoverAnswer struct {
	Status         int      `json:"status"`
	Errors         []string `json:"errors"`
	Request        string   `json:"request"`
	Receipt        string   `json:"receipt"`
	Limit          int64    `json:"limit"`
	Group          int      `json:"group"`
	Devices        []string `json:"devices"`
	Acknowledged   int      `json:"acknowledged"`
	AcknowledgedBy string   `json:"acknowledged_by"`
	Expired        int      `json:"expired"`
	ExpiresAt      int64    `json:"expires_at"`
}
type pushoverAPIError struct {
	HTTPStatusCode int
	Text           string
}

func (e *pushoverAPIError) Error() string {
	return fmt.Sprintf("Pushover answered HTTP %d: %s", e.HTTPStatusCode, e.Text)
}

var errPushoverNoAnswer = errors.New("no readable answer from Pushover")
var errPushoverConnection = errors.New("could not connect to Pushover")

// request builds one Pushover API request with the application token. Both
// delivery and the MMHotline-down alarm's held messages use it.
func (c *PushoverNotificationContactMethodType) request(ctx context.Context, method, path string, fields url.Values) (*http.Request, error) {
	if fields == nil {
		fields = url.Values{}
	}
	fields.Set("token", c.ApplicationToken)
	base := c.APIBaseURL
	if base == "" {
		base = "https://api.pushover.net"
	}
	address := strings.TrimRight(base, "/") + "/1/" + path + ".json"
	var body io.Reader
	if method == http.MethodGet {
		address += "?" + fields.Encode()
	} else {
		body = strings.NewReader(fields.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, errors.New("could not construct the Pushover request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request, nil
}

func (c *PushoverNotificationContactMethodType) call(ctx context.Context, method, path string, fields url.Values) (pushoverAnswer, error) {
	var answer pushoverAnswer
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := c.request(ctx, method, path, fields)
	if err != nil {
		return answer, err
	}
	client := http.Client{}
	if c.Client != nil {
		client = *c.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		var connection *net.OpError
		if errors.As(err, &connection) && connection.Op == "dial" {
			return answer, errPushoverConnection
		}
		return answer, errPushoverNoAnswer
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	decoded := readErr == nil && len(data) <= 65536 && json.Unmarshal(data, &answer) == nil
	if response.StatusCode != http.StatusOK || (decoded && answer.Status != 1) {
		diagnostic := "unreadable answer"
		if decoded && len(answer.Errors) > 0 {
			diagnostic = strings.Join(answer.Errors, "; ")
		}
		diagnostic = strings.ReplaceAll(diagnostic, c.ApplicationToken, "[secret removed]")
		if key := fields.Get("user"); key != "" {
			diagnostic = strings.ReplaceAll(diagnostic, key, "[user key removed]")
		}
		return answer, &pushoverAPIError{response.StatusCode, boundedDiagnostic(diagnostic)}
	}
	if !decoded {
		return answer, errPushoverNoAnswer
	}
	return answer, nil
}

// checkUserKeyAndDevice asks Pushover about key once
// (https://pushover.net/api#validate). It answers whether key is a
// delivery-group key ("group": 1), false when Pushover does not answer. When a
// device name is given it must be one of the devices Pushover lists for the
// user; otherwise the error lists the names Pushover returned. Pushover not
// answering is not a refusal: the test message then shows any problem.
func (c *PushoverNotificationContactMethodType) checkUserKeyAndDevice(ctx context.Context, key, device string) (groupKey bool, err error) {
	answer, callErr := c.call(ctx, http.MethodPost, "users/validate", url.Values{"user": {key}})
	if callErr != nil {
		return false, nil
	}
	if device != "" && !slices.Contains(answer.Devices, device) {
		names := "Pushover returned no device names for this user key"
		if len(answer.Devices) > 0 {
			names = "The device names Pushover returned are: " + strings.Join(answer.Devices, ", ")
		}
		return false, fmt.Errorf("%w: this Pushover user has no device named %s. %s", ErrInvalid, device, names)
	}
	return answer.Group == 1, nil
}

// userKeyDevices asks Pushover about key once
// (https://pushover.net/api#validate) and answers the device names Pushover
// lists for it, for `connect-pushover` to offer. Pushover refusing the key
// (an HTTP 4xx answer) is an error. Pushover not answering is not: the device
// names are then unknown (nil), and the contact method's test message shows
// any problem with the key, as checkUserKeyAndDevice treats it.
func (c *PushoverNotificationContactMethodType) userKeyDevices(ctx context.Context, key string) ([]string, error) {
	if !pushoverKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("%w: a Pushover user key is exactly 30 letters and digits", ErrInvalid)
	}
	answer, err := c.call(ctx, http.MethodPost, "users/validate", url.Values{"user": {key}})
	var refused *pushoverAPIError
	if errors.As(err, &refused) && refused.HTTPStatusCode >= 400 && refused.HTTPStatusCode < 500 {
		return nil, fmt.Errorf("%w: Pushover refused the user key: %s", ErrInvalid, refused.Text)
	}
	if err != nil {
		return nil, nil
	}
	return answer.Devices, nil
}

// withPushoverDevice adds Pushover's `device` field when the contact method
// names one device. With no device named, Pushover sends to all of the
// user's devices.
func withPushoverDevice(fields url.Values, settings string) url.Values {
	if device := settingValue(settings, "pushover_device_name"); device != "" {
		fields.Set("device", device)
	}
	return fields
}

// pushoverGroupKeyWarning is shown, in capitals on its own line, when a
// person adds a group key. name is the contact method owner's name.
func pushoverGroupKeyWarning(name string) string {
	return fmt.Sprintf("THIS IS A PUSHOVER GROUP KEY. ACKNOWLEDGING ON A PHONE WILL NOT CLAIM THE INCIDENT FOR %s. USE %s'S OWN PUSHOVER USER KEY. CLAIMING ON THE INCIDENT'S PAGE STILL WORKS.", strings.ToUpper(name), strings.ToUpper(name))
}

// pushoverUserKeyHelp follows the group key warning and says where the
// person's own user key is.
const pushoverUserKeyHelp = "Your own user key is at the top of pushover.net after you sign in, labelled \"Your User Key\". A group key comes from a delivery group's page."

// pushoverDefaultSubscriptions is every severity: Pushover is often a person's
// only contact method, and Warning and Info go at priorities that wake nobody
// (Send), so subscribing to SOS alone would leave them unheard.
func pushoverDefaultSubscriptions() []string { return []string{"sos", "warn", "info"} }

// pushoverEmergency is true for an SOS only. A contact method test goes at
// ordinary priority and needs no acknowledgement: only an SOS (the test SOS
// of `incidents send-test-sos`) proves that a phone wakes its owner.
func pushoverEmergency(severity string) bool { return severity == "sos" }

// pushoverContactMethodTestTitle is the title of a contact method's test
// notification. It says what the test proves and nothing more.
const pushoverContactMethodTestTitle = "MMHotline can reach this phone"

func (c *PushoverNotificationContactMethodType) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	key := settingValue(m.Settings, "pushover_user_key")
	if !pushoverKeyPattern.MatchString(key) {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the Pushover user key is incomplete"}
	}
	// Info and resolved messages go at low priority, without sound; a
	// warning at normal priority. The resolved message of an SOS that waited
	// for a server replacement goes at normal priority too: the person heard
	// the replacement start as a warn alert, so they also hear it end.
	priority := -1
	if m.Severity == "warn" || m.ResolvesAnSOSThatWaitedForAServerReplacement {
		priority = 0
	}
	emergency := pushoverEmergency(m.Severity)
	text := m.Text
	subject := m.Subject
	if m.Purpose == "contact_method_test" {
		priority, subject = 0, pushoverContactMethodTestTitle
	}
	// The supplementary URL is separate from alert text, so shortening cannot
	// remove the link. Emergency content lists only titles; the page has detail.
	link := m.PublicURL
	if m.IncidentID != "" {
		link += "/incidents/" + m.IncidentID
	} else if m.AlertID != "" {
		link += "/alerts/" + m.AlertID
	}
	if emergency {
		priority = 2
		subject = "[SOS] " + m.Title
		if len(m.CombinedAlerts) > 1 {
			subject = fmt.Sprintf("[SOS] %d incidents need you", len(m.CombinedAlerts))
		}
		instruction := "Acknowledge this to tell MMHotline you are handling it. MMHotline then stops contacting other people."
		if len(m.CombinedAlerts) > 1 {
			instruction = fmt.Sprintf("Acknowledge this to tell MMHotline you are handling all %d. MMHotline then stops contacting other people.", len(m.CombinedAlerts))
		}
		text = m.AlertMessage
		if len(m.CombinedAlerts) > 1 {
			var titles []string
			for _, alert := range m.CombinedAlerts {
				titles = append(titles, alert.Title)
			}
			text = linesWithin(titles, 1024-utf8.RuneCountInString(instruction)-2)
		}
		text = truncateRunes(text, 1024-utf8.RuneCountInString(instruction)-2)
		if text != "" {
			text += "\n\n"
		}
		text += instruction
	} else {
		// Keep the link in the supplementary URL. Only alert text is shortened.
		if len(m.CombinedAlerts) > 0 {
			var titles []string
			for _, alert := range m.CombinedAlerts {
				titles = append(titles, severityLabel(alert.Severity)+": "+alert.Title)
			}
			text = linesWithin(titles, 1024)
		} else if m.Title != "" || m.AlertMessage != "" {
			// About one alert: the title field names it, so the message starts
			// with the alert's message. A message about no alert, such as a
			// claim result, keeps its own text.
			text = m.AlertMessage
			if m.NotesFromMmhotline != "" {
				text += "\nNotes: " + m.NotesFromMmhotline
			}
			if !m.AlertReceivedAt.IsZero() {
				text += "\nAt: " + m.AlertReceivedAt.UTC().Format(time.RFC3339)
			}
		}
		text = strings.TrimLeft(text, "\n")
		text = truncateRunes(text, 1024)
	}
	if strings.TrimSpace(text) == "" {
		text = "Open MMHotline for details."
	}
	if utf8.RuneCountInString(link) > 512 || utf8.RuneCountInString(text) > 1024 {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the Pushover message or MMHotline link exceeds Pushover's limits"}
	}
	fields := withPushoverDevice(url.Values{"user": {key}, "title": {truncateRunes(subject, 250)}, "message": {text}, "priority": {strconv.Itoa(priority)}, "url": {link}, "url_title": {"Open in MMHotline"}}, m.Settings)
	if emergency {
		fields.Set("retry", "30")
		fields.Set("expire", strconv.Itoa(pushoverEmergencyExpirySeconds))
	}
	answer, err := c.call(ctx, http.MethodPost, "messages", fields)
	if err != nil {
		var refusal *pushoverAPIError
		if errors.As(err, &refusal) {
			return DeliveryResult{State: "failed", Diagnostic: err.Error(), Retryable: refusal.HTTPStatusCode == 429 || refusal.HTTPStatusCode >= 500}
		}
		if errors.Is(err, errPushoverConnection) {
			return DeliveryResult{State: "failed", Diagnostic: err.Error(), Retryable: true}
		}
		return DeliveryResult{State: "uncertain", Diagnostic: err.Error(), Retryable: !emergency}
	}
	id := answer.Request
	if emergency {
		if !pushoverKeyPattern.MatchString(answer.Receipt) {
			return DeliveryResult{State: "uncertain", Diagnostic: "Pushover accepted the emergency message without a usable receipt"}
		}
		id = answer.Receipt
	}
	return DeliveryResult{State: "accepted", IDFromSendingService: id}
}

// downAlarmRequests builds the MMHotline-down alarm's held message: an emergency
// notification that repeats until acknowledged, as an SOS does, and its test.
func (c *PushoverNotificationContactMethodType) downAlarmRequests(contactMethod ContactMethod, text downAlarmText) (downalarm.HeldRequest, downalarm.HeldRequest, error) {
	key := settingValue(contactMethod.Settings, "pushover_user_key")
	if !pushoverKeyPattern.MatchString(key) {
		return downalarm.HeldRequest{}, downalarm.HeldRequest{}, errors.New("the Pushover user key is incomplete")
	}
	build := func(title, message string, expire int) (downalarm.HeldRequest, error) {
		fields := withPushoverDevice(url.Values{"user": {key}, "title": {title}, "message": {message}, "priority": {"2"}, "retry": {"30"}, "expire": {strconv.Itoa(expire)}, "url": {text.WebURL}, "url_title": {"Open MMHotline"}}, contactMethod.Settings)
		return heldRequest(c.request(context.Background(), http.MethodPost, "messages", fields))
	}
	request, err := build(text.title(), text.message(), pushoverEmergencyExpirySeconds)
	if err != nil {
		return request, downalarm.HeldRequest{}, err
	}
	test, err := build(text.testTitle(), text.testMessage(), pushoverTestExpirySeconds)
	return request, test, err
}

// linesWithin joins whole lines, in order, while they fit in limit
// characters, and ends with "And N more." for the lines left out, as a
// combined email does. A first line longer than the limit is shortened.
func linesWithin(lines []string, limit int) string {
	text := ""
	for i, line := range lines {
		next := line
		if i > 0 {
			next = text + "\n" + line
		}
		more := ""
		if left := len(lines) - i - 1; left > 0 {
			more = fmt.Sprintf("\nAnd %d more.", left)
		}
		if utf8.RuneCountInString(next+more) > limit {
			if i == 0 {
				return truncateRunes(line, limit-utf8.RuneCountInString(more)) + more
			}
			return text + fmt.Sprintf("\nAnd %d more.", len(lines)-i)
		}
		text = next
	}
	return text
}
