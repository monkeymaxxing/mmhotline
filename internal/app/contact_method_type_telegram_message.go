package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// TelegramMessageContactMethodType sends chat messages through the MMHotline Telegram bot
// (Bot API: https://core.telegram.org/bots/api). A member connects a private
// chat, or an administrator adds the bot to a group, by opening a t.me link
// with a single-use start token. SOS messages carry "Claim this incident" and
// "Resolve" buttons. MMHotline long-polls getUpdates for /start messages and
// button presses, so Telegram needs no inbound endpoint.
//
// APIBaseURL is https://api.telegram.org in production and the fake
// sending services' /telegram in development and tests. LinkBaseURL is https://t.me,
// or the fake sending services' /telegram/t.me stand-in.
type TelegramMessageContactMethodType struct {
	BotToken, APIBaseURL, LinkBaseURL string
	Client                            *http.Client

	mu sync.Mutex
	// botUsername is the immutable username checked and saved during setup.
	botUsername string
	// lastSuccessfulGetUpdates is kept in memory for the server administrator; polling
	// failures are reported with it.
	lastSuccessfulGetUpdates time.Time
}

const (
	telegramStartTokenLifetime = 15 * time.Minute
	// A member may hold at most 5 unused start tokens at a time.
	telegramUnusedStartTokensPerMember = 5
	telegramLongPollSeconds            = 25
	// Telegram allows 4,096 characters in a message and 200 in a button
	// press answer.
	telegramMaximumMessageRunes   = 4000
	telegramMaximumAnswerRunes    = 200
	telegramPollingRetryDelay     = 5 * time.Second
	telegramPollingReportInterval = 10 * time.Minute
	telegramClaimAction           = "c"
	telegramResolveAction         = "r"
)

// telegramTeamSchema links a Telegram user to one member of a team, so a
// button press can act as that member.
const telegramTeamSchema = `
CREATE TABLE telegram_user_links (
 telegram_user_id INTEGER PRIMARY KEY,
 member_id TEXT NOT NULL,
 linked_at INTEGER NOT NULL
);
`

// telegramStartTokenSchema stores only the SHA-256 of each single-use token.
const telegramStartTokenSchema = `
CREATE TABLE telegram_start_tokens (
 token_hash TEXT PRIMARY KEY,
 member_id TEXT NOT NULL,
 telegram_connection TEXT NOT NULL CHECK(telegram_connection IN ('personal_chat','team_group')),
 expires_at INTEGER NOT NULL,
 consumed_at INTEGER NOT NULL DEFAULT 0,
 consumed_by_telegram_chat_id INTEGER NOT NULL DEFAULT 0,
 consumed_by_telegram_user_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX telegram_start_tokens_member ON telegram_start_tokens(member_id,expires_at);
`

type telegramSettings struct {
	TelegramChatID string `json:"telegram_chat_id"`
}

func (*TelegramMessageContactMethodType) Name() string { return "telegram_message" }

// Facts: RetryUncertain is true because Telegram has no idempotency key and a
// repeated chat message is harmless. If a lost answer causes a second copy of
// an SOS message, only the copy MMHotline recorded has working buttons; the
// other answers that the button does not belong to its message.
func (*TelegramMessageContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{
		CardTitle: "Telegram message", CardTitleIsAProductName: true, ValueLabel: "Telegram chat", ValueField: "telegram_chat_id",
		ConnectsThroughSendingService: true, ClaimMethod: "button", CombinesMessages: true,
		RetryUncertain: true, AllowedForTeamAccount: true, AcceptedLabel: "Sent to Telegram",
	}
}

// CheckSettings validates the chat the bot was connected to. People never
// enter these fields: only connecting through the bot sets them.
func (*TelegramMessageContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	id, err := strconv.ParseInt(input.Fields["telegram_chat_id"], 10, 64)
	description := strings.TrimSpace(input.Fields["telegram_chat_description"])
	if err != nil || id == 0 || description == "" || !utf8.ValidString(description) || strings.IndexFunc(description, unicode.IsControl) >= 0 {
		return ContactMethodSettings{}, fmt.Errorf("%w: connect Telegram with its button on the Contact methods page", ErrInvalid)
	}
	settings, err := settingsJSON(telegramSettings{TelegramChatID: strconv.FormatInt(id, 10)})
	return ContactMethodSettings{JSON: settings, SentTo: truncateRunes(description, 200)}, err
}

// Bot API types: only the fields MMHotline reads or sends.
// https://core.telegram.org/bots/api#available-types
type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}
type telegramChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}
type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat  `json:"chat"`
	Text      string        `json:"text"`
	// MigrateToChatID is set when Telegram upgraded this group to a
	// supergroup, which has a new chat ID.
	MigrateToChatID int64 `json:"migrate_to_chat_id"`
}
type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}
type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}
type telegramButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}
type telegramKeyboard struct {
	InlineKeyboard [][]telegramButton `json:"inline_keyboard"`
}

// telegramAPIError is an answer from Telegram that refused the request.
type telegramAPIError struct {
	HTTPStatusCode int
	Description    string
}

func (e *telegramAPIError) Error() string {
	return fmt.Sprintf("Telegram answered HTTP %d: %s", e.HTTPStatusCode, e.Description)
}

// errTelegramNoAnswer means the outcome is unknown. Its text never includes
// the request URL, which holds the bot token.
var errTelegramNoAnswer = errors.New("no readable answer from Telegram")

// call posts one Bot API method as JSON and decodes its result.
// https://core.telegram.org/bots/api#making-requests
func (c *TelegramMessageContactMethodType) call(ctx context.Context, method string, request, result any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	base := c.APIBaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	// Every call is bounded, so one hung connection can never stop the poller.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/bot"+c.BotToken+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not construct the Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{}
	if c.Client != nil {
		client = *c.Client
	}
	// A redirect must never move the bot token in the URL elsewhere.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errTelegramNoAnswer
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return errTelegramNoAnswer
	}
	var answer struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if json.Unmarshal(data, &answer) != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return errTelegramNoAnswer
		}
		return &telegramAPIError{HTTPStatusCode: resp.StatusCode, Description: "unreadable answer"}
	}
	if !answer.OK || resp.StatusCode != http.StatusOK {
		return &telegramAPIError{HTTPStatusCode: resp.StatusCode, Description: boundedDiagnostic(strings.ReplaceAll(answer.Description, c.BotToken, "[secret removed]"))}
	}
	if result != nil && json.Unmarshal(answer.Result, result) != nil {
		return errTelegramNoAnswer
	}
	return nil
}

// telegramMessageKey is the sending service ID of a sent message: its chat and
// message ID. A button press must come from exactly this message.
func telegramMessageKey(chatID, messageID int64) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(messageID, 10)
}

// telegramButtonData packs an action and a 16-byte delivery ID into 24 bytes
// of callback_data, within Telegram's 64-byte limit.
// https://core.telegram.org/bots/api#inlinekeyboardbutton
func telegramButtonData(action, deliveryID string) string {
	delivery, _ := hex.DecodeString(deliveryID)
	return action + ":" + base64.RawURLEncoding.EncodeToString(delivery)
}

func parseTelegramButtonData(data string) (action, deliveryID string, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 2 || (parts[0] != telegramClaimAction && parts[0] != telegramResolveAction) {
		return "", "", false
	}
	delivery, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(delivery) != 16 {
		return "", "", false
	}
	return parts[0], hex.EncodeToString(delivery), true
}

func (c *TelegramMessageContactMethodType) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	var settings telegramSettings
	if json.Unmarshal([]byte(m.Settings), &settings) != nil {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the Telegram settings are incomplete"}
	}
	chatID, err := strconv.ParseInt(settings.TelegramChatID, 10, 64)
	if err != nil || chatID == 0 {
		return DeliveryResult{State: "failed", Diagnostic: "Not sent: the Telegram settings are incomplete"}
	}
	// Plain text (no parse_mode), so nothing in an alert is read as markup.
	request := struct {
		ChatID      int64             `json:"chat_id"`
		Text        string            `json:"text"`
		ReplyMarkup *telegramKeyboard `json:"reply_markup,omitempty"`
	}{ChatID: chatID, Text: truncateUTF16(fitChatMessage(m.Text, m.AlertMessage, func(s string) bool { return len(utf16.Encode([]rune(s))) <= telegramMaximumMessageRunes }), telegramMaximumMessageRunes)}
	if m.ClaimButtons {
		request.ReplyMarkup = &telegramKeyboard{InlineKeyboard: [][]telegramButton{{
			{Text: "Claim this incident (assign it to me)", CallbackData: telegramButtonData(telegramClaimAction, m.DeliveryID)},
			{Text: "Resolve", CallbackData: telegramButtonData(telegramResolveAction, m.DeliveryID)},
		}}}
	}
	var sent telegramMessage
	err = c.call(ctx, "sendMessage", request, &sent)
	var refused *telegramAPIError
	switch {
	case err == nil && sent.MessageID != 0:
		return DeliveryResult{State: "accepted", IDFromSendingService: telegramMessageKey(chatID, sent.MessageID)}
	case errors.As(err, &refused) && refused.HTTPStatusCode == http.StatusTooManyRequests:
		return DeliveryResult{State: "failed", Diagnostic: "Telegram asked MMHotline to send more slowly (HTTP 429)", Retryable: true}
	case errors.As(err, &refused) && refused.HTTPStatusCode >= 500:
		return DeliveryResult{State: "uncertain", Diagnostic: fmt.Sprintf("Telegram answered HTTP %d", refused.HTTPStatusCode), Retryable: true}
	case errors.As(err, &refused):
		return DeliveryResult{State: "failed", Diagnostic: fmt.Sprintf("Refused by Telegram (HTTP %d): %s", refused.HTTPStatusCode, refused.Description)}
	}
	return DeliveryResult{State: "uncertain", Diagnostic: "No answer from Telegram", Retryable: true}
}

var telegramUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)

// LastSuccessfulGetUpdates is when polling Telegram last worked, for the
// server administrator. It is zero until the first successful getUpdates.
func (c *TelegramMessageContactMethodType) LastSuccessfulGetUpdates() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSuccessfulGetUpdates
}

// StartTelegramConnection makes a single-use start token and returns the
// Telegram link that carries it. connection is "personal_chat" (any member:
// "Connect Telegram") or "team_group" (administrators: "Add the MMHotline bot
// to a Telegram group"). The link opens https://t.me/{bot}?start={token} or
// ?startgroup={token}; the token is 43 URL-safe characters, within
// Telegram's 64 (https://core.telegram.org/bots/features#deep-linking).
func (a *App) StartTelegramConnection(ctx context.Context, p Principal, connection string) (string, error) {
	telegram, ok := a.configuredContactMethodTypes()["telegram_message"].(*TelegramMessageContactMethodType)
	if !ok {
		return "", fmt.Errorf("%w: Telegram is not set up on this MMHotline server", ErrInvalid)
	}
	if connection != "personal_chat" && connection != "team_group" {
		return "", ErrInvalid
	}
	// Check authorization before the sending service lookup, then recheck in the save transaction.
	if err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := RequireMember(ctx, tx, p, connection == "team_group")
		return err
	}); err != nil {
		return "", err
	}
	username := telegram.botUsername
	a.sendingServiceSetupMu.Lock()
	defer a.sendingServiceSetupMu.Unlock()
	if a.configuredContactMethodTypes()["telegram_message"] != telegram {
		return "", fmt.Errorf("%w: Telegram setup changed; make a new connection link", ErrConflict)
	}
	token := randomToken()
	now := a.Now()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		m, err := RequireMember(ctx, tx, p, connection == "team_group")
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM telegram_start_tokens WHERE expires_at<=?`, now.Unix()); err != nil {
			return err
		}
		var unused int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM telegram_start_tokens WHERE member_id=? AND consumed_at=0`, m.ID).Scan(&unused); err != nil {
			return err
		}
		if unused >= telegramUnusedStartTokensPerMember {
			return ErrRateLimited
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO telegram_start_tokens(token_hash,member_id,telegram_connection,expires_at) VALUES(?,?,?,?)`, tokenHash(token), m.ID, connection, now.Add(telegramStartTokenLifetime).Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	base := telegram.LinkBaseURL
	if base == "" {
		base = "https://t.me"
	}
	parameter := "start"
	if connection == "team_group" {
		parameter = "startgroup"
	}
	return strings.TrimRight(base, "/") + "/" + username + "?" + parameter + "=" + token, nil
}

var (
	errTelegramStartTokenUnusable = errors.New("the Telegram start token expired, was used, or is unknown")
	errTelegramWrongChat          = errors.New("the Telegram start token is for another kind of chat")
	errTelegramButtonNotOurs      = errors.New("the Telegram button does not belong to its message")
	telegramStartTokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// connectTelegramChat consumes the token, adds the contact method, and links
// its member atomically. A repeat from the same chat changes nothing. Only
// the Telegram user who first used the token may link their account.
func (a *App) connectTelegramChat(ctx context.Context, startToken string, chat telegramChat, from telegramUser) (string, error) {
	if !telegramStartTokenPattern.MatchString(startToken) {
		return "", errTelegramStartTokenUnusable
	}
	now := a.Now()
	contactMethodType, _ := a.contactMethodTypeFor("telegram_message")
	settings, err := contactMethodType.CheckSettings(ctx, ContactMethodSettingsInput{Fields: map[string]string{"telegram_chat_id": strconv.FormatInt(chat.ID, 10), "telegram_chat_description": telegramChatDescription(chat)}}) //nolint:nilaway // telegram_message is a contact method type MMHotline always knows.
	if err != nil {
		return "", err
	}
	var connection string
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var memberID string
		var expires, consumed, consumedBy, consumedByUser int64
		err := tx.QueryRowContext(ctx, `SELECT member_id,telegram_connection,expires_at,consumed_at,consumed_by_telegram_chat_id,consumed_by_telegram_user_id FROM telegram_start_tokens WHERE token_hash=?`, tokenHash(startToken)).Scan(&memberID, &connection, &expires, &consumed, &consumedBy, &consumedByUser)
		if errors.Is(err, sql.ErrNoRows) {
			return errTelegramStartTokenUnusable
		}
		if err != nil {
			return err
		}
		if expires <= now.Unix() || (consumed != 0 && consumedBy != chat.ID) {
			return errTelegramStartTokenUnusable
		}
		group := chat.Type == "group" || chat.Type == "supergroup"
		if (connection == "team_group") != group || (!group && chat.Type != "private") {
			return errTelegramWrongChat
		}
		var role string
		err = tx.QueryRowContext(ctx, `SELECT role FROM members WHERE id=? AND removed_at IS NULL`, memberID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && connection == "team_group" && role != RoleAdministrator) {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		if consumed == 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE telegram_start_tokens SET consumed_at=?,consumed_by_telegram_chat_id=?,consumed_by_telegram_user_id=? WHERE token_hash=? AND consumed_at=0`, now.Unix(), chat.ID, from.ID, tokenHash(startToken)); err != nil {
				return err
			}
			consumedByUser = from.ID
		}
		owner := memberID
		if connection == "team_group" {
			owner = ""
		}
		var existing int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM contact_methods WHERE deleted=0 AND contact_method_type=? AND member_id=? AND settings_json=?`, contactMethodType.Name(), owner, settings.JSON).Scan(&existing); err != nil {
			return err
		}
		if existing == 0 {
			actorName, err := incidentMemberName(ctx, tx, memberID)
			if err != nil {
				return err
			}
			if _, err = createContactMethodTx(ctx, tx, contactMethodType, owner, settings, true, contactMethodType.Facts().defaultSubscriptions(owner == ""), actorName, now); err != nil {
				return err
			}
		}
		if consumedByUser != from.ID {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO telegram_user_links(telegram_user_id,member_id,linked_at) VALUES(?,?,?) ON CONFLICT(telegram_user_id) DO UPDATE SET member_id=excluded.member_id,linked_at=excluded.linked_at`, from.ID, memberID, now.Unix())
		return err
	})
	return connection, err
}

// telegramChatDescription is the plain "Sent to" text of a connected chat.
func telegramChatDescription(chat telegramChat) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, strings.ToValidUTF8(s, "")))
	}
	if chat.Type == "group" || chat.Type == "supergroup" {
		if title := clean(chat.Title); title != "" {
			return "Telegram group “" + title + "”"
		}
		return "Telegram group"
	}
	name := clean(strings.TrimSpace(chat.FirstName + " " + chat.LastName))
	if username := clean(chat.Username); username != "" {
		if name == "" {
			return "Telegram chat with @" + username
		}
		return "Telegram chat with " + name + " (@" + username + ")"
	}
	if name == "" {
		return "Telegram chat"
	}
	return "Telegram chat with " + name
}

// RunTelegramPolling long-polls the bot's getUpdates until ctx ends. It runs
// beside the background scheduler in mmhotline serve. The
// offset is kept in memory: after a restart Telegram sends the unconfirmed
// updates again (it keeps them for 24 hours), and handling one twice is
// harmless. Failures are reported with the last successful getUpdates, at
// most once every 10 minutes while polling keeps failing.
func (a *App) RunTelegramPolling(ctx context.Context, report func(error)) {
	for ctx.Err() == nil {
		a.contactMethodTypesMu.RLock()
		telegram, _ := a.ContactMethodTypes["telegram_message"].(*TelegramMessageContactMethodType)
		changed := a.contactMethodTypesChanged
		a.contactMethodTypesMu.RUnlock()
		pollCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		if telegram != nil {
			go func() { defer close(done); a.runTelegramPolling(pollCtx, telegram, report) }()
		} else {
			close(done)
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
		cancel()
		<-done // Never overlap two getUpdates loops, even during token replacement.
	}
}

func (a *App) runTelegramPolling(ctx context.Context, telegram *TelegramMessageContactMethodType, report func(error)) {
	var offset int64
	var failingSince, lastReport time.Time
	for ctx.Err() == nil {
		next, err := a.pollTelegramOnce(ctx, telegram, offset, telegramLongPollSeconds, report)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			offset = next
			if !failingSince.IsZero() {
				report(fmt.Errorf("Telegram getUpdates works again after failing since %s", displayTime(failingSince)))
				failingSince = time.Time{}
			}
			continue
		}
		now := a.Now()
		if failingSince.IsZero() {
			failingSince = now
		}
		if lastReport.Before(failingSince) || now.Sub(lastReport) >= telegramPollingReportInterval {
			last := "never"
			if at := telegram.LastSuccessfulGetUpdates(); !at.IsZero() {
				last = displayTime(at)
			}
			report(fmt.Errorf("Telegram getUpdates is failing since %s (last successful getUpdates: %s): %w", displayTime(failingSince), last, err))
			lastReport = now
		}
		timer := time.NewTimer(telegramPollingRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollTelegramOnce asks for the updates from offset on, handles each, and
// returns the next offset. A failure to handle one update is reported and
// does not hold back the others.
// https://core.telegram.org/bots/api#getupdates
func (a *App) pollTelegramOnce(ctx context.Context, telegram *TelegramMessageContactMethodType, offset int64, waitSeconds int, report func(error)) (int64, error) {
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds)*time.Second+15*time.Second)
	// Each update is decoded on its own, so one that MMHotline cannot read is
	// reported and skipped instead of blocking the ones after it.
	var raw []json.RawMessage
	err := telegram.call(callCtx, "getUpdates", struct {
		Offset         int64    `json:"offset"`
		Limit          int      `json:"limit"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, 20, waitSeconds, []string{"message", "callback_query"}}, &raw)
	cancel()
	if err != nil {
		return offset, err
	}
	telegram.mu.Lock()
	telegram.lastSuccessfulGetUpdates = a.Now()
	telegram.mu.Unlock()
	for _, item := range raw {
		var id struct {
			UpdateID int64 `json:"update_id"`
		}
		if json.Unmarshal(item, &id) != nil || id.UpdateID < offset {
			continue
		}
		offset = id.UpdateID + 1
		var u telegramUpdate
		if err := json.Unmarshal(item, &u); err != nil {
			report(fmt.Errorf("Telegram update %d could not be read; skipped", id.UpdateID))
			continue
		}
		var err error
		reportPanic(report, fmt.Sprintf("Telegram update %d", u.UpdateID), func() { err = a.handleTelegramUpdate(ctx, telegram, u) })
		if err != nil && ctx.Err() == nil {
			report(fmt.Errorf("Telegram update %d: %w", u.UpdateID, err))
		}
	}
	return offset, nil
}

func (a *App) handleTelegramUpdate(ctx context.Context, telegram *TelegramMessageContactMethodType, u telegramUpdate) error {
	switch {
	case u.CallbackQuery != nil:
		return a.handleTelegramButtonPress(ctx, telegram, *u.CallbackQuery)
	case u.Message != nil && u.Message.MigrateToChatID != 0:
		return a.moveTelegramChat(ctx, u.Message.Chat.ID, u.Message.MigrateToChatID)
	case u.Message != nil:
		return a.handleTelegramMessage(ctx, telegram, *u.Message)
	}
	return nil
}

// moveTelegramChat follows a group that Telegram upgraded to a supergroup:
// every Telegram contact method for the old chat now sends to the new one,
// so a team's shared alerts never stop because the chat ID changed.
func (a *App) moveTelegramChat(ctx context.Context, oldChatID, newChatID int64) error {
	oldSettings, err := settingsJSON(telegramSettings{TelegramChatID: strconv.FormatInt(oldChatID, 10)})
	if err != nil {
		return err
	}
	newSettings, err := settingsJSON(telegramSettings{TelegramChatID: strconv.FormatInt(newChatID, 10)})
	if err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		// Keep the revision so waiting deliveries go to the new chat.
		_, err := tx.ExecContext(ctx, `UPDATE contact_methods SET settings_json=? WHERE contact_method_type='telegram_message' AND deleted=0 AND settings_json=?`, newSettings, oldSettings)
		return err
	})
}

// Replies from the bot. They are plain answers in the chat, not deliveries.
const (
	telegramIntroText              = "This is the MMHotline bot. To get alerts here, open Contact methods in MMHotline and press Connect Telegram."
	telegramConnectedText          = "MMHotline is connected. Your alerts will arrive in this chat."
	telegramGroupConnectedText     = "MMHotline is connected. Team alerts will arrive in this group."
	telegramStartTokenUnusable     = "This MMHotline link has expired or was already used. In MMHotline, open Contact methods and make a new link."
	telegramPersonalLinkInGroup    = "This MMHotline link is for a private chat with the bot. Open the link again and press Start in the private chat with the MMHotline bot."
	telegramGroupLinkInPrivate     = "This MMHotline link adds the bot to a group. Open the link again and choose a group."
	telegramConnectForbiddenText   = "MMHotline could not connect this chat: the person who made the link no longer has permission to connect the chat. In MMHotline, open Contact methods and make a new link."
	telegramConnectFailedText      = "MMHotline could not connect this chat. Try again in a minute."
	telegramContactMethodLimitText = "MMHotline could not connect this chat: this installation has reached its limit of contact methods."
	telegramUnknownAccountText     = "MMHotline does not know your Telegram account. Connect the account under Contact methods, or claim the incident in MMHotline"
	telegramButtonNotUsableText    = "MMHotline cannot use this button. Open MMHotline to see the incident."
	telegramButtonPressFailedText  = "MMHotline could not apply the incident action. Try again, or open the incident in MMHotline."
)

// handleTelegramMessage answers /start. Every other message is ignored.
func (a *App) handleTelegramMessage(ctx context.Context, telegram *TelegramMessageContactMethodType, message telegramMessage) error {
	fields := strings.Fields(message.Text)
	if len(fields) == 0 || message.From == nil || message.From.IsBot {
		return nil
	}
	// In a group the command may name the bot: /start@mmhotline_bot TOKEN.
	command, bot, _ := strings.Cut(fields[0], "@")
	if command != "/start" {
		return nil
	}
	if bot != "" {
		if !strings.EqualFold(bot, telegram.botUsername) {
			return nil
		}
	}
	reply := func(text string) {
		_ = telegram.call(ctx, "sendMessage", struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}{message.Chat.ID, text}, nil)
	}
	if len(fields) < 2 {
		if message.Chat.Type == "private" {
			reply(telegramIntroText)
		}
		return nil
	}
	connection, err := a.connectTelegramChat(ctx, fields[1], message.Chat, *message.From)
	switch {
	case err == nil && connection == "team_group":
		reply(telegramGroupConnectedText)
	case err == nil:
		reply(telegramConnectedText)
	case errors.Is(err, errTelegramStartTokenUnusable):
		reply(telegramStartTokenUnusable)
	case errors.Is(err, errTelegramWrongChat) && connection == "team_group":
		reply(telegramGroupLinkInPrivate)
	case errors.Is(err, errTelegramWrongChat):
		reply(telegramPersonalLinkInGroup)
	case errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound):
		reply(telegramConnectForbiddenText)
	case errors.Is(err, ErrRateLimited):
		reply(telegramContactMethodLimitText)
	default:
		reply(telegramConnectFailedText)
		return err
	}
	return nil
}

// telegramButtonPress is what one press did.
type telegramButtonPress struct {
	incidentURL string
	unknownUser bool
	results     []NotificationActionResult
}

// pressTelegramButton checks that the press came from the message the
// delivery sent and from a Telegram user linked to a current member, then
// claims or resolves through the shared notification operation.
func (a *App) pressTelegramButton(ctx context.Context, deliveryID, action string, chatID, messageID, telegramUserID int64) (telegramButtonPress, error) {
	var press telegramButtonPress
	var memberID string
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var contactMethodType, idFromSendingService, incidentID string
		err := tx.QueryRowContext(ctx, `SELECT contact_method_type,id_from_sending_service,incident_id FROM deliveries WHERE id=?`, deliveryID).Scan(&contactMethodType, &idFromSendingService, &incidentID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if contactMethodType != "telegram_message" || incidentID == "" || idFromSendingService != telegramMessageKey(chatID, messageID) {
			return errTelegramButtonNotOurs
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
			return errTelegramButtonNotOurs
		}
		press.incidentURL = strings.TrimRight(a.WebURL, "/") + "/incidents/" + incidentID
		err = tx.QueryRowContext(ctx, `SELECT l.member_id FROM telegram_user_links l JOIN members m ON m.id=l.member_id WHERE m.removed_at IS NULL AND l.telegram_user_id=?`, telegramUserID).Scan(&memberID)
		if errors.Is(err, sql.ErrNoRows) {
			press.unknownUser = true
			return nil
		}
		return err
	})
	if err != nil || press.unknownUser {
		return press, err
	}
	if action == telegramResolveAction {
		press.results, err = a.ResolveIncidentsFromNotification(ctx, deliveryID, memberID, "telegram_button")
	} else {
		press.results, err = a.ClaimIncidentsFromNotification(ctx, deliveryID, memberID, "telegram_button")
	}
	if err == nil && len(press.results) == 0 {
		err = ErrNotFound
	}
	return press, err
}

// handleTelegramButtonPress answers a press on "Claim this incident" or
// "Resolve" and, after a claim or resolve, replaces the message's buttons
// with one that shows the result and opens the incident. After a claim,
// Resolve stays beside it.
// https://core.telegram.org/bots/api#answercallbackquery
// https://core.telegram.org/bots/api#editmessagereplymarkup
func (a *App) handleTelegramButtonPress(ctx context.Context, telegram *TelegramMessageContactMethodType, q telegramCallbackQuery) error {
	answer := func(text string, showAlert bool) {
		_ = telegram.call(ctx, "answerCallbackQuery", struct {
			CallbackQueryID string `json:"callback_query_id"`
			Text            string `json:"text"`
			ShowAlert       bool   `json:"show_alert"`
		}{q.ID, truncateRunes(text, telegramMaximumAnswerRunes), showAlert}, nil)
	}
	action, deliveryID, ok := parseTelegramButtonData(q.Data)
	if !ok || q.Message == nil {
		answer(telegramButtonNotUsableText, true)
		return nil
	}
	press, err := a.pressTelegramButton(ctx, deliveryID, action, q.Message.Chat.ID, q.Message.MessageID, q.From.ID)
	switch {
	case errors.Is(err, errTelegramButtonNotOurs) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden):
		answer(telegramButtonNotUsableText, true)
		return nil
	case err != nil:
		answer(telegramButtonPressFailedText, true)
		return err
	case press.unknownUser:
		text := telegramUnknownAccountText + ": " + press.incidentURL
		if utf8.RuneCountInString(text) > telegramMaximumAnswerRunes {
			text = telegramUnknownAccountText + "."
		}
		answer(text, true)
		return nil
	}
	result := press.results[0]
	label := "Resolved"
	switch {
	case result.Status == "claimed":
		label = "Claimed by " + result.OwnerName
	case strings.HasPrefix(result.Text, "Resolved by "):
		label = result.Text
	}
	// After a claim the Resolve button stays, so the person who claimed can
	// resolve from the chat without signing in. Its press carries the same
	// callback_data as the original Resolve button.
	buttons := []telegramButton{{Text: label, URL: press.incidentURL}}
	if result.Status == "claimed" {
		buttons = append(buttons, telegramButton{Text: "Resolve", CallbackData: telegramButtonData(telegramResolveAction, deliveryID)})
	}
	// Telegram refuses an edit that changes nothing ("message is not
	// modified"), which happens when a press is handled twice; that is fine.
	_ = telegram.call(ctx, "editMessageReplyMarkup", struct {
		ChatID      int64            `json:"chat_id"`
		MessageID   int64            `json:"message_id"`
		ReplyMarkup telegramKeyboard `json:"reply_markup"`
	}{q.Message.Chat.ID, q.Message.MessageID, telegramKeyboard{InlineKeyboard: [][]telegramButton{buttons}}}, nil)
	texts := make([]string, 0, len(press.results))
	for _, r := range press.results {
		texts = append(texts, r.Text)
	}
	answer(strings.Join(texts, " "), false)
	return nil
}

// truncateUTF16 shortens text to at most n UTF-16 code units, the unit
// Telegram counts a message's length in, so characters outside the basic
// plane (most emoji) cannot push a message over the limit.
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > n-1 {
			if len(utf16.Encode([]rune(s))) <= n {
				return s
			}
			return s[:i] + "…"
		}
		units += width
	}
	return s
}

var telegramBotTokenPattern = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,100}$`)

func (*TelegramMessageContactMethodType) Setup() SendingServiceSetupDefinition {
	return SendingServiceSetupDefinition{
		Name: "telegram", Title: "Telegram",
		Fields: []SendingServiceSetupField{{Name: "telegram_bot_token", SettingsKey: "bot_token", Label: "Telegram bot token", Secret: true, Help: "The token BotFather gives you."}},
		Guide: []SendingServiceSetupStep{
			{Text: "Open BotFather in Telegram and send /newbot. Choose a name and username for your team's bot.", URL: "https://t.me/BotFather", LinkLabel: "Open BotFather"},
			{Text: "Put the Telegram bot token in the settings file, as shown below, and apply the settings file. MMHotline checks the token with Telegram before saving the token. To replace a revoked token, use the new token for the existing bot."},
			{Text: "Connect your own Telegram chat under Contact methods. Saving the contact method sends a test message."},
		},
		NextStep:        "Connect your own Telegram chat under Contact methods. Saving the contact method sends a test message.",
		IdentitySetting: "telegram_bot_id",
		IdentityLabel:   "Telegram bot",
		DescribeCheck: func(settings map[string]any) string {
			return "Checked with Telegram: bot @" + fmt.Sprint(settings["telegram_bot_username"])
		},
		Check: checkTelegramSetup,
		Build: buildTelegramSetup,
		ClearConnections: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM telegram_start_tokens`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM telegram_user_links`)
			return err
		},
	}
}
func buildTelegramSetup(settings map[string]any, secrets map[string]string, fakeURL string) ContactMethodType {
	telegram := &TelegramMessageContactMethodType{BotToken: secrets["telegram_bot_token"]}
	telegram.botUsername, _ = settings["telegram_bot_username"].(string)
	if fakeURL != "" {
		telegram.APIBaseURL = fakeURL + "/telegram"
		telegram.LinkBaseURL = fakeURL + "/telegram/t.me"
	}
	return telegram
}
func checkTelegramSetup(ctx context.Context, fields map[string]string, fakeURL string) (sendingServiceSetupCheck, error) {
	token := fields["telegram_bot_token"]
	if !telegramBotTokenPattern.MatchString(token) && !(fakeURL != "" && token == "fake-telegram-bot-token") {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Telegram bot token must be the token from BotFather (digits, a colon, and URL-safe characters)", ErrInvalid)
	}
	telegram := buildTelegramSetup(nil, map[string]string{"telegram_bot_token": token}, fakeURL).(*TelegramMessageContactMethodType)
	var me telegramUser
	if err := telegram.call(ctx, "getMe", struct{}{}, &me); err != nil {
		// Even an unexpected sending service refusal must never echo the submitted token.
		reason := strings.ReplaceAll(err.Error(), token, "[secret removed]")
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Checked with Telegram: %s", ErrInvalid, reason)
	}
	if !me.IsBot || me.ID <= 0 || !telegramUsernamePattern.MatchString(me.Username) {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Telegram getMe did not return a bot ID and username", ErrInvalid)
	}
	if digits, _, ok := strings.Cut(token, ":"); ok && digits != strconv.FormatInt(me.ID, 10) {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: Telegram getMe returned a different bot ID from the token", ErrInvalid)
	}
	return sendingServiceSetupCheck{Settings: map[string]any{"telegram_bot_username": me.Username, "telegram_bot_id": me.ID}, SecretSettings: map[string]string{"telegram_bot_token": token}}, nil
}
