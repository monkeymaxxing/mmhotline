package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// EmailContactMethodType sends one email per delivery through the installation's SMTP server.
type EmailContactMethodType struct {
	SMTPServer, SMTPConnectionSecurity, SMTPUserName, SMTPPassword, EmailFromAddress string
	// tlsConfig is supplied only by tests. Production uses the system authorities.
	tlsConfig *tls.Config
}

type emailSettings struct {
	EmailAddress string `json:"email_address"`
}

const smtpConversationTimeLimit = 30 * time.Second

func (EmailContactMethodType) Name() string { return "email" }
func (EmailContactMethodType) Facts() ContactMethodTypeFacts {
	return ContactMethodTypeFacts{
		CardTitle: "Email", ValueLabel: "Email address", ValueField: "email_address",
		ValueInputType: "email", SettingsFields: []string{"email_address"},
		CombinesMessages: true, RetryUncertain: true,
		AttemptTimeLimit:      smtpConversationTimeLimit,
		AllowedForTeamAccount: true, AcceptedLabel: "Accepted by the mail server",
	}
}
func (EmailContactMethodType) CheckSettings(_ context.Context, input ContactMethodSettingsInput) (ContactMethodSettings, error) {
	address, err := normalizeEmail(input.Fields["email_address"])
	if err != nil {
		return ContactMethodSettings{}, err
	}
	settings, err := settingsJSON(emailSettings{EmailAddress: address})
	return ContactMethodSettings{JSON: settings, SentTo: address}, err
}
func (c EmailContactMethodType) Setup() SendingServiceSetupDefinition {
	return SendingServiceSetupDefinition{
		Name: "mail_server", Title: "Mail server", TitleInSentence: "the mail server",
		Fields: []SendingServiceSetupField{
			{Name: "smtp_server", SettingsKey: "smtp_server", Label: "SMTP server", Help: "The mail server's host name and port, such as smtp.example.com:587."},
			{Name: "smtp_connection_security", SettingsKey: "smtp_connection_security", Label: "SMTP connection security", Help: "The value “starttls” requires a successful switch to TLS on the SMTP connection. The value “implicit_tls” uses TLS from the start of the connection. The value “unencrypted” sends the password and alerts without encryption."},
			{Name: "smtp_user_name", SettingsKey: "smtp_user_name", Label: "SMTP user name", Optional: true, Help: "For a relay that needs no authentication, leave out smtp_user_name and smtp_password."},
			{Name: "smtp_password", SettingsKey: "smtp_password", Label: "SMTP password", Secret: true, Optional: true, PreserveWhitespace: true, Help: "Required with the SMTP user name. Spaces belong to the password."},
			{Name: "email_from_address", SettingsKey: "email_from_address", Label: "Email from address", Help: "One plain ASCII address, such as alerts@example.com. Messages show the sender as MMHotline <email_from_address>."},
		},
		Guide: []SendingServiceSetupStep{
			{Text: "Get the SMTP server, connection security, credentials if needed, and sender address from your mail sending service or relay administrator."},
			{Text: "Put the mail server settings in the settings file, as shown below, and apply the settings file. MMHotline connects, greets the server, checks TLS and any authentication, then quits without sending. The value “unencrypted” sends the password and alerts without encryption."},
			{Text: "Add your own email address under Contact methods. Saving the contact method sends a test message."},
		},
		NextStep: "Add your own email address under Contact methods. Saving the contact method sends a test message.",
		DescribeCheck: func(settings map[string]any) string {
			description := fmt.Sprintf("Checked with the mail server: %s (%s); sender MMHotline <%s>. No message was sent by the setup check.", settings["smtp_server"], settings["smtp_connection_security"], settings["email_from_address"])
			if settings["smtp_connection_security"] == "unencrypted" {
				description += " The password and alerts cross the network unencrypted."
			}
			return description
		},
		Check: c.checkSetup,
		Build: c.buildEmailSetup,
	}
}
func (c EmailContactMethodType) buildEmailSetup(settings map[string]any, secrets map[string]string, _ string) ContactMethodType {
	value := func(key string) string { s, _ := settings[key].(string); return s }
	return EmailContactMethodType{SMTPServer: value("smtp_server"), SMTPConnectionSecurity: value("smtp_connection_security"), SMTPUserName: value("smtp_user_name"), SMTPPassword: secrets["smtp_password"], EmailFromAddress: value("email_from_address"), tlsConfig: c.tlsConfig}
}
func (c EmailContactMethodType) checkSetup(ctx context.Context, fields map[string]string, _ string) (sendingServiceSetupCheck, error) {
	settings := map[string]any{}
	for _, key := range []string{"smtp_server", "smtp_connection_security", "smtp_user_name", "email_from_address"} {
		settings[key] = fields[key]
	}
	checked := sendingServiceSetupCheck{Settings: settings, SecretSettings: map[string]string{"smtp_password": fields["smtp_password"]}}
	contactMethodType := c.buildEmailSetup(settings, checked.SecretSettings, "").(EmailContactMethodType)
	if err := contactMethodType.validateSetup(); err != nil {
		return sendingServiceSetupCheck{}, err
	}
	if _, err := contactMethodType.conversation(ctx, nil); err != nil {
		return sendingServiceSetupCheck{}, fmt.Errorf("%w: SMTP setup check: %s", ErrInvalid, contactMethodType.diagnostic(err))
	}
	return checked, nil
}
func (c EmailContactMethodType) validateSetup() error {
	host, port, err := net.SplitHostPort(c.SMTPServer)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 || strings.ContainsAny(host, "\r\n\t ") {
		return fmt.Errorf("%w: smtp_server must be a host name and port, such as smtp.example.com:587", ErrInvalid)
	}
	switch c.SMTPConnectionSecurity {
	case "starttls", "implicit_tls", "unencrypted":
	default:
		return fmt.Errorf("%w: smtp_connection_security must be starttls, implicit_tls, or unencrypted", ErrInvalid)
	}
	if !plainEmailAddress(c.EmailFromAddress) {
		return fmt.Errorf("%w: email_from_address must be one plain ASCII email address, such as alerts@example.com", ErrInvalid)
	}
	if (c.SMTPUserName == "") != (c.SMTPPassword == "") {
		return fmt.Errorf("%w: smtp_user_name and smtp_password are required together, or both must be empty", ErrInvalid)
	}
	for _, value := range []string{c.SMTPUserName, c.SMTPPassword} {
		if !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("%w: SMTP credentials must be one line of UTF-8 text without control characters", ErrInvalid)
		}
	}
	return nil
}
func (c EmailContactMethodType) Send(ctx context.Context, message ContactMethodSend) DeliveryResult {
	if c.SMTPServer == "" {
		return DeliveryResult{State: "failed", Diagnostic: "email contact method type is not set up"}
	}
	if message.SentFrom == "" {
		message.SentFrom = "MMHotline <" + c.EmailFromAddress + ">"
	}
	result := DeliveryResult{State: "accepted", SentFrom: message.SentFrom}
	unknown, err := c.conversation(ctx, &message)
	if err == nil {
		return result
	}
	result.State = "failed"
	result.Diagnostic = c.diagnostic(err)
	var reply *textproto.Error
	if errors.As(err, &reply) {
		result.Retryable = reply.Code >= 400 && reply.Code < 500
	} else {
		result.Retryable = true
		if unknown {
			result.State = "uncertain"
		}
	}
	return result
}
func (c EmailContactMethodType) diagnostic(err error) string {
	text := err.Error()
	var reply *textproto.Error
	if errors.As(err, &reply) {
		text = strings.ReplaceAll(text, reply.Error(), fmt.Sprintf("%d %s", reply.Code, reply.Msg))
	}
	if c.SMTPPassword != "" {
		for _, secret := range []string{c.SMTPPassword, base64.StdEncoding.EncodeToString([]byte(c.SMTPPassword)), base64.StdEncoding.EncodeToString([]byte("\x00" + c.SMTPUserName + "\x00" + c.SMTPPassword))} {
			text = strings.ReplaceAll(text, secret, "[secret removed]")
		}
	}
	text = strings.Join(strings.Fields(strings.ToValidUTF8(text, "?")), " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	if len(text) > 500 {
		text = strings.ToValidUTF8(text[:500], "") + "…"
	}
	return text
}

// conversation stops after authentication for setup. unknown means DATA may
// have reached the server but its final acceptance reply was unavailable.
func (c EmailContactMethodType) conversation(ctx context.Context, message *ContactMethodSend) (unknown bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, smtpConversationTimeLimit)
	defer cancel()
	host, _, err := net.SplitHostPort(c.SMTPServer)
	if err != nil {
		return false, err
	}
	network, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.SMTPServer)
	if err != nil {
		return false, fmt.Errorf("connecting to the mail server: %w", err)
	}
	defer network.Close()
	stop := context.AfterFunc(ctx, func() { _ = network.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = network.SetDeadline(deadline)
	secure := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if c.tlsConfig != nil {
		secure = c.tlsConfig.Clone()
		secure.ServerName = host
		secure.MinVersion = tls.VersionTLS12
	}
	var connection net.Conn = network
	if c.SMTPConnectionSecurity == "implicit_tls" {
		encrypted := tls.Client(network, secure)
		if err = encrypted.HandshakeContext(ctx); err != nil {
			return false, fmt.Errorf("starting SMTP TLS: %w", err)
		}
		connection = encrypted
	}
	client, err := smtp.NewClient(&smtpReplyConnection{Conn: connection, reader: io.LimitReader(connection, 1024*1024)}, host)
	if err != nil {
		return false, fmt.Errorf("mail server greeting: %w", err)
	}
	defer client.Close()
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	if err = client.Hello(hostname); err != nil {
		return false, fmt.Errorf("greeting the mail server: %w", err)
	}
	if c.SMTPConnectionSecurity == "starttls" {
		if offered, _ := client.Extension("STARTTLS"); !offered {
			return false, errors.New("the mail server does not offer STARTTLS, which smtp_connection_security: starttls requires")
		}
		if err = client.StartTLS(secure); err != nil {
			return false, fmt.Errorf("switching SMTP to TLS: %w", err)
		}
	}
	if c.SMTPUserName != "" {
		_, offered := client.Extension("AUTH")
		methods := strings.Fields(strings.ToUpper(offered))
		var authentication smtp.Auth
		switch {
		case slices.Contains(methods, "PLAIN"):
			authentication = smtpPlainAuthentication{c.SMTPUserName, c.SMTPPassword}
		case slices.Contains(methods, "LOGIN"):
			authentication = &smtpLoginAuthentication{userName: c.SMTPUserName, password: c.SMTPPassword}
		default:
			return false, errors.New("the mail server offers no supported authentication method; MMHotline uses PLAIN or LOGIN")
		}
		if err = client.Auth(authentication); err != nil {
			return false, fmt.Errorf("SMTP authentication: %w", err)
		}
	}
	if message == nil {
		return false, client.Quit()
	}
	sender, err := mail.ParseAddress(message.SentFrom)
	if err != nil {
		return false, err
	}
	// ASCII addresses and base64 content need no SMTPUTF8 or 8BITMIME extension.
	command, err := client.Text.Cmd("MAIL FROM:<%s>", sender.Address)
	if err != nil {
		return false, err
	}
	client.Text.StartResponse(command)
	_, _, err = client.Text.ReadResponse(250)
	client.Text.EndResponse(command)
	if err != nil {
		return false, fmt.Errorf("SMTP sender: %w", err)
	}
	if err = client.Rcpt(message.SentTo); err != nil {
		return false, fmt.Errorf("SMTP recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return false, fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err = writer.Write(emailMessage(*message, time.Now())); err != nil {
		return true, fmt.Errorf("sending the email: %w", err)
	}
	if err = writer.Close(); err != nil {
		return true, fmt.Errorf("mail server acceptance reply: %w", err)
	}
	// Once DATA received 250, a failed QUIT cannot undo acceptance.
	_ = client.Quit()
	return false, nil
}

// Bound server replies, including a greeting or multiline refusal, to 1 MiB.
// The embedded connection keeps deadlines and TLS upgrade available to net/smtp.
type smtpReplyConnection struct {
	net.Conn
	reader io.Reader
}

func (c *smtpReplyConnection) Read(buffer []byte) (int, error) { return c.reader.Read(buffer) }

type smtpPlainAuthentication struct{ userName, password string }

func (a smtpPlainAuthentication) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.userName + "\x00" + a.password), nil
}
func (smtpPlainAuthentication) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected challenge after SMTP PLAIN authentication")
	}
	return nil, nil
}

type smtpLoginAuthentication struct {
	userName, password string
	questions          int
}

func (*smtpLoginAuthentication) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}
func (a *smtpLoginAuthentication) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.questions++
	switch a.questions {
	case 1:
		return []byte(a.userName), nil
	case 2:
		return []byte(a.password), nil
	}
	return nil, errors.New("unexpected third challenge during SMTP LOGIN authentication")
}
func emailMessage(message ContactMethodSend, now time.Time) []byte {
	subject := strings.ReplaceAll(strings.ReplaceAll(message.Subject, "\r", " "), "\n", " ")
	subject = mime.QEncoding.Encode("UTF-8", subject)
	subject = strings.ReplaceAll(subject, "?= =?", "?=\r\n =?")
	sender, _ := mail.ParseAddress(message.SentFrom)
	domain := sender.Address[strings.LastIndex(sender.Address, "@")+1:] //nolint:nilaway // conversation parses SentFrom, and stops at an error, before it builds the message.
	var buffer bytes.Buffer
	for _, header := range [][2]string{
		{"From", message.SentFrom}, {"To", message.SentTo}, {"Subject", subject},
		{"Date", now.UTC().Format(time.RFC1123Z)}, {"Message-ID", "<" + message.DeliveryID + "@" + domain + ">"},
		{"Auto-Submitted", "auto-generated"}, {"MIME-Version", "1.0"}, {"Content-Type", "text/plain; charset=UTF-8"}, {"Content-Transfer-Encoding", "base64"},
	} {
		fmt.Fprintf(&buffer, "%s: %s\r\n", header[0], header[1])
	}
	buffer.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(message.Text))
	for len(encoded) > 76 {
		buffer.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	buffer.WriteString(encoded + "\r\n")
	return buffer.Bytes()
}
func plainEmailAddress(value string) bool {
	if len(value) > 254 {
		return false
	}
	for _, character := range []byte(value) {
		if character >= 128 {
			return false
		}
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Name == "" && address.Address == value && strings.Contains(value, "@") //nolint:nilaway // address is read only when err is nil.
}
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !plainEmailAddress(email) {
		return "", fmt.Errorf("%w: enter one plain ASCII email address", ErrInvalid)
	}
	return email, nil
}
