package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var callingNumberPattern = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)
var twilioAccountSIDPattern = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
var twilioAPIKeySIDPattern = regexp.MustCompile(`^SK[0-9a-fA-F]{32}$`)

func (*AutomatedVoiceCallContactMethodType) Setup() SendingServiceSetupDefinition {
	return SendingServiceSetupDefinition{
		Name: "twilio", Title: "Twilio",
		Fields: []SendingServiceSetupField{
			{Name: "twilio_account_sid", SettingsKey: "account_sid", Label: "Twilio account SID", Help: "The account that owns the calling number."},
			{Name: "twilio_api_key_sid", SettingsKey: "api_key_sid", Label: "Twilio API key SID", Help: "Create a dedicated API key for MMHotline."},
			{Name: "twilio_api_key_secret", SettingsKey: "api_key_secret", Label: "Twilio API key secret", Secret: true, Help: "The API key's secret."},
			{Name: "twilio_phone_number_to_call_from", SettingsKey: "phone_number_to_call_from", Label: "Twilio calling number", Help: "An incoming phone number owned by the Twilio account and able to receive voice calls. Use a plus sign, country code, and number without spaces, such as +12025550102."},
			{Name: "extra_phone_numbers_on_the_contact_card", SettingsKey: "extra_phone_numbers_on_the_contact_card", Label: "Extra phone numbers on the contact card", Optional: true, Help: "A list of phone numbers to include on the contact card, each with a plus sign, country code, and number without spaces, such as +12025550102. Use [] for none."},
		},
		Guide: []SendingServiceSetupStep{
			{Text: "Use your team's Twilio account and create a dedicated API key.", URL: "https://www.twilio.com/docs/iam/api-keys", LinkLabel: "Twilio API keys"},
			{Text: "Choose a voice-capable incoming phone number owned by the account. MMHotline opens all connections to Twilio."},
			{Text: "Put the Twilio account SID, API key, and calling number in the settings file using the keys below, then apply the settings file. Before saving the setup, MMHotline checks with Twilio that the account owns the calling number and the number supports voice calls."},
		},
		NextStep: "Add your own phone number under Contact methods, then download the MMHotline contact card and test Do Not Disturb.",
		// No fixed identity: people's recipient numbers belong to people, not Twilio.
		DescribeCheck: func(settings map[string]any) string {
			return fmt.Sprintf("Checked with Twilio: account %v, calling from %v", settings["twilio_account_sid"], settings["twilio_phone_number_to_call_from"])
		},
		Check: checkTwilioSetup, Build: buildTwilioSetup,
	}
}

func buildTwilioSetup(settings map[string]any, secrets map[string]string, fakeURL string) ContactMethodType {
	text := func(name string) string { value, _ := settings[name].(string); return value }
	account := &TwilioAccount{AccountSID: text("twilio_account_sid"), APIKeySID: text("twilio_api_key_sid"), APIKeySecret: secrets["twilio_api_key_secret"], PhoneNumberToCallFrom: text("twilio_phone_number_to_call_from")}
	if fakeURL != "" {
		account.APIBaseURL = fakeURL + "/twilio"
	}
	var extraPhoneNumbers []string
	if numbers := text("extra_phone_numbers_on_the_contact_card"); numbers != "" {
		extraPhoneNumbers = strings.Split(numbers, ",")
	}
	return &AutomatedVoiceCallContactMethodType{SendingService: &TwilioSendingService{Twilio: account, ExtraPhoneNumbersOnTheContactCard: extraPhoneNumbers}}
}

func checkTwilioSetup(ctx context.Context, fields map[string]string, fakeURL string) (sendingServiceSetupCheck, error) {
	refused := func(message string) (sendingServiceSetupCheck, error) {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: %s", ErrInvalid, message)
	}
	if !twilioAccountSIDPattern.MatchString(fields["twilio_account_sid"]) {
		return refused("Twilio account SID must be AC followed by 32 hexadecimal digits")
	}
	if !twilioAPIKeySIDPattern.MatchString(fields["twilio_api_key_sid"]) {
		return refused("Twilio API key SID must be SK followed by 32 hexadecimal digits")
	}
	if !callingNumberPattern.MatchString(fields["twilio_phone_number_to_call_from"]) {
		return refused("Twilio calling number must be in E.164 format, such as +12025550102")
	}
	var extraPhoneNumbers []string
	if fields["extra_phone_numbers_on_the_contact_card"] != "" {
		for _, number := range strings.Split(fields["extra_phone_numbers_on_the_contact_card"], ",") {
			number = strings.TrimSpace(number)
			if !callingNumberPattern.MatchString(number) {
				return refused("Extra phone numbers on the contact card must be comma-separated E.164 numbers")
			}
			extraPhoneNumbers = append(extraPhoneNumbers, number)
		}
	}
	settings := map[string]any{"twilio_account_sid": fields["twilio_account_sid"], "twilio_api_key_sid": fields["twilio_api_key_sid"], "twilio_phone_number_to_call_from": fields["twilio_phone_number_to_call_from"], "extra_phone_numbers_on_the_contact_card": strings.Join(extraPhoneNumbers, ",")}
	secrets := map[string]string{"twilio_api_key_secret": fields["twilio_api_key_secret"]}
	p := buildTwilioSetup(settings, secrets, fakeURL).(*AutomatedVoiceCallContactMethodType).SendingService
	// A standard API key, the narrowest that can make calls, cannot read the
	// account itself, so the calling number's listing is the check: Twilio
	// refuses it for a wrong key or secret, and a suspended account.
	status, data, err := p.twilioGet(ctx, "/IncomingPhoneNumbers.json?"+url.Values{"PhoneNumber": {p.Twilio.PhoneNumberToCallFrom}}.Encode())
	if err != nil {
		return refused("MMHotline could not reach Twilio for the calling number check")
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return refused("Twilio refused the API key; check the account SID, the API key SID, and the secret")
	}
	if status != http.StatusOK {
		return refused(fmt.Sprintf("Twilio answered HTTP %d to the calling number check", status))
	}
	var numbers struct {
		Numbers []struct {
			AccountSID   string `json:"account_sid"`
			PhoneNumber  string `json:"phone_number"`
			Capabilities struct {
				Voice bool `json:"voice"`
			} `json:"capabilities"`
		} `json:"incoming_phone_numbers"`
	}
	if json.Unmarshal(data, &numbers) != nil {
		return refused("Twilio returned unreadable calling numbers")
	}
	for _, number := range numbers.Numbers {
		if number.AccountSID == p.Twilio.AccountSID && number.PhoneNumber == p.Twilio.PhoneNumberToCallFrom && number.Capabilities.Voice {
			return sendingServiceSetupCheck{Settings: settings, SecretSettings: secrets}, nil
		}
	}
	return refused("the Twilio calling number must be an incoming phone number owned by the configured Twilio account and able to receive voice calls")
}
