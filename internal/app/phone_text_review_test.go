package app

import (
	"testing"
)

// Contact card names use natural articles and preserve product names.

func TestCardTitlesInSentences(t *testing.T) {
	for kind, want := range map[string]string{"automated_voice_call": "an automated voice call", "email": "an email", "outgoing_webhook": "an outgoing webhook", "slack_message": "a Slack message", "telegram_message": "a Telegram message"} {
		facts, _ := contactMethodTypeFacts(kind)
		if got := cardTitleWithArticle(facts); got != want {
			t.Errorf("%s: %q", kind, got)
		}
	}
}
