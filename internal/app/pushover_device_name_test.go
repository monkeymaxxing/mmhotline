package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"mmhotline/internal/fakesendingservices"
)

func (r *pushoverRig) addOnDevice(t *testing.T, device string) (ContactMethod, error) {
	t.Helper()
	return r.a.SaveContactMethod(context.Background(), r.principal, ContactMethodInput{ContactMethodType: "pushover_notification",
		Fields:        map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey, "pushover_device_name": device},
		Subscriptions: []string{"sos", "warn", "info"}})
}

// A named device is sent as `device` on the quiet contact test message, on an
// emergency SOS, and on warning and info messages. Fails if any Send path
// drops the field.
func TestPushoverDeviceNameIsSentOnEveryDelivery(t *testing.T) {
	r := newPushoverRig(t)
	if _, err := r.addOnDevice(t, "tablet"); err != nil {
		t.Fatal(err)
	}
	r.dispatch(t)
	sent := r.fake.PushoverMessages()
	if len(sent) != 1 || sent[0].Device != "tablet" || sent[0].Title != "MMHotline can reach this phone" {
		t.Fatalf("contact test message: %+v", sent)
	}
	r.sos(t, "Database down")
	r.dispatch(t)
	sent = r.fake.PushoverMessages()
	if last := sent[len(sent)-1]; last.Priority != 2 || last.Device != "tablet" {
		t.Fatalf("emergency: %+v", last)
	}
	c := r.a.configuredContactMethodTypes()["pushover_notification"].(*PushoverNotificationContactMethodType)
	settings, err := c.CheckSettings(context.Background(), ContactMethodSettingsInput{Fields: map[string]string{"pushover_user_key": fakesendingservices.PushoverUserKey, "pushover_device_name": "tablet"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, severity := range []string{"warn", "info"} {
		if result := c.Send(context.Background(), ContactMethodSend{Purpose: "subscribed_alert", Severity: severity, Title: "Disk", AlertMessage: "full", IncidentID: NewID(), PublicURL: r.a.WebURL, Settings: settings.JSON}); result.State != "accepted" {
			t.Fatalf("%s: %+v", severity, result)
		}
		sent = r.fake.PushoverMessages()
		if last := sent[len(sent)-1]; last.Device != "tablet" {
			t.Fatalf("%s: %+v", severity, last)
		}
	}
}

// With no device named, no `device` field is sent, and the device name is not
// stored in the settings.
func TestPushoverWithoutDeviceNameSendsNoDeviceField(t *testing.T) {
	r := newPushoverRig(t)
	c, err := r.addOnDevice(t, "")
	if err != nil {
		t.Fatal(err)
	}
	key := fakesendingservices.PushoverUserKey
	if strings.Contains(c.Settings, "device") || c.SentToText() != "the Pushover user key ending in "+key[len(key)-4:] || strings.Contains(c.SentToText(), key) {
		t.Fatalf("settings %s, shown %s", c.Settings, c.SentToText())
	}
	r.dispatch(t)
	r.sos(t, "Database down")
	r.dispatch(t)
	sent := r.fake.PushoverMessages()
	if len(sent) != 2 {
		t.Fatalf("messages: %+v", sent)
	}
	for _, m := range sent {
		if m.Device != "" {
			t.Fatalf("device sent: %+v", m)
		}
	}
}

// The MMHotline-down alarm's held requests, the ordinary one and its test,
// carry the device; without one they carry none.
func TestPushoverDownAlarmRequestsCarryTheDeviceName(t *testing.T) {
	c := &PushoverNotificationContactMethodType{ApplicationToken: fakesendingservices.PushoverApplicationToken}
	text := downAlarmText{WebURL: "https://mmhotline.example.com"}
	for settings, want := range map[string]string{
		`{"pushover_user_key":"` + fakesendingservices.PushoverUserKey + `","pushover_device_name":"tablet"}`: "tablet",
		`{"pushover_user_key":"` + fakesendingservices.PushoverUserKey + `"}`:                                 "",
	} {
		request, test, err := c.downAlarmRequests(ContactMethod{Settings: settings}, text)
		if err != nil {
			t.Fatal(err)
		}
		for name, held := range map[string]string{"request": request.Body, "test": test.Body} {
			fields, _ := url.ParseQuery(held)
			if _, has := fields["device"]; fields.Get("device") != want || has != (want != "") {
				t.Fatalf("%s for %q: %v", name, want, fields)
			}
		}
	}
}

// An invalid name and a device Pushover does not list for the user are refused
// in plain words, and nothing is saved or sent.
func TestPushoverInvalidOrUnknownDeviceNameIsRefusedAndChangesNothing(t *testing.T) {
	r := newPushoverRig(t)
	_, err := r.addOnDevice(t, "my phone!")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "1 to 25 characters") {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err = r.addOnDevice(t, strings.Repeat("a", 26)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("26 characters: %v", err)
	}
	_, err = r.addOnDevice(t, "watch")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no device named watch") || !strings.Contains(err.Error(), "phone, tablet") {
		t.Fatalf("unknown device: %v", err)
	}
	methods, err := r.a.ContactMethods(context.Background(), r.principal)
	if err != nil || len(methods) != 0 {
		t.Fatalf("saved: %+v %v", methods, err)
	}
	r.dispatch(t)
	if sent := r.fake.PushoverMessages(); len(sent) != 0 {
		t.Fatalf("sent: %+v", sent)
	}
}

// The command adds a contact method on a device, and list shows it after the
// user key.
func TestContactMethodsAddWithPushoverDeviceNameThenListShowsIt(t *testing.T) {
	r := newPushoverRig(t)
	add := CommandRequest{Group: "contact-methods", Action: "add", ContactMethods: ContactMethodsCommandRequest{
		SignInEmailAddress: r.principal.SignInEmailAddress, ContactMethodType: "pushover_notification", PushoverUserKey: fakesendingservices.PushoverUserKey, PushoverDeviceName: "tablet"}}
	result, _ := commandRequest(t, r.a, add, 200)
	key := fakesendingservices.PushoverUserKey
	want := "the Pushover user key ending in " + key[len(key)-4:] + " on device tablet"
	if len(result.ContactMethods) != 1 || result.ContactMethods[0].SendsTo != want {
		t.Fatalf("add: %+v", result.ContactMethods)
	}
	listed, _ := commandRequest(t, r.a, CommandRequest{Group: "contact-methods", Action: "list"}, 200)
	if len(listed.ContactMethods) != 1 || listed.ContactMethods[0].SendsTo != want {
		t.Fatalf("list: %+v", listed.ContactMethods)
	}
	methods, _ := r.a.ContactMethods(context.Background(), r.principal)
	if len(methods) != 1 || methods[0].Setting("pushover_device_name") != "tablet" || methods[0].SentTo != fakesendingservices.PushoverUserKey {
		t.Fatalf("stored: %+v", methods)
	}
}
