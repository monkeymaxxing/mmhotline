package app

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

type smtpFixture struct {
	server      net.Listener
	mu          sync.Mutex
	messages    []string
	commands    []string
	reply       string
	authMethods string
	refuseAuth  bool
	implicitTLS *tls.Config
}

func newSMTPFixture(t *testing.T, reply, methods string, refuseAuth bool, secure *tls.Config) *smtpFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &smtpFixture{server: listener, reply: reply, authMethods: methods, refuseAuth: refuseAuth, implicitTLS: secure}
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() { defer workers.Done(); fixture.serve(t, connection) }()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; workers.Wait() })
	return fixture
}
func (s *smtpFixture) serve(t *testing.T, connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if s.implicitTLS != nil {
		connection = tls.Server(connection, s.implicitTLS)
	}
	client := textproto.NewConn(connection)
	defer client.Close()
	send := func(line string) { _ = client.PrintfLine("%s", line) }
	send("220 smtp.example.test ready")
	for {
		line, err := client.ReadLine()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, line)
		s.mu.Unlock()
		switch {
		case strings.HasPrefix(line, "EHLO"):
			send("250-smtp.example.test")
			send("250-SMTPUTF8")
			if s.authMethods != "" {
				send("250-AUTH " + s.authMethods)
			}
			send("250 OK")
		case strings.HasPrefix(line, "AUTH PLAIN"):
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			if err != nil || string(raw) != "\x00barbara\x00 password with spaces " {
				t.Errorf("PLAIN credentials changed: %q %v", raw, err)
			}
			if s.refuseAuth {
				send("535 authentication refused")
			} else {
				send("235 authenticated")
			}
		case strings.HasPrefix(line, "AUTH LOGIN"):
			send("334 VXNlcm5hbWU6")
			user, _ := client.ReadLine()
			send("334 UGFzc3dvcmQ6")
			password, _ := client.ReadLine()
			decodedUser, _ := base64.StdEncoding.DecodeString(user)
			decodedPassword, _ := base64.StdEncoding.DecodeString(password)
			if string(decodedUser) != "barbara" || string(decodedPassword) != " password with spaces " {
				t.Errorf("LOGIN credentials changed")
			}
			if s.refuseAuth {
				send("535 authentication refused")
			} else {
				send("235 authenticated")
			}
		case strings.HasPrefix(line, "MAIL FROM:"):
			if strings.Contains(line, "SMTPUTF8") || strings.Contains(line, "BODY=") {
				t.Error("ASCII email requested unnecessary SMTP extensions")
			}
			send("250 sender accepted")
		case strings.HasPrefix(line, "RCPT TO:"):
			send("250 recipient accepted")
		case line == "DATA":
			send("354 send message")
			raw, err := io.ReadAll(client.DotReader())
			if err != nil {
				return
			}
			s.mu.Lock()
			s.messages = append(s.messages, string(raw))
			s.mu.Unlock()
			if s.reply == "lost" {
				return
			}
			send(s.reply)
		case line == "QUIT":
			send("221 goodbye")
			return
		default:
			send("500 unexpected command")
		}
	}
}
func (s *smtpFixture) contactMethodType() EmailContactMethodType {
	return EmailContactMethodType{SMTPServer: s.server.Addr().String(), SMTPConnectionSecurity: "unencrypted", EmailFromAddress: "alerts@example.com"}
}
func (s *smtpFixture) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...), append([]string(nil), s.commands...)
}
func emailSetupFields(contactMethodType EmailContactMethodType) map[string]string {
	return map[string]string{"smtp_server": contactMethodType.SMTPServer, "smtp_connection_security": contactMethodType.SMTPConnectionSecurity, "smtp_user_name": contactMethodType.SMTPUserName, "smtp_password": contactMethodType.SMTPPassword, "email_from_address": contactMethodType.EmailFromAddress}
}

func TestSMTPSetupCheck(t *testing.T) {
	t.Parallel()
	for _, methods := range []string{"PLAIN LOGIN", "LOGIN", ""} {
		t.Run(methods, func(t *testing.T) {
			server := newSMTPFixture(t, "250 accepted", methods, false, nil)
			contactMethodType := server.contactMethodType()
			if methods != "" {
				contactMethodType.SMTPUserName = "barbara"
				contactMethodType.SMTPPassword = " password with spaces "
			}
			app := foundationApp(t)
			member := memberFixture(t, app, "smtp-setup@example.test")
			result, err := app.SetUpSendingService(context.Background(), member.Principal, "mail_server", emailSetupFields(contactMethodType))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Checked, "password and alerts cross the network unencrypted") {
				t.Fatal(result.Checked)
			}
			messages, commands := server.snapshot()
			if len(messages) != 0 || strings.Contains(strings.Join(commands, "\n"), "MAIL FROM") {
				t.Fatal("setup sent a message or envelope", commands)
			}
			var public, secret string
			if err = app.Store.db.QueryRow(`SELECT settings_json,secret_settings_json FROM sending_service_setups WHERE sending_service='mail_server'`).Scan(&public, &secret); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(public, "smtp_password") {
				t.Fatal("public setup exposed password")
			}
			var secrets map[string]string
			_ = json.Unmarshal([]byte(secret), &secrets)
			if secrets["smtp_password"] != contactMethodType.SMTPPassword {
				t.Fatal("password spaces changed")
			}
			restarted := New(app.Store)
			if err = restarted.LoadSendingServiceSetups(context.Background(), nil, "http://fake.example.test"); err != nil {
				t.Fatal(err)
			}
			restored := restarted.configuredContactMethodTypes()["email"].(EmailContactMethodType)
			if restored.SMTPServer != contactMethodType.SMTPServer || restored.SMTPPassword != contactMethodType.SMTPPassword {
				t.Fatal("restart lost SMTP setup")
			}
		})
	}
}
func TestSMTPSetupRefusalKeepsPrevious(t *testing.T) {
	t.Parallel()
	good := newSMTPFixture(t, "250 accepted", "", false, nil)
	app := foundationApp(t)
	member := memberFixture(t, app, "smtp-refusal@example.test")
	if _, err := app.SetUpSendingService(context.Background(), member.Principal, "mail_server", emailSetupFields(good.contactMethodType())); err != nil {
		t.Fatal(err)
	}
	bad := newSMTPFixture(t, "250 accepted", "PLAIN", true, nil)
	contactMethodType := bad.contactMethodType()
	contactMethodType.SMTPUserName = "barbara"
	contactMethodType.SMTPPassword = " password with spaces "
	_, err := app.SetUpSendingService(context.Background(), member.Principal, "mail_server", emailSetupFields(contactMethodType))
	if err == nil || !strings.Contains(err.Error(), "535 authentication refused") {
		t.Fatal(err)
	}
	if app.configuredContactMethodTypes()["email"].(EmailContactMethodType).SMTPServer != good.contactMethodType().SMTPServer {
		t.Fatal("failed check replaced setup")
	}
}
func TestSMTPStartTLSRequired(t *testing.T) {
	t.Parallel()
	server := newSMTPFixture(t, "250 accepted", "", false, nil)
	contactMethodType := server.contactMethodType()
	contactMethodType.SMTPConnectionSecurity = "starttls"
	if _, err := contactMethodType.checkSetup(context.Background(), emailSetupFields(contactMethodType), ""); err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatal(err)
	}
	if messages, _ := server.snapshot(); len(messages) != 0 {
		t.Fatal("sent without required TLS")
	}
}
func TestSMTPAddressAndCredentialRules(t *testing.T) {
	t.Parallel()
	valid := EmailContactMethodType{SMTPServer: "smtp.example.com:587", SMTPConnectionSecurity: "starttls", EmailFromAddress: "alerts@example.com"}
	for _, test := range []struct {
		name   string
		change func(*EmailContactMethodType)
	}{
		{"missing_port", func(c *EmailContactMethodType) { c.SMTPServer = "smtp.example.com" }},
		{"bad_port", func(c *EmailContactMethodType) { c.SMTPServer = "smtp.example.com:0" }},
		{"missing_security", func(c *EmailContactMethodType) { c.SMTPConnectionSecurity = "" }},
		{"display_name", func(c *EmailContactMethodType) { c.EmailFromAddress = "MMHotline <alerts@example.com>" }},
		{"non_ascii", func(c *EmailContactMethodType) { c.EmailFromAddress = "alérts@example.com" }},
		{"password_without_user", func(c *EmailContactMethodType) { c.SMTPPassword = "password" }},
		{"user_without_password", func(c *EmailContactMethodType) { c.SMTPUserName = "barbara" }},
		{"credential_control", func(c *EmailContactMethodType) { c.SMTPUserName = "barbara"; c.SMTPPassword = "a\x00b" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			contactMethodType := valid
			test.change(&contactMethodType)
			if err := contactMethodType.validateSetup(); err == nil {
				t.Fatal("accepted invalid setup")
			}
		})
	}
	for _, address := range []string{"Person <member@example.com>", "mé@example.com", "member@example.com\r\nBcc: other@example.com"} {
		if _, err := normalizeEmail(address); err == nil {
			t.Fatal("accepted non-plain address", address)
		}
	}
}
func TestSMTPMessageEncodingAndStableID(t *testing.T) {
	t.Parallel()
	server := newSMTPFixture(t, "250 accepted", "", false, nil)
	contactMethodType := server.contactMethodType()
	message := ContactMethodSend{DeliveryID: "delivery-1", SentTo: "barbara@example.com", Subject: "[SOS] Chișinău — investigate", Text: "Unicode: 你好 👋\n.dot\r\nTabs:\t trailing \n" + strings.Repeat("very long line ", 300)}
	for range 2 {
		if result := contactMethodType.Send(context.Background(), message); result.State != "accepted" || result.SentFrom != "MMHotline <alerts@example.com>" {
			t.Fatal(result)
		}
	}
	messages, _ := server.snapshot()
	if len(messages) != 2 {
		t.Fatal(messages)
	}
	for _, raw := range messages {
		parsed, err := mail.ReadMessage(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
		if err != nil || subject != message.Subject {
			t.Fatal(subject, err)
		}
		body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, parsed.Body))
		if err != nil || string(body) != message.Text {
			t.Fatalf("body changed: %q %v", body, err)
		}
		if parsed.Header.Get("Message-ID") != "<delivery-1@example.com>" || parsed.Header.Get("From") != "MMHotline <alerts@example.com>" || parsed.Header.Get("To") != message.SentTo || parsed.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
			t.Fatal(parsed.Header)
		}
		if _, err = time.Parse(time.RFC1123Z, parsed.Header.Get("Date")); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(raw, "\n") {
			if len(line) > 998 {
				t.Fatal("message line too long")
			}
		}
	}
}
func TestSMTPReplyOutboxResults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		reply, state string
		retry        bool
	}{{"250 accepted", "accepted", false}, {"451 try again later", "failed", true}, {"550 mailbox refused", "failed", false}, {"lost", "uncertain", true}} {
		t.Run(test.reply, func(t *testing.T) {
			server := newSMTPFixture(t, test.reply, "", false, nil)
			contactMethodType := server.contactMethodType()
			app := foundationApp(t)
			advance := deliveryTestClock(app)
			member := memberFixture(t, app, "smtp-results@example.test")
			savedEmail(t, app, member.Principal, true)
			queueDelivery(t, app, member.Principal, "smtp-result", "warn")
			if _, err := dispatchWith(app, "https://mmhotline.example.com", contactMethodType); err != nil {
				t.Fatal(err)
			}
			delivery := findDelivery(t, app, member.Principal, "smtp-result")
			if delivery.State != test.state || (!delivery.NextAttempt.IsZero()) != test.retry {
				t.Fatalf("%+v", delivery)
			}
			if test.reply != "lost" && test.state != "accepted" && !strings.Contains(delivery.Reason, test.reply) {
				t.Fatal(delivery.Reason)
			}
			if test.retry {
				advance(5 * time.Second)
				contactMethodType.EmailFromAddress = "replacement@example.com"
				if _, err := dispatchWith(app, "https://mmhotline.example.com", contactMethodType); err != nil {
					t.Fatal(err)
				}
				messages, _ := server.snapshot()
				if len(messages) != 2 {
					t.Fatal(len(messages))
				}
				for _, raw := range messages {
					parsed, err := mail.ReadMessage(strings.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					if parsed.Header.Get("Message-ID") != "<"+delivery.ID+"@example.com>" || parsed.Header.Get("From") != "MMHotline <alerts@example.com>" {
						t.Fatal(parsed.Header)
					}
				}
			}
		})
	}
}
func TestSMTPConnectionFailureAndCancellation(t *testing.T) {
	t.Parallel()
	server := newSMTPFixture(t, "250 accepted", "", false, nil)
	contactMethodType := server.contactMethodType()
	_ = server.server.Close()
	result := contactMethodType.Send(context.Background(), ContactMethodSend{DeliveryID: "one", SentTo: "member@example.com"})
	if result.State != "failed" || !result.Retryable {
		t.Fatal(result)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err == nil {
			defer connection.Close()
			_, _ = io.Copy(io.Discard, connection)
		}
	}()
	contactMethodType.SMTPServer = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result = contactMethodType.Send(ctx, ContactMethodSend{DeliveryID: "two", SentTo: "member@example.com"})
	<-done
	if result.State != "failed" || !result.Retryable {
		t.Fatal(result)
	}
}

func TestSMTPSetupPageOptionalCredentials(t *testing.T) {
	t.Parallel()
	rig := newWorkflowRig(t)
	browser := rig.browser()
	browser.addPerson("smtp-admin@example.test", "Administrator")
	page := browser.get("/sending-services/mail_server", 200)
	for field, label := range map[string]string{"smtp_user_name": "SMTP user name (optional)", "smtp_password": "SMTP password (optional)"} {
		if !strings.Contains(settingsPageText(page), "sending_services.mail_server."+field) || !strings.Contains(settingsPageText(page), label) {
			t.Fatal("optional field not shown as optional", field)
		}
	}
	if !strings.Contains(page, "password and alerts cross the network unencrypted") {
		t.Fatal("missing security warning")
	}
	server := newSMTPFixture(t, "250 accepted", "", false, nil)
	if _, err := rig.app.SetUpSendingService(context.Background(), Principal{UnixUser: "fixture"}, "mail_server", emailSetupFields(server.contactMethodType())); err != nil {
		t.Fatal(err)
	}
	saved := browser.get("/sending-services/mail_server", 200)
	if !strings.Contains(saved, "Checked with the mail server") {
		t.Fatal("setup success missing")
	}
	browser.get("/contact-methods", 200)
}
