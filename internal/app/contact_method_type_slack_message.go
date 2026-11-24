package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
)

// SlackMessageContactMethodType belongs to the team's own Slack app. All sending service traffic is
// outbound: Web API for sending, Socket Mode for connections and button presses.
type SlackMessageContactMethodType struct {
	BotToken, AppLevelToken                      string
	WorkspaceID, WorkspaceName, BotUserID, AppID string
	BaseURL                                      string
	Client                                       *http.Client
}

const (
	slackConnectionCodeLifetime         = 15 * time.Minute
	slackUnusedConnectionCodesPerMember = 5
	slackMaximumSectionRunes            = 3000
	slackCallTimeout                    = 10 * time.Second
	slackClaimActionID                  = "claim_incident"
	slackResolveActionID                = "resolve_incident"
	slackOpenIncidentActionID           = "open_incident"
	slackClaimAction                    = "c"
	slackResolveAction                  = "r"
)

const slackTeamSchema = `
CREATE TABLE slack_user_links (
 slack_workspace_id TEXT NOT NULL,
 slack_user_id TEXT NOT NULL,
 member_id TEXT NOT NULL,
 linked_at INTEGER NOT NULL,
 PRIMARY KEY(slack_workspace_id,slack_user_id)
);
CREATE TABLE slack_connection_codes (
 code_hash TEXT PRIMARY KEY,
 member_id TEXT NOT NULL,
 slack_connection TEXT NOT NULL CHECK(slack_connection IN ('personal_direct_message','shared_channel')),
 expires_at INTEGER NOT NULL,
 consumed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX slack_connection_codes_member ON slack_connection_codes(member_id,expires_at);
`

type slackSettings struct {
	SlackWorkspaceID      string `json:"slack_workspace_id"`
	SlackConversationID   string `json:"slack_conversation_id"`
	SlackConversationKind string `json:"slack_conversation_kind"`
}

var slackIDPattern = regexp.MustCompile(`^[A-Z0-9]{3,40}$`)

func (*SlackMessageContactMethodType) Name() string { return "slack_message" }

// Facts: RetryUncertain is true because a repeated chat message is harmless,
// as with Telegram.
func (*SlackMessageContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{
		CardTitle: "Slack message", CardTitleIsAProductName: true, ValueLabel: "Slack channel or direct message", ValueField: "slack_conversation_id",
		ConnectsThroughSendingService: true, ClaimMethod: "button", CombinesMessages: true,
		RetryUncertain: true, AllowedForTeamAccount: true, AcceptedLabel: "Posted to Slack",
	}
}

func (c *SlackMessageContactMethodType) baseURL() string {
	if c.BaseURL == "" {
		return "https://slack.com"
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *SlackMessageContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	settings := slackSettings{input.Fields["slack_workspace_id"], input.Fields["slack_conversation_id"], input.Fields["slack_conversation_kind"]}
	description := cleanSlackName(input.Fields["slack_conversation_description"])
	if !slackIDPattern.MatchString(settings.SlackWorkspaceID) || !slackIDPattern.MatchString(settings.SlackConversationID) || description == "" || (settings.SlackConversationKind != "channel" && settings.SlackConversationKind != "direct_message") {
		return ContactMethodSettings{}, fmt.Errorf("%w: Choose Connect Slack on the Contact methods page", ErrInvalid)
	}
	encoded, err := settingsJSON(settings)
	return ContactMethodSettings{JSON: encoded, SentTo: description}, err
}

func (*SlackMessageContactMethodType) buttonValue(action, deliveryID string) string {
	return action + "." + deliveryID
}
func (*SlackMessageContactMethodType) parseButtonValue(value string) (action, deliveryID string, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || (parts[0] != slackClaimAction && parts[0] != slackResolveAction) || !recordIDPattern.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func slackIDFromSendingService(workspaceID, conversationID, timestamp string) string {
	return workspaceID + "/" + conversationID + "/" + timestamp
}

// cleanSlackName makes a name Slack returned safe to show: valid UTF-8, no
// control characters, at most 80 characters.
func cleanSlackName(s string) string {
	return truncateRunes(strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))), 80)
}

// slackEscape escapes the three characters Slack's message text treats as
// markup, so nothing in an alert can mention @channel or make a link.
// https://api.slack.com/reference/surfaces/formatting#escaping
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Block Kit objects, with only the fields MMHotline uses.
// https://api.slack.com/reference/block-kit/blocks
type slackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji"`
}
type slackBlock struct {
	Type     string         `json:"type"`
	BlockID  string         `json:"block_id,omitempty"`
	Text     *slackText     `json:"text,omitempty"`
	Elements []slackElement `json:"elements,omitempty"`
}

// slackElement is a button. A button has a value (sent back when pressed) or
// a URL (opened when pressed).
// https://api.slack.com/reference/block-kit/block-elements#button
type slackElement struct {
	Type     string    `json:"type"`
	Text     slackText `json:"text"`
	ActionID string    `json:"action_id"`
	Value    string    `json:"value,omitempty"`
	URL      string    `json:"url,omitempty"`
	Style    string    `json:"style,omitempty"`
}
type slackMessage struct {
	Channel string       `json:"channel,omitempty"`
	Text    string       `json:"text"`
	Blocks  []slackBlock `json:"blocks"`
}

func plainSlackText(s string) slackText { return slackText{Type: "plain_text", Text: s} }

// slackAPIError is an answer from Slack that refused the request. Code is
// Slack's error code, such as "channel_not_found".
type slackAPIError struct {
	HTTPStatusCode int
	Code           string
}

func (e *slackAPIError) Error() string {
	return fmt.Sprintf("Slack answered HTTP %d: %s", e.HTTPStatusCode, e.Code)
}

// errSlackNoAnswer means the outcome is unknown. Its text never includes the
// request URL or submitted tokens.
var errSlackNoAnswer = errors.New("no readable answer from Slack")

// do sends one request to Slack and decodes the Web API answer shape:
// HTTP 200 with {"ok": true, ...} or {"ok": false, "error": "..."}.
// https://api.slack.com/web#responses
func (c *SlackMessageContactMethodType) do(req *http.Request, result any) error {
	// Every request has a ten-second bound, even inside a longer operation.
	ctx, cancel := context.WithTimeout(req.Context(), slackCallTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	client := http.Client{}
	if c.Client != nil {
		client = *c.Client
	}
	// A redirect must never move a token elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errSlackNoAnswer
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errSlackNoAnswer
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &slackAPIError{HTTPStatusCode: resp.StatusCode}
	}
	var answer struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &answer) != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return errSlackNoAnswer
		}
		return &slackAPIError{HTTPStatusCode: resp.StatusCode, Code: "unreadable answer"}
	}
	if !answer.OK || resp.StatusCode != http.StatusOK {
		return &slackAPIError{HTTPStatusCode: resp.StatusCode, Code: c.safeDiagnostic(answer.Error)}
	}
	if result != nil && json.Unmarshal(data, result) != nil {
		return errSlackNoAnswer
	}
	return nil
}

func (c *SlackMessageContactMethodType) safeDiagnostic(text string) string {
	for _, token := range []string{c.BotToken, c.AppLevelToken} {
		if token != "" {
			text = strings.ReplaceAll(text, token, "[secret removed]")
		}
	}
	return boundedDiagnostic(text)
}

// callJSON posts one Web API method as JSON with a bot token.
// https://api.slack.com/web#posting_json
func (c *SlackMessageContactMethodType) callJSON(ctx context.Context, method, botToken string, request, result any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not construct the Slack request")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+botToken)
	return c.do(req, result)
}

func (c *SlackMessageContactMethodType) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	var settings slackSettings
	if json.Unmarshal([]byte(m.Settings), &settings) != nil || !slackIDPattern.MatchString(settings.SlackWorkspaceID) || !slackIDPattern.MatchString(settings.SlackConversationID) || c.BotToken == "" || settings.SlackWorkspaceID != c.WorkspaceID {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the Slack settings are incomplete"}
	}
	text := truncateRunes(fitChatMessage(m.Text, m.AlertMessage, func(s string) bool { return utf8.RuneCountInString(s) <= slackMaximumSectionRunes }), slackMaximumSectionRunes)
	message := slackMessage{Text: slackEscape(text), Blocks: []slackBlock{{Type: "section", Text: &slackText{Type: "plain_text", Text: text}}}}
	if m.ClaimButtons {
		message.Blocks = append(message.Blocks, slackBlock{Type: "actions", BlockID: "mmhotline_incident_actions", Elements: []slackElement{
			{Type: "button", Text: plainSlackText("Claim this incident (assign it to me)"), ActionID: slackClaimActionID, Value: c.buttonValue(slackClaimAction, m.DeliveryID), Style: "primary"},
			{Type: "button", Text: plainSlackText("Resolve"), ActionID: slackResolveActionID, Value: c.buttonValue(slackResolveAction, m.DeliveryID)},
		}})
	}
	message.Channel = settings.SlackConversationID
	var sent struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	err := c.callJSON(ctx, "chat.postMessage", c.BotToken, message, &sent)
	var refused *slackAPIError
	switch {
	case err == nil && sent.TS != "":
		return DeliveryResult{State: "accepted", IDFromSendingService: slackIDFromSendingService(settings.SlackWorkspaceID, settings.SlackConversationID, sent.TS)}
	case errors.As(err, &refused) && refused.HTTPStatusCode == http.StatusTooManyRequests:
		return DeliveryResult{State: "failed", Diagnostic: "Slack asked MMHotline to send more slowly (HTTP 429)", Retryable: true}
	case errors.As(err, &refused) && refused.HTTPStatusCode >= 500:
		return DeliveryResult{State: "uncertain", Diagnostic: fmt.Sprintf("Slack answered HTTP %d", refused.HTTPStatusCode), Retryable: true}
	// Slack reports its own temporary trouble as HTTP 200 with ok:false.
	// After these errors Slack may have posted the message after all.
	// https://api.slack.com/methods/chat.postMessage#errors
	case errors.As(err, &refused) && (refused.Code == "internal_error" || refused.Code == "fatal_error" || refused.Code == "request_timeout"):
		return DeliveryResult{State: "uncertain", Diagnostic: "Slack had a temporary problem: " + refused.Code, Retryable: true}
	case errors.As(err, &refused) && (refused.Code == "service_unavailable" || refused.Code == "ratelimited"):
		return DeliveryResult{State: "failed", Diagnostic: "Slack had a temporary problem: " + refused.Code, Retryable: true}
	case errors.As(err, &refused):
		return DeliveryResult{State: "failed", Diagnostic: "Refused by Slack: " + refused.Code}
	}
	return DeliveryResult{State: "uncertain", Diagnostic: "No answer from Slack", Retryable: true}
}
func (a *App) slackMessageContactMethodType() (*SlackMessageContactMethodType, error) {
	slack, ok := a.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType)
	if !ok {
		return nil, fmt.Errorf("%w: Ask an administrator to set up Slack before connecting your Slack account", ErrInvalid)
	}
	return slack, nil
}

// slackInteraction is a block_actions payload: only the fields MMHotline
// reads. team is the workspace the message is in; user.team_id is the
// presser's own workspace, which differs in a channel shared between
// workspaces.
// https://api.slack.com/reference/interaction-payloads/block-actions
type slackInteraction struct {
	AppID string `json:"api_app_id"`
	Type  string `json:"type"`
	Team  struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		MessageTS string `json:"message_ts"`
		ChannelID string `json:"channel_id"`
	} `json:"container"`
	Message struct {
		Text   string            `json:"text"`
		Blocks []json.RawMessage `json:"blocks"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// Replies only the presser sees, sent with chat.postEphemeral.
const (
	slackUnknownAccountText    = "MMHotline does not know your Slack account yet. Connect your Slack account at %s. Or claim the incident in MMHotline at %s"
	slackButtonNotUsableText   = "MMHotline cannot use this button. Open MMHotline to see the incident."
	slackButtonPressFailedText = "MMHotline could not apply the incident action. Try again, or open the incident in MMHotline."
)

var errSlackButtonNotOurs = errors.New("the Slack button does not belong to its message")

// slackLink marks a URL MMHotline made as a link, so Slack never takes the
// period after it into the link. MMHotline's own URLs hold no <, >, or |.
// https://api.slack.com/reference/surfaces/formatting#linking-urls
func slackLink(u string) string { return "<" + u + ">" }

// slackButtonPress is what one press did.
type slackButtonPress struct {
	incidentURL, linkURL string
	unknownUser          bool
	results              []NotificationActionResult
}

// pressSlackButton checks the delivery's workspace, conversation, message,
// and the current membership of the linked user. Only a
// Slack user linked to a current member can claim or resolve through the
// shared notification operation.
func (a *App) pressSlackButton(ctx context.Context, slack *SlackMessageContactMethodType, deliveryID, action string, press slackInteraction) (slackButtonPress, error) {
	var result slackButtonPress
	var memberID string
	channelID := press.Channel.ID
	if channelID == "" {
		channelID = press.Container.ChannelID
	}
	presserTeamID := press.User.TeamID
	if presserTeamID == "" {
		presserTeamID = press.Team.ID
	}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var contactMethodType, idFromSendingService, incidentID string
		err := tx.QueryRowContext(ctx, `SELECT contact_method_type,id_from_sending_service,incident_id FROM deliveries WHERE id=?`, deliveryID).Scan(&contactMethodType, &idFromSendingService, &incidentID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		sentTo := strings.SplitN(idFromSendingService, "/", 3)
		if contactMethodType != "slack_message" || incidentID == "" || len(sentTo) != 3 || sentTo[1] != channelID || (sentTo[2] == "" || sentTo[2] != press.Container.MessageTS) || sentTo[0] != press.Team.ID || sentTo[0] != slack.WorkspaceID {
			return errSlackButtonNotOurs
		}
		// Only a message that was sent with buttons can be pressed.
		d, err := scanDelivery(tx.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM deliveries WHERE id=?`, deliveryID))
		if err != nil {
			return err
		}
		combined, err := combinedAlerts(ctx, tx, Delivery{ID: d.ID})
		if err != nil {
			return err
		}
		facts, _ := contactMethodTypeFacts(contactMethodType)
		if !claimButtons(d, facts, combined) {
			return errSlackButtonNotOurs
		}
		result.incidentURL = strings.TrimRight(a.WebURL, "/") + "/incidents/" + incidentID
		result.linkURL = strings.TrimRight(a.WebURL, "/") + "/contact-methods"
		// A Slack user is known only in the workspace the message went to.
		if presserTeamID != sentTo[0] {
			result.unknownUser = true
			return nil
		}
		err = tx.QueryRowContext(ctx, `SELECT l.member_id FROM slack_user_links l JOIN members m ON m.id=l.member_id WHERE m.removed_at IS NULL AND l.slack_workspace_id=? AND l.slack_user_id=?`, sentTo[0], press.User.ID).Scan(&memberID)
		if errors.Is(err, sql.ErrNoRows) {
			result.unknownUser = true
			return nil
		}
		return err
	})
	if err != nil || result.unknownUser {
		return result, err
	}
	if action == slackResolveAction {
		result.results, err = a.ResolveIncidentsFromNotification(ctx, deliveryID, memberID, "slack_button")
	} else {
		result.results, err = a.ClaimIncidentsFromNotification(ctx, deliveryID, memberID, "slack_button")
	}
	if err == nil && len(result.results) == 0 {
		err = ErrNotFound
	}
	return result, err
}

// handleSlackButtonPress runs a press on "Claim this incident" or "Resolve"
// after acknowledgement, then replaces the pressed message with chat.update.
// One button shows the incident's state
// ("Claimed by Alice") and opens the incident; after a claim, Resolve stays
// beside it. Other copies of the message
// are not updated. The presser also gets a reply only they see, including
// "Already claimed by Alice" when someone else holds the incident.
// https://api.slack.com/interactivity/handling#message_responses
func (a *App) handleSlackButtonPress(ctx context.Context, slack *SlackMessageContactMethodType, press slackInteraction) (replyError error) {
	if len(press.Actions) != 1 || !slackIDPattern.MatchString(press.User.ID) || press.Container.ChannelID == "" || (press.Channel.ID != "" && press.Channel.ID != press.Container.ChannelID) {
		return
	}
	pressed := press.Actions[0]
	if pressed.ActionID != slackClaimActionID && pressed.ActionID != slackResolveActionID {
		// The button that opens the incident needs nothing from MMHotline.
		return
	}
	replyWithLinks := func(text string) {
		replyError = errors.Join(replyError, slack.callJSON(ctx, "chat.postEphemeral", slack.BotToken, map[string]any{"channel": press.Container.ChannelID, "user": press.User.ID, "text": text}, nil))
	}
	reply := func(text string) { replyWithLinks(slackEscape(text)) }
	action, deliveryID, ok := slack.parseButtonValue(pressed.Value)
	if !ok || (action == slackClaimAction) != (pressed.ActionID == slackClaimActionID) {
		reply(slackButtonNotUsableText)
		return
	}
	result, err := a.pressSlackButton(ctx, slack, deliveryID, action, press)
	switch {
	case errors.Is(err, errSlackButtonNotOurs) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden):
		reply(slackButtonNotUsableText)
		return
	case err != nil:
		reply(slackButtonPressFailedText)
		return
	case result.unknownUser:
		replyWithLinks(fmt.Sprintf(slackUnknownAccountText, slackLink(result.linkURL), slackLink(result.incidentURL)))
		return
	}
	first := result.results[0]
	label := "Resolved"
	switch {
	case first.Status == "claimed":
		label = "Claimed by " + first.OwnerName
	case strings.HasPrefix(first.Text, "Resolved by "):
		label = first.Text
	}
	blocks := []json.RawMessage{}
	for _, block := range press.Message.Blocks {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(block, &kind) == nil && kind.Type != "actions" {
			blocks = append(blocks, block)
		}
	}
	// After a claim the Resolve button stays, so the person who claimed can
	// resolve from the chat without signing in. Its value is the original
	// Resolve button's value.
	elements := []slackElement{{Type: "button", Text: plainSlackText(truncateRunes(label, 75)), ActionID: slackOpenIncidentActionID, URL: result.incidentURL}}
	if first.Status == "claimed" {
		elements = append(elements, slackElement{Type: "button", Text: plainSlackText("Resolve"), ActionID: slackResolveActionID, Value: slack.buttonValue(slackResolveAction, deliveryID)})
	}
	stateButton, _ := json.Marshal(slackBlock{Type: "actions", BlockID: "mmhotline_incident_state", Elements: elements})
	blocks = append(blocks, stateButton)
	text := press.Message.Text
	if text == "" {
		text = slackEscape(label)
	}
	replyError = slack.callJSON(ctx, "chat.update", slack.BotToken, map[string]any{"channel": press.Container.ChannelID, "ts": press.Container.MessageTS, "text": text, "blocks": blocks}, nil)
	var texts []string
	for _, r := range result.results {
		texts = append(texts, r.Text)
	}
	reply(strings.Join(texts, " "))
	return
}

// SlackManifestLink opens Slack's app creation page with the team's manifest.
// https://docs.slack.dev/reference/app-manifest/
func SlackManifestLink() string {
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(slackManifest)
}

const slackManifest = `{"display_information":{"name":"MMHotline"},"features":{"bot_user":{"display_name":"MMHotline","always_online":false},"app_home":{"messages_tab_enabled":true,"messages_tab_read_only_enabled":false}},"oauth_config":{"scopes":{"bot":["chat:write","im:history","app_mentions:read"]}},"settings":{"socket_mode_enabled":true,"interactivity":{"is_enabled":true},"event_subscriptions":{"bot_events":["message.im","app_mention"]},"token_rotation_enabled":false}}`

func (*SlackMessageContactMethodType) Setup() SendingServiceSetupDefinition {
	return SendingServiceSetupDefinition{
		Name: "slack", Title: "Slack",
		Fields: []SendingServiceSetupField{
			{Name: "slack_bot_token", SettingsKey: "bot_token", Label: "Slack bot token", Secret: true, Help: "Install the MMHotline app and copy the app's Bot User OAuth Token (xoxb-)."},
			{Name: "slack_app_level_token", SettingsKey: "app_level_token", Label: "Slack app-level token", Secret: true, Help: "Under Basic Information, generate an app-level token (xapp-) with connections:write."},
		},
		Guide: []SendingServiceSetupStep{
			{Text: "Open the link below to create the MMHotline app in your Slack workspace. The link includes the app manifest.", URL: SlackManifestLink(), LinkLabel: "Create the Slack app"},
			{Text: "Install the MMHotline app to your workspace. Copy the MMHotline app's Bot User OAuth Token (xoxb-)."},
			{Text: "Under Basic Information, generate an app-level token (xapp-) with connections:write and copy the app-level token."},
			{Text: "Put the bot token and app-level token in the settings file, as shown below, and apply the settings file. MMHotline checks the tokens with Slack before saving the tokens. Then connect Slack under Contact methods."},
		},
		NextStep:        "Connect your own Slack account under Contact methods. Saving the contact method sends a test message.",
		IdentitySetting: "slack_workspace_id", IdentityLabel: "Slack workspace",
		DescribeCheck: func(settings map[string]any) string {
			return "Checked with Slack: workspace " + fmt.Sprint(settings["slack_workspace_name"]) + " (" + fmt.Sprint(settings["slack_workspace_id"]) + ")"
		},
		Check: checkSlackSetup, Build: buildSlackSetup,
		ClearConnections: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM slack_connection_codes`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM slack_user_links`)
			return err
		},
	}
}

func buildSlackSetup(settings map[string]any, secrets map[string]string, fakeURL string) ContactMethodType {
	c := &SlackMessageContactMethodType{BotToken: secrets["slack_bot_token"], AppLevelToken: secrets["slack_app_level_token"]}
	c.WorkspaceID, _ = settings["slack_workspace_id"].(string)
	c.WorkspaceName, _ = settings["slack_workspace_name"].(string)
	c.BotUserID, _ = settings["slack_bot_user_id"].(string)
	c.AppID, _ = settings["slack_app_id"].(string)
	if fakeURL != "" {
		c.BaseURL = strings.TrimRight(fakeURL, "/") + "/slack"
	}
	return c
}

func checkSlackSetup(ctx context.Context, fields map[string]string, fakeURL string) (sendingServiceSetupCheck, error) {
	c := buildSlackSetup(nil, fields, fakeURL).(*SlackMessageContactMethodType)
	if !strings.HasPrefix(c.BotToken, "xoxb-") || !strings.HasPrefix(c.AppLevelToken, "xapp-") {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Slack requires a bot token starting xoxb- and an app-level token starting xapp-", ErrInvalid)
	}
	failure := func(err error) (sendingServiceSetupCheck, error) {
		reason := strings.NewReplacer(c.BotToken, "[secret removed]", c.AppLevelToken, "[secret removed]").Replace(err.Error())
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Checked with Slack: %s", ErrInvalid, reason)
	}
	var identity struct {
		TeamID string `json:"team_id"`
		Team   string `json:"team"`
		UserID string `json:"user_id"`
	}
	if err := c.callJSON(ctx, "auth.test", c.BotToken, struct{}{}, &identity); err != nil {
		return failure(err)
	}
	// The app ID comes from Socket Mode's hello: bots.info would need the
	// users:read scope, which reads every member's profile, for one ID.
	appID, err := c.helloAppID(ctx)
	if err != nil {
		return failure(err)
	}
	if !slackIDPattern.MatchString(identity.TeamID) || !slackIDPattern.MatchString(identity.UserID) || !slackIDPattern.MatchString(appID) || cleanSlackName(identity.Team) == "" {
		return failure(errors.New("Slack did not return the workspace, bot user, and app identity"))
	}
	return sendingServiceSetupCheck{Settings: map[string]any{"slack_workspace_id": identity.TeamID, "slack_workspace_name": cleanSlackName(identity.Team), "slack_bot_user_id": identity.UserID, "slack_app_id": appID}, SecretSettings: map[string]string{"slack_bot_token": c.BotToken, "slack_app_level_token": c.AppLevelToken}}, nil
}

// helloAppID opens one Socket Mode connection, reads its hello, and closes
// it. This also proves that the app-level token can connect.
func (c *SlackMessageContactMethodType) helloAppID(ctx context.Context) (string, error) {
	connection, err := c.dialSocket(ctx)
	if err != nil {
		return "", err
	}
	defer connection.CloseNow()
	readCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	connection.SetReadLimit(1 << 20)
	_, data, err := connection.Read(readCtx)
	var hello slackSocketEnvelope
	if err != nil || json.Unmarshal(data, &hello) != nil || hello.Type != "hello" {
		return "", errors.New("Slack's WebSocket did not answer with hello")
	}
	return hello.ConnectionInfo.AppID, nil
}

// dialSocket asks Slack for a fresh connection address and opens it.
func (c *SlackMessageContactMethodType) dialSocket(ctx context.Context) (*websocket.Conn, error) {
	address, err := c.openSocketURL(ctx)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	client := http.Client{}
	if c.Client != nil {
		client = *c.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	connection, _, err := websocket.Dial(dialCtx, address, &websocket.DialOptions{HTTPClient: &client})
	if err != nil {
		return nil, errors.New("could not open the Slack WebSocket")
	}
	return connection, nil
}

// SlackConnection contains a one-time code shown only to its member.
type SlackConnection struct {
	Code, AppURL  string
	SharedChannel bool
}

func (a *App) StartSlackConnection(ctx context.Context, p Principal, connection string) (SlackConnection, error) {
	var result SlackConnection
	if connection != "personal_direct_message" && connection != "shared_channel" {
		return result, ErrInvalid
	}
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	slack, err := a.slackMessageContactMethodType()
	if err != nil {
		return result, err
	}
	now := a.Now()
	code := randomToken()
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		member, err := RequireMember(ctx, tx, p, connection == "shared_channel")
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM slack_connection_codes WHERE expires_at<=?`, now.Unix()); err != nil {
			return err
		}
		var unused int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM slack_connection_codes WHERE member_id=? AND consumed_at=0`, member.ID).Scan(&unused); err != nil {
			return err
		}
		if unused >= slackUnusedConnectionCodesPerMember {
			return ErrRateLimited
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_connection_codes(code_hash,member_id,slack_connection,expires_at) VALUES(?,?,?,?)`, tokenHash(code), member.ID, connection, now.Add(slackConnectionCodeLifetime).Unix())
		return err
	})
	if err != nil {
		return result, err
	}
	return SlackConnection{Code: code, AppURL: "https://slack.com/app_redirect?" + url.Values{"app": {slack.AppID}, "team": {slack.WorkspaceID}}.Encode(), SharedChannel: connection == "shared_channel"}, nil
}

type slackEvent struct {
	Type        string `json:"type"`
	User        string `json:"user"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	Text        string `json:"text"`
	Subtype     string `json:"subtype"`
	BotID       string `json:"bot_id"`
}

var slackConnectionCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

var errSlackCodeUnusable = errors.New("the Slack connection code expired, was used, or is unknown")
var errSlackWrongConversation = errors.New("the Slack connection code is for another kind of conversation")

func (a *App) connectSlackConversation(ctx context.Context, slack *SlackMessageContactMethodType, event slackEvent, code string) error {
	if !slackIDPattern.MatchString(event.User) || !slackIDPattern.MatchString(event.Channel) {
		return ErrInvalid
	}
	kind := "direct_message"
	shared := event.Type == "app_mention"
	if shared {
		kind = "channel"
	}
	description := "Slack direct messages with " + event.User + " in " + slack.WorkspaceName
	if shared {
		description = "Slack channel " + event.Channel + " in " + slack.WorkspaceName
	}
	settings, err := slack.CheckSettings(ctx, ContactMethodSettingsInput{Fields: map[string]string{"slack_workspace_id": slack.WorkspaceID, "slack_conversation_id": event.Channel, "slack_conversation_kind": kind, "slack_conversation_description": description}})
	if err != nil {
		return err
	}
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	if a.configuredContactMethodTypes()["slack_message"] != slack {
		return ErrConflict
	}
	now := a.Now()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var memberID, connection string
		var expires, consumed int64
		err := tx.QueryRowContext(ctx, `SELECT member_id,slack_connection,expires_at,consumed_at FROM slack_connection_codes WHERE code_hash=?`, tokenHash(code)).Scan(&memberID, &connection, &expires, &consumed)
		if errors.Is(err, sql.ErrNoRows) {
			return errSlackCodeUnusable
		}
		if err != nil {
			return err
		}
		if expires <= now.Unix() || consumed != 0 {
			return errSlackCodeUnusable
		}
		private := event.Type == "message" && event.ChannelType == "im" && strings.HasPrefix(event.Channel, "D")
		channel := shared && (strings.HasPrefix(event.Channel, "C") || strings.HasPrefix(event.Channel, "G"))
		if (connection == "shared_channel" && !channel) || (connection == "personal_direct_message" && !private) {
			return errSlackWrongConversation
		}
		var role string
		if err = tx.QueryRowContext(ctx, `SELECT role FROM members WHERE id=? AND removed_at IS NULL`, memberID).Scan(&role); errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		} else if err != nil {
			return err
		}
		if shared && role != RoleAdministrator {
			return ErrForbidden
		}
		owner := memberID
		if shared {
			owner = ""
		}
		if err = a.requireSendingServiceSetupTx(ctx, tx, slack); err != nil {
			return err
		}
		var existing int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE deleted=0 AND contact_method_type='slack_message' AND member_id=? AND settings_json=?`, owner, settings.JSON).Scan(&existing); err != nil {
			return err
		}
		if existing == 0 {
			actor, err := incidentMemberName(ctx, tx, memberID)
			if err != nil {
				return err
			}
			if _, err = createContactMethodTx(ctx, tx, slack, owner, settings, true, slack.Facts().defaultSubscriptions(shared), actor, now); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE slack_connection_codes SET consumed_at=? WHERE code_hash=?`, now.Unix(), tokenHash(code)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO slack_user_links(slack_workspace_id,slack_user_id,member_id,linked_at) VALUES(?,?,?,?) ON CONFLICT(slack_workspace_id,slack_user_id) DO UPDATE SET member_id=excluded.member_id,linked_at=excluded.linked_at`, slack.WorkspaceID, event.User, memberID, now.Unix())
		return err
	})
}

func (a *App) handleSlackEvent(ctx context.Context, slack *SlackMessageContactMethodType, event slackEvent) error {
	if event.Subtype != "" || event.BotID != "" || event.User == slack.BotUserID {
		return nil
	}
	text := strings.TrimSpace(event.Text)
	if event.Type == "app_mention" {
		mention := "<@" + slack.BotUserID + ">"
		if !strings.HasPrefix(text, mention) {
			return nil
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, mention))
	} else if event.Type != "message" {
		return nil
	}
	if !slackConnectionCodePattern.MatchString(text) {
		return nil
	}
	err := a.connectSlackConversation(ctx, slack, event, text)
	reply := "Slack is connected. Alerts will arrive in this conversation."
	switch {
	case errors.Is(err, errSlackCodeUnusable):
		reply = "The Slack connection code expired or was already used. Choose Connect Slack under Contact methods for a new code."
	case errors.Is(err, errSlackWrongConversation):
		reply = "Send a personal connection code in a direct message to the MMHotline app. For a shared channel code, mention the MMHotline app with the code in the Slack channel you want to connect."
	case err != nil:
		return err
	}
	return slack.callJSON(ctx, "chat.postMessage", slack.BotToken, map[string]string{"channel": event.Channel, "text": reply}, nil)
}

// openSocketURL asks Slack for a short-lived connection address. The address
// contains a secret and must never appear in errors or logs.
func (c *SlackMessageContactMethodType) openSocketURL(ctx context.Context) (string, error) {
	var response struct {
		URL string `json:"url"`
	}
	if err := c.callJSON(ctx, "apps.connections.open", c.AppLevelToken, struct{}{}, &response); err != nil {
		return "", err
	}
	parsed, err := url.Parse(response.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "wss" && !(c.BaseURL != "" && parsed.Scheme == "ws")) {
		return "", errors.New("Slack did not return a WebSocket address")
	}
	return response.URL, nil
}

// RunSlackSocketMode follows saved setup and never overlaps two connections.
func (a *App) RunSlackSocketMode(ctx context.Context, report func(error)) {
	var lastReport time.Time
	reportLimited := func(err error) {
		if ctx.Err() == nil && (lastReport.IsZero() || time.Since(lastReport) >= 10*time.Minute) {
			report(err)
			lastReport = time.Now()
		}
	}
	for ctx.Err() == nil {
		a.contactMethodTypesMu.RLock()
		slack, _ := a.ContactMethodTypes["slack_message"].(*SlackMessageContactMethodType)
		changed := a.contactMethodTypesChanged
		a.contactMethodTypesMu.RUnlock()
		socketCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		if slack != nil {
			go func() { defer close(done); a.runSlackSocketMode(socketCtx, slack, reportLimited) }()
		} else {
			close(done)
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
		cancel()
		<-done
	}
}
func (a *App) runSlackSocketMode(ctx context.Context, slack *SlackMessageContactMethodType, report func(error)) {
	pause := time.Second
	for ctx.Err() == nil {
		err := a.slackSocketConnection(ctx, slack, report)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			report(fmt.Errorf("Slack Socket Mode: %w", err))
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		pause = min(pause*2, 30*time.Second)
	}
}

type slackSocketEnvelope struct {
	EnvelopeID     string          `json:"envelope_id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	ConnectionInfo struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"`
}

func (a *App) slackSocketConnection(ctx context.Context, slack *SlackMessageContactMethodType, report func(error)) error {
	connection, err := slack.dialSocket(ctx)
	if err != nil {
		return err
	}
	connection.SetReadLimit(1 << 20)
	readCtx, cancelRead := context.WithCancel(ctx)
	// A bounded queue lets the reader acknowledge the next envelope while a
	// sending service reply is waiting, without creating an unbounded worker per press.
	envelopes := make(chan slackSocketEnvelope, 32)
	readDone := make(chan error, 1)
	go func() { readDone <- readSlackEnvelopes(readCtx, connection, envelopes) }()
	// The receive waits for the reader to end; the loop below returns why the
	// connection ended.
	//lint:ignore S1005 The loop's own error is the result; explicitly discard the reader's error for errcheck.
	defer func() { cancelRead(); connection.CloseNow(); _ = <-readDone }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case envelope, open := <-envelopes:
			if !open {
				return errors.New("the Slack WebSocket closed or sent an unreadable envelope")
			}
			if envelope.Type == "disconnect" {
				return nil
			}
			if envelope.Type == "hello" && envelope.ConnectionInfo.AppID != slack.AppID {
				return errors.New("the Slack app-level token belongs to a different app")
			}
			handleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			reportPanic(report, "Slack envelope handling", func() { err = a.handleSlackEnvelope(handleCtx, slack, envelope) })
			cancel()
			if err != nil && ctx.Err() == nil {
				report(fmt.Errorf("Slack envelope handling failed: %w", err))
			}
		}
	}
}
func readSlackEnvelopes(ctx context.Context, connection *websocket.Conn, envelopes chan<- slackSocketEnvelope) error {
	defer close(envelopes)
	for {
		_, data, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		var envelope slackSocketEnvelope
		if json.Unmarshal(data, &envelope) != nil {
			return errors.New("unreadable Slack envelope")
		}
		if envelope.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": envelope.EnvelopeID})
			ackCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
			err = connection.Write(ackCtx, websocket.MessageText, ack)
			cancel()
			if err != nil {
				return err
			}
		}
		select {
		case envelopes <- envelope:
		case <-ctx.Done():
			return ctx.Err()
		}
		if envelope.Type == "disconnect" {
			return nil
		}
	}
}
func (a *App) handleSlackEnvelope(ctx context.Context, slack *SlackMessageContactMethodType, envelope slackSocketEnvelope) error {
	switch envelope.Type {
	case "events_api":
		var payload struct {
			TeamID string     `json:"team_id"`
			AppID  string     `json:"api_app_id"`
			Event  slackEvent `json:"event"`
		}
		if json.Unmarshal(envelope.Payload, &payload) == nil && payload.TeamID == slack.WorkspaceID && payload.AppID == slack.AppID {
			return a.handleSlackEvent(ctx, slack, payload.Event)
		}
	case "interactive":
		var press slackInteraction
		if json.Unmarshal(envelope.Payload, &press) == nil && press.Type == "block_actions" && press.AppID == slack.AppID {
			return a.handleSlackButtonPress(ctx, slack, press)
		}
	}
	return nil
}
