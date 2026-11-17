package fakesendingservices

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The fake Telegram Bot API lives at {fake}/telegram/bot{token}/{method}. It
// implements the calls MMHotline makes (getMe, sendMessage,
// editMessageReplyMarkup, answerCallbackQuery, getUpdates) and lets tests and
// the fake sending services page play a Telegram user: press Start on a link, add
// the bot to a group, and press a button.
// Docs: https://core.telegram.org/bots/api

// TelegramBotUsername is the fake bot's username, answered by getMe.
const TelegramBotUsername = "mmhotline_fake_bot"

const telegramBotUserID = 7000000001

// Bot API objects, with only the fields MMHotline uses.
// Docs: https://core.telegram.org/bots/api#available-types
type TelegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}
type TelegramChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
}
type TelegramButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}
type TelegramKeyboard struct {
	InlineKeyboard [][]TelegramButton `json:"inline_keyboard"`
}
type TelegramMessage struct {
	MessageID   int64             `json:"message_id"`
	From        *TelegramUser     `json:"from,omitempty"`
	Chat        TelegramChat      `json:"chat"`
	Date        int64             `json:"date"`
	Text        string            `json:"text"`
	ReplyMarkup *TelegramKeyboard `json:"reply_markup,omitempty"`
}
type TelegramCallbackQuery struct {
	ID           string           `json:"id"`
	From         TelegramUser     `json:"from"`
	Message      *TelegramMessage `json:"message,omitempty"`
	ChatInstance string           `json:"chat_instance"`
	Data         string           `json:"data"`
}
type TelegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *TelegramMessage       `json:"message,omitempty"`
	CallbackQuery *TelegramCallbackQuery `json:"callback_query,omitempty"`
}

// TelegramCallbackAnswer is one answerCallbackQuery call: the text the
// person who pressed the button sees.
type TelegramCallbackAnswer struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text"`
	ShowAlert       bool   `json:"show_alert"`
}

type telegramState struct {
	nextUpdateID, nextMessageID, nextQueryID int64
	// updates holds the updates no getUpdates offset has confirmed yet.
	updates []TelegramUpdate
	// updatesArrived is closed and replaced whenever an update is added, to
	// wake a waiting getUpdates.
	updatesArrived chan struct{}
	messages       []TelegramMessage
	answers        []TelegramCallbackAnswer
	chats          map[int64]TelegramChat
	openQueries    map[string]bool
}

func newTelegramState() telegramState {
	return telegramState{nextUpdateID: 1, nextMessageID: 1, updatesArrived: make(chan struct{}), chats: map[int64]TelegramChat{}, openQueries: map[string]bool{}}
}

// addTelegramUpdate must be called with s.mu held.
func (s *Server) addTelegramUpdate(u TelegramUpdate) {
	u.UpdateID = s.telegram.nextUpdateID
	s.telegram.nextUpdateID++
	s.telegram.updates = append(s.telegram.updates, u)
	close(s.telegram.updatesArrived)
	s.telegram.updatesArrived = make(chan struct{})
}

// TelegramStart adds the message Telegram sends the bot when a person presses
// Start on https://t.me/{bot}?start={token} (a private chat), or adds the bot
// to a group through ?startgroup={token}. In a group the command names the
// bot: "/start@{bot} {token}". A private chat's ID is the user's ID.
// Docs: https://core.telegram.org/bots/features#deep-linking
func (s *Server) TelegramStart(user TelegramUser, chat TelegramChat, startToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text := "/start " + startToken
	if chat.Type != "private" {
		text = "/start@" + TelegramBotUsername + " " + startToken
	}
	s.telegram.chats[chat.ID] = chat
	s.addTelegramUpdate(TelegramUpdate{Message: &TelegramMessage{MessageID: s.newTelegramMessageID(), From: &user, Chat: chat, Date: time.Now().Unix(), Text: text}})
}

// TelegramPressButton adds the callback_query Telegram sends when a person
// presses an inline keyboard button, and returns the callback query ID.
// Docs: https://core.telegram.org/bots/api#callbackquery
func (s *Server) TelegramPressButton(user TelegramUser, chatID, messageID int64, callbackData string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := &TelegramMessage{MessageID: messageID, Chat: TelegramChat{ID: chatID}}
	for _, m := range s.telegram.messages {
		if m.Chat.ID == chatID && m.MessageID == messageID {
			message = &m
		}
	}
	id := strconv.FormatInt(s.telegram.nextQueryID+900000, 10)
	s.telegram.nextQueryID++
	s.telegram.openQueries[id] = true
	s.addTelegramUpdate(TelegramUpdate{CallbackQuery: &TelegramCallbackQuery{ID: id, From: user, Message: message, ChatInstance: strconv.FormatInt(chatID, 10), Data: callbackData}})
	return id
}

// TelegramMessages returns every message the bot sent, oldest first, with
// its current buttons.
func (s *Server) TelegramMessages() []TelegramMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TelegramMessage(nil), s.telegram.messages...)
}

// TelegramCallbackAnswers returns every answerCallbackQuery, oldest first.
func (s *Server) TelegramCallbackAnswers() []TelegramCallbackAnswer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TelegramCallbackAnswer(nil), s.telegram.answers...)
}

func (s *Server) newTelegramMessageID() int64 {
	id := s.telegram.nextMessageID
	s.telegram.nextMessageID++
	return id
}

// telegramAnswer writes the Bot API's answer shape: {"ok": true, "result": ...}
// or {"ok": false, "error_code": ..., "description": ...}.
// Docs: https://core.telegram.org/bots/api#making-requests
func telegramAnswer(w http.ResponseWriter, result any) {
	writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}
func telegramRefuse(w http.ResponseWriter, status int, description string) {
	writeJSON(w, status, map[string]any{"ok": false, "error_code": status, "description": description})
}

// telegramBotAPI routes POST {fake}/telegram/bot{token}/{method}. MMHotline
// sends JSON bodies.
// Docs: https://core.telegram.org/bots/api#making-requests
func (s *Server) telegramBotAPI(w http.ResponseWriter, r *http.Request) {
	bot := r.PathValue("bot")
	token := strings.TrimPrefix(bot, "bot")
	// The legacy fake token remains convenient for development. Numbered tokens
	// let tests revoke a token and replace a bot through the same fake API.
	botID := int64(telegramBotUserID)
	switch token {
	case "fake-telegram-bot-token", "fake-token", "7000000001:fake_bot_token_abcdefghij", "7000000001:rotated_bot_token_abcdefg":
	case "7000000002:other_bot_token_abcdefgh":
		botID++
	default:
		telegramRefuse(w, 401, "Unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		telegramRefuse(w, 400, "Bad Request: could not read the request")
		return
	}
	decode := func(v any) bool {
		if json.Unmarshal(body, v) != nil {
			telegramRefuse(w, 400, "Bad Request: can't parse JSON")
			return false
		}
		return true
	}
	switch r.PathValue("method") {
	case "getMe":
		// Docs: https://core.telegram.org/bots/api#getme
		telegramAnswer(w, TelegramUser{ID: botID, IsBot: true, FirstName: "MMHotline (fake)", Username: TelegramBotUsername})
	case "sendMessage":
		var request struct {
			ChatID      int64             `json:"chat_id"`
			Text        string            `json:"text"`
			ReplyMarkup *TelegramKeyboard `json:"reply_markup"`
		}
		if decode(&request) {
			s.telegramSendMessage(w, request.ChatID, request.Text, request.ReplyMarkup)
		}
	case "editMessageReplyMarkup":
		var request struct {
			ChatID      int64             `json:"chat_id"`
			MessageID   int64             `json:"message_id"`
			ReplyMarkup *TelegramKeyboard `json:"reply_markup"`
		}
		if decode(&request) {
			s.telegramEditMessageReplyMarkup(w, request.ChatID, request.MessageID, request.ReplyMarkup)
		}
	case "answerCallbackQuery":
		var request TelegramCallbackAnswer
		if decode(&request) {
			s.telegramAnswerCallbackQuery(w, request)
		}
	case "getUpdates":
		var request struct {
			Offset  int64 `json:"offset"`
			Limit   int   `json:"limit"`
			Timeout int   `json:"timeout"`
		}
		if decode(&request) {
			s.telegramGetUpdates(w, r, request.Offset, request.Limit, request.Timeout)
		}
	default:
		telegramRefuse(w, 404, "Not Found")
	}
}

// telegramKeyboardProblem checks an inline keyboard the way Telegram does:
// each button has a text and one action, and callback_data is 1 to 64 bytes.
// Docs: https://core.telegram.org/bots/api#inlinekeyboardbutton
func telegramKeyboardProblem(k *TelegramKeyboard) string {
	if k == nil {
		return ""
	}
	for _, row := range k.InlineKeyboard {
		for _, b := range row {
			if b.Text == "" {
				return "Bad Request: text buttons are unallowed in the inline keyboard"
			}
			if (b.CallbackData == "") == (b.URL == "") {
				return "Bad Request: BUTTON_TYPE_INVALID"
			}
			if b.URL == "" && len(b.CallbackData) > 64 {
				return "Bad Request: BUTTON_DATA_INVALID"
			}
		}
	}
	return ""
}

// telegramSendMessage imitates sendMessage. The bot can write only to a chat
// that started it or a group it was added to.
// Docs: https://core.telegram.org/bots/api#sendmessage
func (s *Server) telegramSendMessage(w http.ResponseWriter, chatID int64, text string, markup *TelegramKeyboard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, known := s.telegram.chats[chatID]
	switch {
	case !known:
		telegramRefuse(w, 400, "Bad Request: chat not found")
		return
	case strings.TrimSpace(text) == "":
		telegramRefuse(w, 400, "Bad Request: message text is empty")
		return
	case utf8.RuneCountInString(text) > 4096:
		telegramRefuse(w, 400, "Bad Request: message is too long")
		return
	}
	if problem := telegramKeyboardProblem(markup); problem != "" {
		telegramRefuse(w, 400, problem)
		return
	}
	message := TelegramMessage{MessageID: s.newTelegramMessageID(), From: &TelegramUser{ID: telegramBotUserID, IsBot: true, FirstName: "MMHotline (fake)", Username: TelegramBotUsername}, Chat: chat, Date: time.Now().Unix(), Text: text, ReplyMarkup: markup}
	s.telegram.messages = append(s.telegram.messages, message)
	s.record(Sent{SendingService: "telegram", IDFromSendingService: fmt.Sprintf("%d:%d", chatID, message.MessageID), From: "@" + TelegramBotUsername, To: strconv.FormatInt(chatID, 10), Text: text})
	telegramAnswer(w, message)
}

// telegramEditMessageReplyMarkup imitates editMessageReplyMarkup, including
// Telegram's refusal of an edit that changes nothing.
// Docs: https://core.telegram.org/bots/api#editmessagereplymarkup
func (s *Server) telegramEditMessageReplyMarkup(w http.ResponseWriter, chatID, messageID int64, markup *TelegramKeyboard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if problem := telegramKeyboardProblem(markup); problem != "" {
		telegramRefuse(w, 400, problem)
		return
	}
	for i, m := range s.telegram.messages {
		if m.Chat.ID != chatID || m.MessageID != messageID {
			continue
		}
		before, _ := json.Marshal(m.ReplyMarkup)
		after, _ := json.Marshal(markup)
		if bytes.Equal(before, after) {
			telegramRefuse(w, 400, "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message")
			return
		}
		s.telegram.messages[i].ReplyMarkup = markup
		telegramAnswer(w, s.telegram.messages[i])
		return
	}
	telegramRefuse(w, 400, "Bad Request: message to edit not found")
}

// telegramAnswerCallbackQuery imitates answerCallbackQuery. A query can be
// answered once; the text is at most 200 characters.
// Docs: https://core.telegram.org/bots/api#answercallbackquery
func (s *Server) telegramAnswerCallbackQuery(w http.ResponseWriter, answer TelegramCallbackAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.telegram.openQueries[answer.CallbackQueryID] {
		telegramRefuse(w, 400, "Bad Request: query is too old and response timeout expired or query ID is invalid")
		return
	}
	if utf8.RuneCountInString(answer.Text) > 200 {
		telegramRefuse(w, 400, "Bad Request: MESSAGE_TOO_LONG")
		return
	}
	delete(s.telegram.openQueries, answer.CallbackQueryID)
	s.telegram.answers = append(s.telegram.answers, answer)
	telegramAnswer(w, true)
}

// telegramGetUpdates imitates getUpdates with long polling. Calling it with
// an offset confirms, and forgets, every update below that offset; the others
// are sent again until confirmed. With a timeout it waits up to that many
// seconds for an update.
// Docs: https://core.telegram.org/bots/api#getupdates
func (s *Server) telegramGetUpdates(w http.ResponseWriter, r *http.Request, offset int64, limit, timeoutSeconds int) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	deadline := time.Now().Add(time.Duration(min(max(timeoutSeconds, 0), 50)) * time.Second)
	for {
		s.mu.Lock()
		kept := s.telegram.updates[:0]
		for _, u := range s.telegram.updates {
			if u.UpdateID >= offset {
				kept = append(kept, u)
			}
		}
		s.telegram.updates = kept
		ready := append([]TelegramUpdate{}, kept[:min(len(kept), limit)]...)
		arrived := s.telegram.updatesArrived
		s.mu.Unlock()
		wait := time.Until(deadline)
		if len(ready) > 0 || wait <= 0 {
			telegramAnswer(w, ready)
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-arrived:
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

// actionFields reads a fake-api action's fields from a JSON object (Linux
// suites) or a form (the fake sending services page).
func actionFields(r *http.Request) (map[string]string, bool) {
	fields := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var raw map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&raw) != nil {
			return nil, false
		}
		for k, v := range raw {
			fields[k] = fmt.Sprint(v)
		}
		return fields, true
	}
	if r.ParseForm() != nil {
		return nil, false
	}
	for k := range r.PostForm {
		fields[k] = r.PostForm.Get(k)
	}
	return fields, true
}

func actionDone(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		w.WriteHeader(204)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func telegramActionUser(fields map[string]string) (TelegramUser, bool) {
	id, err := strconv.ParseInt(fields["telegram_user_id"], 10, 64)
	name := fields["telegram_user_first_name"]
	if name == "" {
		name = "Barbara"
	}
	return TelegramUser{ID: id, FirstName: name}, err == nil && id > 0
}

// telegramStartAction is POST /fake-api/actions/telegram/start with
// telegram_user_id, telegram_user_first_name, start_token, and, to add the bot
// to a group, telegram_group_chat_id and telegram_group_title.
func (s *Server) telegramStartAction(w http.ResponseWriter, r *http.Request) {
	fields, ok := actionFields(r)
	user, userOK := telegramActionUser(fields)
	if !ok || !userOK || fields["start_token"] == "" {
		http.Error(w, "send telegram_user_id, start_token, and optionally telegram_group_chat_id and telegram_group_title", 400)
		return
	}
	chat := TelegramChat{ID: user.ID, Type: "private", FirstName: user.FirstName}
	if fields["telegram_group_chat_id"] != "" {
		id, err := strconv.ParseInt(fields["telegram_group_chat_id"], 10, 64)
		if err != nil || id >= 0 {
			http.Error(w, "telegram_group_chat_id must be a negative number, as Telegram group IDs are", 400)
			return
		}
		chat = TelegramChat{ID: id, Type: "group", Title: fields["telegram_group_title"]}
	}
	s.TelegramStart(user, chat, fields["start_token"])
	actionDone(w, r)
}

// telegramPressAction is POST /fake-api/actions/telegram/press with
// telegram_user_id, telegram_user_first_name, chat_id, message_id, and
// callback_data.
func (s *Server) telegramPressAction(w http.ResponseWriter, r *http.Request) {
	fields, ok := actionFields(r)
	user, userOK := telegramActionUser(fields)
	chatID, err1 := strconv.ParseInt(fields["chat_id"], 10, 64)
	messageID, err2 := strconv.ParseInt(fields["message_id"], 10, 64)
	if !ok || !userOK || err1 != nil || err2 != nil || fields["callback_data"] == "" {
		http.Error(w, "send telegram_user_id, chat_id, message_id, and callback_data", 400)
		return
	}
	s.TelegramPressButton(user, chatID, messageID, fields["callback_data"])
	actionDone(w, r)
}

// telegramLinkStandIn is GET {fake}/telegram/t.me/{bot}?start=... or
// ?startgroup=..., standing in for the t.me link the Telegram app opens.
func (s *Server) telegramLinkStandIn(w http.ResponseWriter, r *http.Request) {
	token, group := r.URL.Query().Get("start"), false
	if token == "" {
		token, group = r.URL.Query().Get("startgroup"), true
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = telegramLinkPage(r.PathValue("bot"), token, group).Render(r.Context(), w)
}
