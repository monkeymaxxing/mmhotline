package app

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"mmhotline/internal/fakesendingservices"
)

// The chat targets fuzz what Telegram and Slack send MMHotline about a press
// of an SOS message's buttons. Each input runs against a new team with one
// SOS message sent, so a failing input reproduces on its own. The seeds are
// the updates and envelopes the services send, with placeholders that each
// input replaces with that message's values: $CLAIM and $RESOLVE are the two
// buttons' data, the other placeholders name the message, its conversation,
// and the linked chat user. The rule both targets check
// (docs/changing-mmhotline/design/notifications.md): a press claims or resolves the incident
// only when it comes from the message the delivery sent, carries that
// delivery's button, and comes from the chat user linked to a current
// member; every other input leaves the incident open.

// FuzzTelegramUpdate decodes one getUpdates item as the poller does and
// handles it. Callback data is compared by what it says, an action and a
// delivery ID in unpadded base64url, as telegramButtonData packs them.
func FuzzTelegramUpdate(f *testing.F) {
	press := func(from, messageID, data string) string {
		return `{"update_id":10,"callback_query":{"id":"900000","from":{"id":` + from + `,"is_bot":false,"first_name":"Barbara"},"message":{"message_id":` + messageID + `,"chat":{"id":$CHAT,"type":"private","first_name":"Barbara"},"date":1759147200,"text":"SOS: Database down"},"chat_instance":"1001","data":"` + data + `"}}`
	}
	for _, seed := range []string{
		press("$USER", "$MESSAGE", "$CLAIM"),
		press("$USER", "$MESSAGE", "$RESOLVE"),
		press("2002", "$MESSAGE", "$CLAIM"),
		press("$USER", "99999", "$CLAIM"),
		press("$USER", "$MESSAGE", "c:bad:data"),
		`{"update_id":11,"message":{"message_id":5,"from":{"id":$USER,"is_bot":false,"first_name":"Barbara"},"chat":{"id":$CHAT,"type":"private","first_name":"Barbara"},"date":1759147200,"text":"/start"}}`,
		`{"update_id":12,"message":{"message_id":6,"from":{"id":$USER,"is_bot":false,"first_name":"Barbara"},"chat":{"id":-1001,"type":"group","title":"Ops alerts"},"date":1759147200,"text":"/start@mmhotline_fake_bot unknown-token"}}`,
		`{"update_id":13,"message":{"message_id":7,"chat":{"id":$CHAT,"type":"group"},"migrate_to_chat_id":-1009999}}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, update string) {
		r := newTelegramRig(t)
		r.connect(t, r.s.Principal, "personal_chat", barbara, barbaraChat())
		alert, message := r.sos(t, "Database down")
		deliveries, err := r.a.IncidentDeliveries(context.Background(), r.s.Principal, alert.IncidentID)
		if err != nil || len(deliveries) != 1 {
			t.Fatalf("deliveries: %+v, %v", deliveries, err)
		}
		buttons := message.ReplyMarkup.InlineKeyboard[0]
		raw := strings.NewReplacer("$CLAIM", buttons[0].CallbackData, "$RESOLVE", buttons[1].CallbackData,
			"$CHAT", strconv.FormatInt(message.Chat.ID, 10), "$MESSAGE", strconv.FormatInt(message.MessageID, 10),
			"$USER", strconv.FormatInt(barbara.ID, 10)).Replace(update)
		var u telegramUpdate
		if json.Unmarshal([]byte(raw), &u) != nil {
			return // The poller reports an update it cannot read and skips it.
		}
		_ = r.a.handleTelegramUpdate(context.Background(), r.telegram, u)

		var sent struct {
			CallbackQuery *struct {
				From struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Message *struct {
					MessageID int64 `json:"message_id"`
					Chat      struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
				Data string `json:"data"`
			} `json:"callback_query"`
		}
		_ = json.Unmarshal([]byte(raw), &sent)
		want := "open"
		if q := sent.CallbackQuery; q != nil && q.Message != nil && q.Message.Chat.ID == message.Chat.ID && q.Message.MessageID == message.MessageID && q.From.ID == barbara.ID {
			action, delivery, _ := strings.Cut(q.Data, ":")
			decoded, err := base64.RawURLEncoding.DecodeString(delivery)
			if err == nil && hex.EncodeToString(decoded) == deliveries[0].ID && strings.Count(q.Data, ":") == 1 {
				switch action {
				case telegramClaimAction:
					want = "claimed"
				case telegramResolveAction:
					want = "resolved"
				}
			}
		}
		incident, err := r.a.GetIncident(context.Background(), r.s.Principal, alert.IncidentID)
		if err != nil || incident.Status != want {
			t.Fatalf("the incident is %q, want %q, after the update %s: %v", incident.Status, want, raw, err)
		}
	})
}

// FuzzSlackInteractiveEnvelope decodes one Socket Mode envelope as the
// reader does and handles it.
func FuzzSlackInteractiveEnvelope(f *testing.F) {
	press := func(user, userTeam, ts, actionID, value string) string {
		return `{"envelope_id":"e1","type":"interactive","payload":{"type":"block_actions","api_app_id":"$APP","team":{"id":"$TEAM"},"user":{"id":"` + user + `","team_id":"` + userTeam + `"},"channel":{"id":"$CHANNEL"},"container":{"type":"message","channel_id":"$CHANNEL","message_ts":"` + ts + `"},"message":{"text":"SOS: Database down","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"SOS"}},{"type":"actions","elements":[]}],"ts":"$TS"},"actions":[{"action_id":"` + actionID + `","value":"` + value + `"}]}}`
	}
	for _, seed := range []string{
		press("$USER", "$TEAM", "$TS", slackClaimActionID, "$CLAIM"),
		press("$USER", "$TEAM", "$TS", slackResolveActionID, "$RESOLVE"),
		press("$USER", "", "$TS", slackClaimActionID, "$CLAIM"),
		press("UUNKNOWN", "$TEAM", "$TS", slackClaimActionID, "$CLAIM"),
		press("$USER", "T0002", "$TS", slackClaimActionID, "$CLAIM"),
		press("$USER", "$TEAM", "0.000001", slackClaimActionID, "$CLAIM"),
		press("$USER", "$TEAM", "$TS", slackResolveActionID, "$CLAIM"),
		press("$USER", "$TEAM", "$TS", slackOpenIncidentActionID, ""),
		`{"envelope_id":"e2","type":"events_api","payload":{"team_id":"$TEAM","api_app_id":"$APP","event":{"type":"message","user":"$USER","channel":"$CHANNEL","channel_type":"im","text":"0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"}}}`,
		`{"type":"hello","connection_info":{"app_id":"$APP"}}`,
		`{"type":"disconnect"}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, envelope string) {
		r := newSlackRig(t, true)
		r.connect(t, "personal_direct_message", "U0001", "D0001")
		r.dispatch(t)
		alert, message := r.sos(t)
		buttons := message.Buttons()
		raw := strings.NewReplacer("$CLAIM", buttons[0].Value, "$RESOLVE", buttons[1].Value, "$APP", fakesendingservices.SlackAppID,
			"$TEAM", message.WorkspaceID, "$CHANNEL", message.ChannelID, "$TS", message.TS, "$USER", "U0001").Replace(envelope)
		var decoded slackSocketEnvelope
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return // The reader ends the connection on an unreadable envelope.
		}
		slack := r.a.configuredContactMethodTypes()["slack_message"].(*SlackMessageContactMethodType)
		_ = r.a.handleSlackEnvelope(context.Background(), slack, decoded)

		var sent struct {
			Type    string `json:"type"`
			Payload struct {
				Type  string `json:"type"`
				AppID string `json:"api_app_id"`
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
					ChannelID string `json:"channel_id"`
					MessageTS string `json:"message_ts"`
				} `json:"container"`
				Actions []struct {
					ActionID string `json:"action_id"`
					Value    string `json:"value"`
				} `json:"actions"`
			} `json:"payload"`
		}
		_ = json.Unmarshal([]byte(raw), &sent)
		p := sent.Payload
		want := "open"
		if sent.Type == "interactive" && p.Type == "block_actions" && p.AppID == fakesendingservices.SlackAppID && len(p.Actions) == 1 &&
			p.Team.ID == message.WorkspaceID && (p.User.TeamID == "" || p.User.TeamID == message.WorkspaceID) && p.User.ID == "U0001" &&
			p.Container.ChannelID == message.ChannelID && (p.Channel.ID == "" || p.Channel.ID == message.ChannelID) && p.Container.MessageTS == message.TS {
			switch pressed := p.Actions[0]; {
			case pressed.ActionID == slackClaimActionID && pressed.Value == buttons[0].Value:
				want = "claimed"
			case pressed.ActionID == slackResolveActionID && pressed.Value == buttons[1].Value:
				want = "resolved"
			}
		}
		incident, err := r.a.GetIncident(context.Background(), r.member.Principal, alert.IncidentID)
		if err != nil || incident.Status != want {
			t.Fatalf("the incident is %q, want %q, after the envelope %s: %v", incident.Status, want, raw, err)
		}
	})
}
