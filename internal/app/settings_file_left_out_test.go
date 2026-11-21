package app

import (
	"context"
	"strings"
	"testing"

	"mmhotline/internal/fakesendingservices"
)

// A part whose section the settings file leaves out is not set up, and
// settings apply and settings show say so, in agreement with
// sending-services list: neither says "applied at" for it, and a first
// setup that fails does not claim that an earlier setting stays.
func TestSettingsFileSaysALeftOutPartIsNotSetUp(t *testing.T) {
	t.Parallel()
	f := newSettingsFileFixture(t, exampleSettingsYAML)
	f.start()

	shown, err := f.a.SettingsFileState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := shown.Text()
	if !strings.Contains(text, "sending_services.twilio: not set up, because the settings file leaves it out.") || strings.Contains(text, "sending_services.twilio: applied at") {
		t.Fatalf("settings show for a left-out service:\n%s", text)
	}
	if !strings.Contains(text, "sending_services.pushover: applied at") {
		t.Fatalf("settings show for a set-up service:\n%s", text)
	}
	for _, part := range shown.Parts {
		if part.Part == "sending_services.twilio" && !part.LeftOutOfTheSettingsFile {
			t.Fatalf("settings show JSON for a left-out service: %+v", part)
		}
	}

	// A first Twilio setup that Twilio refuses keeps Twilio not set up.
	f.write(replaced(t, exampleSettingsYAML, `    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
`, `    application_token: "azGDORePK8gMaC0QOYAMyEEuzJnyUi"
  twilio:
    account_sid: `+fakesendingservices.TwilioAccountSID+`
    api_key_sid: `+fakesendingservices.TwilioAPIKeySID+`
    api_key_secret: wrong-secret
    phone_number_to_call_from: "`+fakesendingservices.TwilioPhoneNumberToCallFrom+`"
    extra_phone_numbers_on_the_contact_card: []
`))
	text = f.apply().Text()
	if !strings.Contains(text, "sending_services.twilio: did not apply: Twilio refused the API key; check the account SID, the API key SID, and the secret. It stays not set up, as before.") || strings.Contains(text, "The earlier setting") {
		t.Fatalf("apply for a refused first setup:\n%s", text)
	}
}
