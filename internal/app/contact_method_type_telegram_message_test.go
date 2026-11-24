package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"mmhotline/internal/fakesendingservices"
)

// telegramRig is a team whose Telegram contact method type talks to the fake Telegram
// Bot API. Its administrator is "Responder".
type telegramRig struct {
	a        *App
	s        personFixture
	fake     *fakesendingservices.Server
	telegram *TelegramMessageContactMethodType
	advance  func(time.Duration)
	offset   int64
}

var (
	barbara  = fakesendingservices.TelegramUser{ID: 1001, FirstName: "Barbara"}
	stranger = fakesendingservices.TelegramUser{ID: 2002, FirstName: "Stranger"}
)

func barbaraChat() fakesendingservices.TelegramChat {
	return fakesendingservices.TelegramChat{ID: barbara.ID, Type: "private", FirstName: "Barbara"}
}
func opsGroup() fakesendingservices.TelegramChat {
	return fakesendingservices.TelegramChat{ID: -1001, Type: "group", Title: "Ops alerts"}
}

func newTelegramRig(t *testing.T) *telegramRig {
	t.Helper()
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	a := foundationApp(t)
	advance := deliveryTestClock(a)
	a.WebURL = "https://mmhotline.example.com"
	fixture := memberFixture(t, a, "telegram@example.test")
	if err := a.LoadSendingServiceSetups(context.Background(), nil, server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetUpSendingService(context.Background(), fixture.Principal, "telegram", map[string]string{"telegram_bot_token": "fake-telegram-bot-token"}); err != nil {
		t.Fatal(err)
	}
	telegram := a.configuredContactMethodTypes()["telegram_message"].(*TelegramMessageContactMethodType)
	return &telegramRig{a: a, s: fixture, fake: fake, telegram: telegram, advance: advance}
}

// poll handles the waiting updates once, keeping the offset like the poller.
func (r *telegramRig) poll(t *testing.T) {
	t.Helper()
	next, err := r.a.pollTelegramOnce(context.Background(), r.telegram, r.offset, 0, func(err error) { t.Errorf("reported: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	r.offset = next
}

func (r *telegramRig) dispatch(t *testing.T) {
	t.Helper()
	if _, err := r.a.DispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *telegramRig) startToken(t *testing.T, p Principal, connection string) string {
	t.Helper()
	link, err := r.a.StartTelegramConnection(context.Background(), p, connection)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if connection == "team_group" {
		return u.Query().Get("startgroup")
	}
	return u.Query().Get("start")
}

// connect makes a start link and plays the Telegram user pressing Start.
func (r *telegramRig) connect(t *testing.T, p Principal, connection string, user fakesendingservices.TelegramUser, chat fakesendingservices.TelegramChat) ContactMethod {
	t.Helper()
	r.fake.TelegramStart(user, chat, r.startToken(t, p, connection))
	r.poll(t)
	finishSaveTest(t, r.a, p, r.a.ContactMethodTypes["telegram_message"])
	return r.telegramMethod(t, p, chat.ID)
}

func (r *telegramRig) telegramMethod(t *testing.T, p Principal, chatID int64) ContactMethod {
	t.Helper()
	methods, err := r.a.ContactMethods(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var found []ContactMethod
	for _, c := range methods {
		if c.ContactMethodType == "telegram_message" && c.Setting("telegram_chat_id") == fmt.Sprint(chatID) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("telegram contact methods for chat %d: %+v", chatID, found)
	}
	return found[0]
}

// lastMessageTo returns the newest bot message in a chat.
func (r *telegramRig) lastMessageTo(t *testing.T, chatID int64) fakesendingservices.TelegramMessage {
	t.Helper()
	messages := r.fake.TelegramMessages()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Chat.ID == chatID {
			return messages[i]
		}
	}
	t.Fatalf("no message in chat %d", chatID)
	return fakesendingservices.TelegramMessage{}
}

func (r *telegramRig) lastAnswer(t *testing.T) fakesendingservices.TelegramCallbackAnswer {
	t.Helper()
	answers := r.fake.TelegramCallbackAnswers()
	if len(answers) == 0 {
		t.Fatal("no button answer")
	}
	return answers[len(answers)-1]
}

// press plays user pressing the button with this text on a message.
func (r *telegramRig) press(t *testing.T, user fakesendingservices.TelegramUser, message fakesendingservices.TelegramMessage, buttonText string) {
	t.Helper()
	if message.ReplyMarkup == nil {
		t.Fatalf("message has no buttons: %+v", message)
	}
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		for _, b := range row {
			if b.Text == buttonText {
				r.fake.TelegramPressButton(user, message.Chat.ID, message.MessageID, b.CallbackData)
				r.poll(t)
				return
			}
		}
	}
	t.Fatalf("no %q button on %+v", buttonText, message.ReplyMarkup)
}

func (r *telegramRig) sos(t *testing.T, title string) (Alert, fakesendingservices.TelegramMessage) {
	t.Helper()
	e, err := r.a.ManualSOS(context.Background(), r.s.Principal, title, "Connection refused")
	if err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	return e, r.lastMessageTo(t, barbara.ID)
}

func buttonTexts(message fakesendingservices.TelegramMessage) []string {
	var texts []string
	if message.ReplyMarkup != nil {
		for _, row := range message.ReplyMarkup.InlineKeyboard {
			for _, b := range row {
				texts = append(texts, b.Text)
			}
		}
	}
	return texts
}

// addMember adds a person through the application operation and returns their principal.
func addMember(t *testing.T, a *App, name, role string) (Principal, Member) {
	t.Helper()
	m := addPerson(t, a, NewID()+"@example.test", name, role)
	return Principal{SignInEmailAddress: m.SignInEmailAddress}, m
}

func incidentHistory(t *testing.T, a *App, p Principal, id string) []string {
	t.Helper()
	actions, err := a.IncidentAudit(context.Background(), p, id)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, action := range actions {
		messages = append(messages, action.Message)
	}
	return messages
}

func TestTelegramConnectsAMemberChatAndATeamGroup(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	link, err := r.a.StartTelegramConnection(ctx, r.s.Principal, "personal_chat")
	if err != nil || !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/telegram/t\.me/mmhotline_fake_bot\?start=[A-Za-z0-9_-]{43}$`).MatchString(link) {
		t.Fatalf("link %q %v", link, err)
	}
	token := link[strings.Index(link, "=")+1:]
	var stored string
	if err = r.a.Store.db.QueryRow(`SELECT token_hash FROM telegram_start_tokens`).Scan(&stored); err != nil || stored != tokenHash(token) {
		t.Fatalf("start token is not stored hashed: %q %v", stored, err)
	}
	r.fake.TelegramStart(barbara, barbaraChat(), token)
	r.poll(t)
	personal := r.telegramMethod(t, r.s.Principal, barbara.ID)
	member, _ := r.a.CurrentMember(ctx, r.s.Principal)
	if personal.MemberID != member.ID || personal.SentTo != "Telegram chat with Barbara" || strings.Join(personal.Subscriptions, ",") != "sos,warn" || !personal.SendWhileUnavailableOrMuted {
		t.Fatalf("personal chat: %+v", personal)
	}
	if ds := deliveriesOf(t, r.a, personal.ID); len(ds) != 1 || ds[0].Purpose != "contact_method_test" {
		t.Fatalf("new Telegram contact test: %+v", ds)
	}
	if got := r.lastMessageTo(t, barbara.ID).Text; got != telegramConnectedText {
		t.Fatalf("bot reply %q", got)
	}
	var linked string
	if err = r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT member_id FROM telegram_user_links WHERE telegram_user_id=?`, barbara.ID).Scan(&linked)
	}); err != nil || linked != member.ID {
		t.Fatalf("Telegram user link: %q %v", linked, err)
	}

	group := r.connect(t, r.s.Principal, "team_group", barbara, opsGroup())
	// A person's own chat leaves Info off; a team group gets every severity.
	if group.MemberID != "" || group.SentTo != "Telegram group “Ops alerts”" || strings.Join(group.Subscriptions, ",") != "info,sos,warn" {
		t.Fatalf("group: %+v", group)
	}
	if got := r.lastMessageTo(t, opsGroup().ID).Text; got != telegramGroupConnectedText {
		t.Fatalf("group reply %q", got)
	}

	// Only administrators may add the bot to a group.
	memberPrincipal, _ := addMember(t, r.a, "Backup", RoleMember)
	if _, err = r.a.StartTelegramConnection(ctx, memberPrincipal, "team_group"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member made a group link: %v", err)
	}
	// Nobody adds a Telegram contact method by typing a chat ID.
	if _, err = r.a.SaveContactMethod(ctx, r.s.Principal, ContactMethodInput{ContactMethodType: "telegram_message", Fields: map[string]string{"telegram_chat_id": "999", "telegram_chat_description": "x"}, Subscriptions: []string{"sos"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("typed chat accepted: %v", err)
	}
	// Saving the card changes subscriptions but never the chat.
	saved, err := r.a.SaveContactMethod(ctx, r.s.Principal, ContactMethodInput{ID: personal.ID, ContactMethodType: "telegram_message", Fields: map[string]string{"telegram_chat_id": "999"}, ExpectedRevision: personal.Revision, Subscriptions: []string{"sos"}})
	if err != nil || saved.Setting("telegram_chat_id") != "1001" || strings.Join(saved.Subscriptions, ",") != "sos" {
		t.Fatalf("saved card: %+v %v", saved, err)
	}
}

func TestTelegramStartTokenIsSingleUseAndExpires(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	token := r.startToken(t, r.s.Principal, "personal_chat")
	// A personal link used in a group changes nothing and stays usable.
	r.fake.TelegramStart(barbara, opsGroup(), token)
	r.poll(t)
	if got := r.lastMessageTo(t, opsGroup().ID).Text; got != telegramPersonalLinkInGroup {
		t.Fatalf("personal link in a group: %q", got)
	}
	r.fake.TelegramStart(barbara, barbaraChat(), token)
	r.poll(t)
	r.telegramMethod(t, r.s.Principal, barbara.ID)
	// The same /start again from the same chat is harmless.
	r.fake.TelegramStart(barbara, barbaraChat(), token)
	r.poll(t)
	r.telegramMethod(t, r.s.Principal, barbara.ID)
	// Another chat cannot use it.
	strangerChat := fakesendingservices.TelegramChat{ID: stranger.ID, Type: "private", FirstName: "Stranger"}
	r.fake.TelegramStart(stranger, strangerChat, token)
	r.poll(t)
	if got := r.lastMessageTo(t, stranger.ID).Text; got != telegramStartTokenUnusable {
		t.Fatalf("second use: %q", got)
	}
	methods, _ := r.a.ContactMethods(ctx, r.s.Principal)
	if len(methods) != 1 {
		t.Fatalf("contact methods: %+v", methods)
	}
	// An expired link does nothing.
	expiring := r.startToken(t, r.s.Principal, "personal_chat")
	r.advance(telegramStartTokenLifetime)
	r.fake.TelegramStart(stranger, strangerChat, expiring)
	r.poll(t)
	if methods, _ = r.a.ContactMethods(ctx, r.s.Principal); len(methods) != 1 || r.lastMessageTo(t, stranger.ID).Text != telegramStartTokenUnusable {
		t.Fatalf("expired link used: %+v", methods)
	}
	// A group link in a private chat, an unknown token, and a bare /start.
	r.fake.TelegramStart(stranger, strangerChat, r.startToken(t, r.s.Principal, "team_group"))
	r.poll(t)
	if got := r.lastMessageTo(t, stranger.ID).Text; got != telegramGroupLinkInPrivate {
		t.Fatalf("group link in a private chat: %q", got)
	}
	r.fake.TelegramStart(stranger, strangerChat, "not-a-real-token")
	r.poll(t)
	if got := r.lastMessageTo(t, stranger.ID).Text; got != telegramStartTokenUnusable {
		t.Fatalf("unknown token: %q", got)
	}
	r.fake.TelegramStart(stranger, strangerChat, "")
	r.poll(t)
	if got := r.lastMessageTo(t, stranger.ID).Text; got != telegramIntroText {
		t.Fatalf("bare /start: %q", got)
	}
	// At most 5 unused links per member.
	for range telegramUnusedStartTokensPerMember - 1 {
		r.startToken(t, r.s.Principal, "personal_chat")
	}
	if _, err := r.a.StartTelegramConnection(ctx, r.s.Principal, "personal_chat"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth unused link: %v", err)
	}
}

func TestTelegramSOSMessageButtonsClaimAsTheLinkedMember(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	c := r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	e, message := r.sos(t, "Database down")
	if strings.Join(buttonTexts(message), ",") != "Claim this incident (assign it to me),Resolve" || !strings.Contains(message.Text, "https://mmhotline.example.com/incidents/"+e.IncidentID) {
		t.Fatalf("SOS message: %+v", message)
	}
	for _, row := range message.ReplyMarkup.InlineKeyboard {
		for _, b := range row {
			if len(b.CallbackData) > 64 {
				t.Fatalf("callback_data too long: %q", b.CallbackData)
			}
		}
	}
	deliveries, _ := r.a.IncidentDeliveries(ctx, r.s.Principal, e.IncidentID)
	if len(deliveries) != 1 || deliveries[0].ContactMethodID != c.ID || deliveries[0].IDFromSendingService != fmt.Sprintf("%d:%d", barbara.ID, message.MessageID) || deliveries[0].OutcomeLabel() != "Sent to Telegram" {
		t.Fatalf("delivery: %+v", deliveries)
	}
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
	member, _ := r.a.CurrentMember(ctx, r.s.Principal)
	if i.Status != "claimed" || i.OwnerMemberID != member.ID {
		t.Fatalf("incident: %+v", i)
	}
	if history := incidentHistory(t, r.a, r.s.Principal, e.IncidentID); history[len(history)-1] != "Claimed by Responder in Telegram" {
		t.Fatalf("history: %v", history)
	}
	after := r.lastMessageTo(t, barbara.ID)
	if after.MessageID != message.MessageID || strings.Join(buttonTexts(after), ",") != "Claimed by Responder,Resolve" || after.ReplyMarkup.InlineKeyboard[0][0].URL != "https://mmhotline.example.com/incidents/"+e.IncidentID {
		t.Fatalf("buttons after the press: %+v", after.ReplyMarkup)
	}
	if answer := r.lastAnswer(t); answer.Text != "Claimed by Responder" || answer.ShowAlert {
		t.Fatalf("answer: %+v", answer)
	}
	// Pressing again on another copy says it is already theirs.
	r.fake.TelegramPressButton(barbara, message.Chat.ID, message.MessageID, message.ReplyMarkup.InlineKeyboard[0][0].CallbackData)
	r.poll(t)
	if answer := r.lastAnswer(t); answer.Text != "Claimed by Responder" {
		t.Fatalf("second press: %+v", answer)
	}
	if history := incidentHistory(t, r.a, r.s.Principal, e.IncidentID); strings.Count(strings.Join(history, "\n"), "Claimed by") != 1 {
		t.Fatalf("claimed twice: %v", history)
	}
}

func TestTelegramResolveButtonStaysAfterAClaimAndResolvesFromThatMessage(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	e, message := r.sos(t, "Database down")
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	claimed := r.lastMessageTo(t, barbara.ID)
	if strings.Join(buttonTexts(claimed), ",") != "Claimed by Responder,Resolve" {
		t.Fatalf("buttons after claim: %v", buttonTexts(claimed))
	}
	keyboard := claimed.ReplyMarkup.InlineKeyboard[0]
	if keyboard[0].URL == "" || keyboard[1].URL != "" || keyboard[1].CallbackData != message.ReplyMarkup.InlineKeyboard[0][1].CallbackData || len(keyboard[1].CallbackData) > 64 {
		t.Fatalf("kept Resolve button: %+v", keyboard)
	}
	r.press(t, barbara, claimed, "Resolve")
	i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
	if i.Status != "resolved" {
		t.Fatalf("incident: %+v", i)
	}
	if got := strings.Join(buttonTexts(r.lastMessageTo(t, barbara.ID)), ","); got != "Resolved by Responder" {
		t.Fatalf("buttons after resolve: %q", got)
	}
}

func TestTelegramPressFromAnUnknownAccountOrTheWrongMessage(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	e, message := r.sos(t, "Database down")
	r.press(t, stranger, message, "Claim this incident (assign it to me)")
	answer := r.lastAnswer(t)
	if answer.Text != "MMHotline does not know your Telegram account. Connect the account under Contact methods, or claim the incident in MMHotline: https://mmhotline.example.com/incidents/"+e.IncidentID || !answer.ShowAlert {
		t.Fatalf("unknown account: %+v", answer)
	}
	claim := message.ReplyMarkup.InlineKeyboard[0][0].CallbackData
	// Made-up data: the real data with the first character of its random
	// delivery ID changed, so it is well formed but names no delivery here.
	madeUp := []byte(claim)
	madeUp[2] = 'A'
	if claim[2] == 'A' {
		madeUp[2] = 'B'
	}
	// The right button data on another message, in another chat, or made up.
	for _, press := range []struct {
		chatID, messageID int64
		data              string
	}{
		{message.Chat.ID, message.MessageID + 1, claim},
		{opsGroup().ID, message.MessageID, claim},
		{message.Chat.ID, message.MessageID, "c:bad:data"},
		{message.Chat.ID, message.MessageID, string(madeUp)},
	} {
		r.fake.TelegramPressButton(barbara, press.chatID, press.messageID, press.data)
		r.poll(t)
		if answer = r.lastAnswer(t); answer.Text != telegramButtonNotUsableText {
			t.Fatalf("press %+v: %+v", press, answer)
		}
	}
	if i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID); i.Status != "open" {
		t.Fatalf("incident changed: %+v", i)
	}
	if strings.Join(buttonTexts(r.lastMessageTo(t, barbara.ID)), ",") != "Claim this incident (assign it to me),Resolve" {
		t.Fatal("buttons changed")
	}
	// A linked Telegram user whose member left is unknown.
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE telegram_user_links SET member_id=?`, NewID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	if answer = r.lastAnswer(t); !strings.HasPrefix(answer.Text, "MMHotline does not know your Telegram account.") {
		t.Fatalf("former member: %+v", answer)
	}
}

func TestTelegramNeverTakesOverAndReportsResolved(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	backup, _ := addMember(t, r.a, "Backup", RoleMember)
	e, message := r.sos(t, "Database down")
	i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
	if _, err := r.a.ClaimIncident(ctx, backup, i.ID, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	if answer := r.lastAnswer(t); answer.Text != "Already claimed by Backup" {
		t.Fatalf("answer: %+v", answer)
	}
	if got := buttonTexts(r.lastMessageTo(t, barbara.ID)); strings.Join(got, ",") != "Claimed by Backup,Resolve" {
		t.Fatalf("buttons: %v", got)
	}
	if after, _ := r.a.GetIncident(ctx, r.s.Principal, i.ID); after.OwnerMemberID == i.OwnerMemberID || after.Status != "claimed" || after.Revision != i.Revision+1 {
		t.Fatalf("taken over: %+v", after)
	}

	e, message = r.sos(t, "Disk full")
	i, _ = r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
	if _, err := r.a.ResolveIncident(ctx, backup, i.ID, i.Revision, ""); err != nil {
		t.Fatal(err)
	}
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	if answer := r.lastAnswer(t); answer.Text != "This incident is already resolved." {
		t.Fatalf("answer: %+v", answer)
	}
	if got := buttonTexts(r.lastMessageTo(t, barbara.ID)); strings.Join(got, ",") != "Resolved" {
		t.Fatalf("buttons: %v", got)
	}
}

func TestTelegramResolveButton(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	// A responder who can be woken: otherwise the notice that MMHotline
	// could not wake them joins the chat's messages.
	giveWakingContactMethod(t, r.a, "telegram@example.test")
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	e, message := r.sos(t, "Database down")
	r.press(t, barbara, message, "Resolve")
	if i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID); i.Status != "resolved" {
		t.Fatalf("incident: %+v", i)
	}
	if history := incidentHistory(t, r.a, r.s.Principal, e.IncidentID); history[len(history)-1] != "Resolved by Responder in Telegram" {
		t.Fatalf("history: %v", history)
	}
	if answer := r.lastAnswer(t); answer.Text != "Resolved by Responder" {
		t.Fatalf("answer: %+v", answer)
	}
	if got := buttonTexts(r.lastMessageTo(t, barbara.ID)); strings.Join(got, ",") != "Resolved by Responder" {
		t.Fatalf("buttons: %v", got)
	}
	// The resolved message that follows is a Warning-like message: no buttons.
	r.advance(time.Minute)
	r.dispatch(t)
	if resolved := r.lastMessageTo(t, barbara.ID); !strings.HasPrefix(resolved.Text, "Resolved SOS") || resolved.ReplyMarkup != nil {
		t.Fatalf("resolved message: %+v", resolved)
	}
}

func TestTelegramSOSChatRuleAfterTenInTenMinutes(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	// A responder who can be woken: otherwise each SOS adds the notice that
	// MMHotline could not wake them to the chat's messages.
	giveWakingContactMethod(t, r.a, "telegram@example.test")
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	var incidents []string
	for n := range 12 {
		e, err := r.a.ManualSOS(ctx, r.s.Principal, fmt.Sprintf("SOS %d", n), "")
		if err != nil {
			t.Fatal(err)
		}
		incidents = append(incidents, e.IncidentID)
		r.advance(10 * time.Second)
	}
	for range 12 {
		r.dispatch(t)
	}
	messages := r.fake.TelegramMessages()[1:] // after the "connected" reply
	if len(messages) != 11 {
		t.Fatalf("messages: %d", len(messages))
	}
	for _, m := range messages[:10] {
		if strings.Join(buttonTexts(m), ",") != "Claim this incident (assign it to me),Resolve" {
			t.Fatalf("one-by-one SOS without buttons: %+v", m)
		}
	}
	summary := messages[10]
	if summary.Text != "2 more SOS incidents. Open MMHotline: https://mmhotline.example.com/dashboard" || summary.ReplyMarkup != nil {
		t.Fatalf("summary: %+v", summary)
	}
	var summaryID string
	deliveries, _ := r.a.Deliveries(ctx, r.s.Principal, 50)
	for _, d := range deliveries {
		if d.MoreSOSIncidentsMessage {
			summaryID = d.ID
		}
		if d.IncidentID == incidents[11] && d.ContactMethodType == "telegram_message" && (d.State != "combined" || d.Reason != "Sent together with 1 other alert") {
			t.Fatalf("12th SOS: %+v", d)
		}
	}
	// A later SOS in the window waits at most a minute for the next summary,
	// never longer.
	sentAt := r.a.Now()
	if _, err := r.a.ManualSOS(ctx, r.s.Principal, "SOS 12", ""); err != nil {
		t.Fatal(err)
	}
	// The newest Telegram delivery: the waking contact method's are made in
	// the same second.
	deliveries, _ = r.a.Deliveries(ctx, r.s.Principal, 10)
	deliveries = slices.DeleteFunc(deliveries, func(d Delivery) bool { return d.ContactMethodType != "telegram_message" })
	if !deliveries[0].MoreSOSIncidentsMessage || !deliveries[0].NextAttempt.Equal(sentAt.Add(time.Minute)) || !strings.HasPrefix(deliveries[0].Reason, "Waiting until ") {
		t.Fatalf("second summary: %+v", deliveries[0])
	}
	// The summary is about the SOS incidents it stands for; the shared claim
	// operation claims all of them.
	member, _ := r.a.CurrentMember(ctx, r.s.Principal)
	results, err := r.a.ClaimIncidentsFromNotification(ctx, summaryID, member.ID, "telegram_button")
	if err != nil || len(results) != 2 || results[0].IncidentID != incidents[10] || results[1].IncidentID != incidents[11] || results[1].Text != "Claimed by Responder" {
		t.Fatalf("claim from the summary: %+v %v", results, err)
	}
	// After 10 minutes, SOS messages go one by one again.
	r.advance(chatSOSMessagesWindow)
	if _, err := r.a.ManualSOS(ctx, r.s.Principal, "SOS 13", ""); err != nil {
		t.Fatal(err)
	}
	deliveries, _ = r.a.Deliveries(ctx, r.s.Principal, 10)
	deliveries = slices.DeleteFunc(deliveries, func(d Delivery) bool { return d.ContactMethodType != "telegram_message" })
	if deliveries[0].MoreSOSIncidentsMessage || deliveries[0].State != "queued" || !deliveries[0].NextAttempt.Equal(r.a.Now()) {
		t.Fatalf("after the window: %+v", deliveries[0])
	}
}

func TestTelegramCombinedMessagesHaveNoButtons(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	r.connect(t, r.s.Principal, "team_group", barbara, opsGroup())
	for n := range 3 {
		queueDelivery(t, r.a, r.s.Principal, fmt.Sprintf("warning-%d", n), "warn")
	}
	for range 3 {
		r.dispatch(t)
	}
	combined := r.lastMessageTo(t, opsGroup().ID)
	if !strings.HasPrefix(combined.Text, "MMHotline combined 3 alerts") || combined.ReplyMarkup != nil {
		t.Fatalf("combined: %+v", combined)
	}
}

func TestTelegramSendTestAndSendFailures(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	c := r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	r.advance(time.Minute)
	if err := r.a.SendContactMethodTest(ctx, r.s.Principal, c.ID); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	test := r.lastMessageTo(t, barbara.ID)
	if !strings.Contains(test.Text, "This is a test of this contact method, not an incident.") || test.ReplyMarkup != nil {
		t.Fatalf("test message: %+v", test)
	}
	deliveries, _ := r.a.Deliveries(ctx, r.s.Principal, 1)
	if deliveries[0].Purpose != "contact_method_test" || deliveries[0].OutcomeLabel() != "Sent to Telegram" {
		t.Fatalf("test delivery: %+v", deliveries[0])
	}
	// A chat the bot cannot write to is refused for good.
	result := r.telegram.Send(ctx, ContactMethodSend{Settings: `{"telegram_chat_id":"424242"}`, Text: "hello"})
	if result.State != "failed" || result.Retryable || result.Diagnostic != "Refused by Telegram (HTTP 400): Bad Request: chat not found" {
		t.Fatalf("unknown chat: %+v", result)
	}
	// No answer is an unknown outcome, retried: a repeated chat message is
	// harmless.
	gone := &TelegramMessageContactMethodType{BotToken: "t", APIBaseURL: "http://127.0.0.1:1/telegram"}
	if result = gone.Send(ctx, ContactMethodSend{Settings: `{"telegram_chat_id":"1001"}`, Text: "hello"}); result.State != "uncertain" || !result.Retryable || strings.Contains(result.Diagnostic, "bot") {
		t.Fatalf("no answer: %+v", result)
	}
	if !gone.Facts().RetryUncertain {
		t.Fatal("Telegram must retry an unknown outcome")
	}
}

func TestTelegramPollerOffsetAndRepeatedUpdates(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	if r.offset != 2 || r.telegram.LastSuccessfulGetUpdates() != r.a.Now() {
		t.Fatalf("offset %d, last successful getUpdates %s", r.offset, r.telegram.LastSuccessfulGetUpdates())
	}
	e, message := r.sos(t, "Database down")
	r.press(t, barbara, message, "Claim this incident (assign it to me)")
	// A poller that lost its offset gets every unconfirmed update again;
	// handling them twice changes nothing.
	unconfirmed := r.offset - 1
	next, err := r.a.pollTelegramOnce(ctx, r.telegram, unconfirmed, 0, func(err error) { t.Errorf("reported: %v", err) })
	if err != nil || next != r.offset {
		t.Fatalf("repeat: %d %v", next, err)
	}
	methods, _ := r.a.ContactMethods(ctx, r.s.Principal)
	history := incidentHistory(t, r.a, r.s.Principal, e.IncidentID)
	if len(methods) != 1 || strings.Count(strings.Join(history, "\n"), "Claimed by") != 1 {
		t.Fatalf("repeated update changed something: %+v %v", methods, history)
	}
	// Polling from the confirmed offset returns nothing new.
	if next, err = r.a.pollTelegramOnce(ctx, r.telegram, r.offset, 0, nil); err != nil || next != r.offset {
		t.Fatalf("confirmed: %d %v", next, err)
	}
}

func TestTelegramPollerStopsCleanlyAndRestarts(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	var reportsMu sync.Mutex
	var reports []error
	report := func(err error) {
		reportsMu.Lock()
		defer reportsMu.Unlock()
		reports = append(reports, err)
	}
	run := func() (context.CancelFunc, chan struct{}) {
		pollCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); r.a.RunTelegramPolling(pollCtx, report) }()
		return cancel, done
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	stop := func(cancel context.CancelFunc, done chan struct{}) {
		t.Helper()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the poller did not stop while waiting in getUpdates")
		}
	}
	cancel, done := run()
	r.fake.TelegramStart(barbara, barbaraChat(), r.startToken(t, r.s.Principal, "personal_chat"))
	waitFor("the chat to connect", func() bool {
		methods, _ := r.a.ContactMethods(ctx, r.s.Principal)
		return len(methods) == 1
	})
	stop(cancel, done)
	// A new poller starts from offset 0 and sees the unconfirmed /start again.
	e, message := r.sos(t, "Database down")
	cancel, done = run()
	r.fake.TelegramPressButton(barbara, message.Chat.ID, message.MessageID, message.ReplyMarkup.InlineKeyboard[0][0].CallbackData)
	waitFor("the claim", func() bool {
		i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
		return i.Status == "claimed"
	})
	stop(cancel, done)
	methods, _ := r.a.ContactMethods(ctx, r.s.Principal)
	reportsMu.Lock()
	defer reportsMu.Unlock()
	if len(methods) != 1 || len(reports) != 0 {
		t.Fatalf("after restart: %+v %v", methods, reports)
	}
}

func TestTelegramPollingFailureIsReportedWithoutTheToken(t *testing.T) {
	t.Parallel()
	a := foundationApp(t)
	a.ContactMethodTypes = map[string]ContactMethodType{"telegram_message": &TelegramMessageContactMethodType{BotToken: "123:secret-bot-token", APIBaseURL: "http://127.0.0.1:1/telegram"}}
	ctx, cancel := context.WithCancel(context.Background())
	reported := make(chan error, 4)
	done := make(chan struct{})
	go func() { defer close(done); a.RunTelegramPolling(ctx, func(err error) { reported <- err }) }()
	err := <-reported
	cancel()
	<-done
	if !strings.Contains(err.Error(), "last successful getUpdates: never") || strings.Contains(err.Error(), "secret-bot-token") {
		t.Fatalf("report: %v", err)
	}
}

func TestClaimFromANotificationRacesTheBrowser(t *testing.T) {
	t.Parallel()
	for range 5 {
		r := newTelegramRig(t)
		ctx := context.Background()
		r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
		backup, _ := addMember(t, r.a, "Backup", RoleMember)
		member, _ := r.a.CurrentMember(ctx, r.s.Principal)
		e, _ := r.sos(t, "Database down")
		i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID)
		deliveries, _ := r.a.IncidentDeliveries(ctx, r.s.Principal, i.ID)
		var wait sync.WaitGroup
		wait.Go(func() {
			if _, err := r.a.ClaimIncident(ctx, backup, i.ID, i.Revision, ""); err != nil && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
		wait.Go(func() {
			if _, err := r.a.ClaimIncidentsFromNotification(ctx, deliveries[0].ID, member.ID, "telegram_button"); err != nil {
				t.Error(err)
			}
		})
		wait.Wait()
		history := strings.Join(incidentHistory(t, r.a, r.s.Principal, i.ID), "\n")
		if strings.Count(history, "Claimed by") != 1 || strings.Contains(history, "Taken over") {
			t.Fatalf("history: %s", history)
		}
	}
}

func TestClaimFromANotificationRules(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	member, _ := r.a.CurrentMember(ctx, r.s.Principal)
	e, _ := r.sos(t, "Database down")
	deliveries, _ := r.a.IncidentDeliveries(ctx, r.s.Principal, e.IncidentID)
	if _, err := r.a.ClaimIncidentsFromNotification(ctx, deliveries[0].ID, member.ID, "carrier_pigeon"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown claimedHow: %v", err)
	}
	if _, err := r.a.ClaimIncidentsFromNotification(ctx, deliveries[0].ID, NewID(), "telegram_button"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("not a member: %v", err)
	}
	if _, err := r.a.ClaimIncidentsFromNotification(ctx, NewID(), member.ID, "telegram_button"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown delivery: %v", err)
	}
	// A contact method type that cannot claim (a webhook) cannot be claimed from.
	var webhookDelivery string
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		webhookDelivery = NewID()
		if _, err := tx.Exec(`INSERT INTO contact_methods(id,member_id,contact_method_type,settings_json,sent_to,revision,send_while_unavailable_or_muted) VALUES('w','','outgoing_webhook','{}','https://example.com',1,1)`); err != nil {
			return err
		}
		return insertDelivery(ctx, tx, Delivery{ID: webhookDelivery, NotificationKey: "w", IncidentID: e.IncidentID, ContactMethodID: "w", ContactMethodRevision: 1, ContactMethodType: "outgoing_webhook", SentTo: "https://example.com", Purpose: "subscribed_alert", Severity: "sos", State: "accepted", CreatedAt: r.a.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.ClaimIncidentsFromNotification(ctx, webhookDelivery, member.ID, "telegram_button"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("webhook claim: %v", err)
	}
	if i, _ := r.a.GetIncident(ctx, r.s.Principal, e.IncidentID); i.Status != "open" {
		t.Fatalf("incident changed: %+v", i)
	}
}

func TestTelegramThroughTheContactMethodsPage(t *testing.T) {
	t.Parallel()
	h := newWorkflowRig(t)
	fake := fakesendingservices.New()
	server := httptest.NewServer(fake)
	defer server.Close()
	owner := h.browser()
	owner.addPerson("owner@example.com", "Alex")
	if page := owner.get("/contact-methods", 200); strings.Contains(page, "Connect Telegram") {
		t.Fatal("Connect Telegram offered without a Telegram bot")
	}
	h.app.WebURL = "https://mmhotline.example.com"
	if err := h.app.LoadSendingServiceSetups(context.Background(), h.app.configuredContactMethodTypes(), server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.SetUpSendingService(context.Background(), Principal{UnixUser: "fixture"}, "telegram", map[string]string{"telegram_bot_token": "fake-telegram-bot-token"}); err != nil {
		t.Fatal(err)
	}
	telegram := h.app.configuredContactMethodTypes()["telegram_message"].(*TelegramMessageContactMethodType)
	page := owner.get("/contact-methods", 200)
	for _, text := range []string{"Connect Telegram", "Add the MMHotline bot to a Telegram group"} {
		if !strings.Contains(page, text) {
			t.Fatalf("missing %q", text)
		}
	}
	page = owner.post("/connect-telegram", url.Values{"csrf": {workflowField(t, page, "csrf")}, "telegram_connection": {"personal_chat"}}, 200)
	link := regexp.MustCompile(`href="(http://[^"]+\?start=([A-Za-z0-9_-]+))"`).FindStringSubmatch(page)
	if len(link) != 3 || !strings.Contains(pageText(page), "The Telegram link works once, for 15 minutes.") {
		t.Fatalf("connect page: %s", page)
	}
	// The fake's t.me stand-in shows the Start button for this token.
	if response, err := server.Client().Get(strings.ReplaceAll(link[1], "&amp;", "&")); err != nil || response.StatusCode != 200 {
		t.Fatalf("t.me stand-in: %v", err)
	}
	fake.TelegramStart(barbara, barbaraChat(), link[2])
	if _, err := h.app.pollTelegramOnce(context.Background(), telegram, 0, 0, func(err error) { t.Error(err) }); err != nil {
		t.Fatal(err)
	}
	page = owner.get("/contact-methods", 200)
	if !strings.Contains(pageText(page), "Telegram message") || !strings.Contains(page, "Telegram chat with Barbara") || strings.Contains(page, `name="telegram_chat_id"`) {
		t.Fatalf("Telegram card: %s", page)
	}
	page = owner.post("/connect-telegram", url.Values{"csrf": {workflowField(t, page, "csrf")}, "telegram_connection": {"team_group"}}, 200)
	if !regexp.MustCompile(`href="http://[^"]+/telegram/t\.me/mmhotline_fake_bot\?startgroup=[A-Za-z0-9_-]{43}"`).MatchString(page) {
		t.Fatalf("group link page: %s", page)
	}
}

// Everyone in a group sees the bot's /start message; repeating it must not
// link anyone else to the member who made the link.
func TestTelegramGroupStartRepeatedByAnotherUserLinksNobody(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	token := r.startToken(t, r.s.Principal, "team_group")
	r.fake.TelegramStart(barbara, opsGroup(), token)
	r.poll(t)
	r.fake.TelegramStart(stranger, opsGroup(), token)
	r.poll(t)
	var n int
	if err := r.a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM telegram_user_links WHERE telegram_user_id=?`, stranger.ID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("the stranger was linked: %d %v", n, err)
	}
	r.telegramMethod(t, r.s.Principal, opsGroup().ID)
}

// A press forged onto the buttonless "{N} more" message does nothing.
func TestTelegramPressOnAMessageWithoutButtonsIsRefused(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	for n := range 12 {
		if _, err := r.a.ManualSOS(ctx, r.s.Principal, fmt.Sprintf("SOS %d", n), ""); err != nil {
			t.Fatal(err)
		}
		r.advance(10 * time.Second)
	}
	for range 12 {
		r.dispatch(t)
	}
	summary := r.lastMessageTo(t, barbara.ID)
	var summaryID string
	deliveries, _ := r.a.Deliveries(ctx, r.s.Principal, 50)
	for _, d := range deliveries {
		if d.MoreSOSIncidentsMessage {
			summaryID = d.ID
		}
	}
	r.fake.TelegramPressButton(barbara, summary.Chat.ID, summary.MessageID, telegramButtonData(telegramResolveAction, summaryID))
	r.poll(t)
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM incidents WHERE status='resolved'`); n != 0 {
		t.Fatalf("a forged press on the summary resolved %d incidents", n)
	}
}

func TestTelegramFollowsAGroupUpgradedToASupergroup(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	group := r.connect(t, r.s.Principal, "team_group", barbara, opsGroup())
	update := telegramUpdate{UpdateID: 99, Message: &telegramMessage{MessageID: 7, Chat: telegramChat{ID: opsGroup().ID, Type: "group"}, MigrateToChatID: -1009999}}
	if err := r.a.handleTelegramUpdate(context.Background(), r.telegram, update); err != nil {
		t.Fatal(err)
	}
	moved := r.telegramMethod(t, r.s.Principal, -1009999)
	if moved.ID != group.ID || moved.Revision != group.Revision {
		t.Fatalf("moved: %+v, was %+v", moved, group)
	}
}

func TestTelegramTextIsCutInUTF16Units(t *testing.T) {
	t.Parallel()
	if got := truncateUTF16(strings.Repeat("😀", 10), 11); got != "😀😀😀😀😀…" {
		t.Fatalf("%q", got)
	}
	if got := truncateUTF16("short", 10); got != "short" {
		t.Fatalf("%q", got)
	}
	if got := truncateUTF16(strings.Repeat("a", 10), 10); got != strings.Repeat("a", 10) {
		t.Fatalf("exact fit: %q", got)
	}
}

func TestTelegramConnectionRollsBackTokenWhenContactCreationFails(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	token := r.startToken(t, r.s.Principal, "personal_chat")
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_contact_method BEFORE INSERT ON contact_methods BEGIN SELECT RAISE(ABORT, 'fixture contact failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	chat := telegramChat{ID: barbara.ID, Type: "private", FirstName: "Barbara"}
	user := telegramUser{ID: barbara.ID, FirstName: "Barbara"}
	if _, err := r.a.connectTelegramChat(ctx, token, chat, user); err == nil {
		t.Fatal("contact creation succeeded")
	}
	if n := scalarTeam(t, r.a, `SELECT consumed_at FROM telegram_start_tokens WHERE token_hash=?`, tokenHash(token)); n != 0 {
		t.Fatal("failed connection consumed the token")
	}
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM telegram_user_links`); n != 0 {
		t.Fatal("failed connection linked a user")
	}
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM contact_methods`); n != 0 {
		t.Fatal("failed connection saved a contact method")
	}
	if err := r.a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DROP TRIGGER reject_contact_method`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.connectTelegramChat(ctx, token, chat, user); err != nil {
		t.Fatal(err)
	}
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM telegram_start_tokens WHERE token_hash=? AND consumed_at>0`, tokenHash(token)); n != 1 {
		t.Fatal("successful connection did not consume the token")
	}
	r.telegramMethod(t, r.s.Principal, chat.ID)
	if n := scalarTeam(t, r.a, `SELECT count(*) FROM telegram_user_links`); n != 1 {
		t.Fatal("successful connection did not link its user")
	}
}

// failingSummaryTelegram is Telegram refusing the "{N} more SOS incidents"
// message for good.
type failingSummaryTelegram struct {
	*TelegramMessageContactMethodType
}

func (f failingSummaryTelegram) Send(ctx context.Context, m ContactMethodSend) DeliveryResult {
	if strings.Contains(m.Text, "more SOS incident") {
		return DeliveryResult{State: "failed", Diagnostic: "Forbidden: bot was blocked by the user"}
	}
	return f.TelegramMessageContactMethodType.Send(ctx, m)
}

// When the "{N} more SOS incidents" message fails for good, the SOS messages
// combined into it are not shown as sent, in the delivery log or on their
// incident page.
func TestSOSCombinedIntoAFailedChatMessageShowsAsFailed(t *testing.T) {
	t.Parallel()
	r := newTelegramRig(t)
	ctx := context.Background()
	r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
	r.a.ContactMethodTypes["telegram_message"] = failingSummaryTelegram{r.telegram}
	var incidents []string
	for n := range 12 {
		e, err := r.a.ManualSOS(ctx, r.s.Principal, fmt.Sprintf("SOS %d", n), "")
		if err != nil {
			t.Fatal(err)
		}
		incidents = append(incidents, e.IncidentID)
		r.advance(10 * time.Second)
	}
	for range 12 {
		r.dispatch(t)
	}
	deliveries, err := r.a.IncidentDeliveries(ctx, r.s.Principal, incidents[11])
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	for _, d := range deliveries {
		if d.ContactMethodType != "telegram_message" {
			continue
		}
		joined = true
		if d.State != "failed" || d.Reason != "Not sent: the message it was combined into failed: Forbidden: bot was blocked by the user" || d.OutcomeLabel() != "Failed, no more retries" {
			t.Fatalf("12th SOS: %+v (outcome %q)", d, d.OutcomeLabel())
		}
	}
	if !joined {
		t.Fatal("the 12th SOS has no Telegram message")
	}
	notifications := liveSectionText(incidentPageAs(t, r.a, r.s.Principal, incidents[11]), "incident-deliveries")
	if strings.Contains(notifications, "Sent together") || !strings.Contains(notifications, "Failed, no more retries") || !strings.Contains(notifications, "the message it was combined into failed") {
		t.Fatalf("incident page: %s", notifications)
	}
}
