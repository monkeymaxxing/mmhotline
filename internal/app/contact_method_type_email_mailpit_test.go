package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func smtpTestCertificate(t *testing.T) (string, string, *tls.Config, *tls.Config) {
	t.Helper()
	directory := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SMTP test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	certPath, keyPath := filepath.Join(directory, "smtp-cert.pem"), filepath.Join(directory, "smtp-key.pem")
	if err = os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid test certificate")
	}
	return certPath, keyPath, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
}
func TestSMTPImplicitTLSAndCertificateChecks(t *testing.T) {
	t.Parallel()
	_, _, trusted, serverTLS := smtpTestCertificate(t)
	server := newSMTPFixture(t, "250 accepted", "", false, serverTLS)
	contactMethodType := server.contactMethodType()
	contactMethodType.SMTPConnectionSecurity = "implicit_tls"
	contactMethodType.tlsConfig = trusted
	if _, err := contactMethodType.checkSetup(context.Background(), emailSetupFields(contactMethodType), ""); err != nil {
		t.Fatal(err)
	}
	contactMethodType.tlsConfig = nil
	if _, err := contactMethodType.checkSetup(context.Background(), emailSetupFields(contactMethodType), ""); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatal("untrusted certificate accepted", err)
	}
	contactMethodType.tlsConfig = trusted.Clone()
	contactMethodType.tlsConfig.RootCAs = x509.NewCertPool()
	if _, err := contactMethodType.checkSetup(context.Background(), emailSetupFields(contactMethodType), ""); err == nil {
		t.Fatal("empty trust store accepted certificate")
	}
}
func freeMailpitAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}
func TestEmailMailpitAcceptance(t *testing.T) {
	binary := os.Getenv("MMHOTLINE_TEST_MAILPIT_BINARY")
	if binary == "" {
		t.Skip("MMHOTLINE_TEST_MAILPIT_BINARY must name a Mailpit v1.31.6 binary to run the Mailpit acceptance test.")
	}
	// Other Mailpit settings must not enable a relay or forwarding in this test.
	environment := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "MP_") {
			environment = append(environment, value)
		}
	}
	environment = append(environment, "MP_DISABLE_VERSION_CHECK=true")
	versionCommand := exec.Command(binary, "version")
	versionCommand.Env = environment
	version, err := versionCommand.CombinedOutput()
	if err != nil || !strings.Contains(string(version), "v1.31.6 ") {
		t.Fatalf("expected Mailpit v1.31.6: %s %v", version, err)
	}
	for _, security := range []string{"unencrypted", "starttls"} {
		t.Run(security, func(t *testing.T) {
			directory := t.TempDir()
			smtpAddress, httpAddress := freeMailpitAddress(t), freeMailpitAddress(t)
			args := []string{"--smtp", smtpAddress, "--listen", httpAddress, "--database", filepath.Join(directory, "mailpit.db"), "--disable-version-check", "--smtp-disable-rdns"}
			var trusted *tls.Config
			if security == "starttls" {
				certPath, keyPath, config, _ := smtpTestCertificate(t)
				trusted = config
				authPath := filepath.Join(directory, "smtp-auth.txt")
				if err := os.WriteFile(authPath, []byte("barbara:mailpit-password\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--smtp-tls-cert", certPath, "--smtp-tls-key", keyPath, "--smtp-auth-file", authPath, "--smtp-require-starttls")
			}
			logPath := filepath.Join(directory, "mailpit.log")
			log, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			command := exec.Command(binary, args...)
			command.Env = environment
			command.Stdout = log
			command.Stderr = log
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
			client := &http.Client{Timeout: time.Second}
			base := "http://" + httpAddress
			deadline := time.Now().Add(5 * time.Second)
			for {
				response, err := client.Get(base + "/readyz")
				if err == nil {
					_ = response.Body.Close()
					if response.StatusCode == 200 {
						break
					}
				}
				if time.Now().After(deadline) {
					raw, _ := os.ReadFile(logPath)
					t.Fatalf("Mailpit not ready: %v %s", err, raw)
				}
				time.Sleep(20 * time.Millisecond)
			}
			// Supply a private authority through the test contact method type's TLS configuration.
			// Production contact method types always use nil, which selects system authorities.
			previous := contactMethodTypes[0]
			contactMethodTypes[0] = EmailContactMethodType{tlsConfig: trusted}
			t.Cleanup(func() { contactMethodTypes[0] = previous })
			app := foundationApp(t)
			member := memberFixture(t, app, "mailpit@example.test")
			app.WebURL = "https://mmhotline.example.com"
			if err = app.LoadSendingServiceSetups(context.Background(), nil, "http://fake.example.test"); err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{"smtp_server": smtpAddress, "smtp_connection_security": security, "email_from_address": "alerts@example.com"}
			if security == "starttls" {
				fields["smtp_user_name"] = "barbara"
				fields["smtp_password"] = "mailpit-password"
			}
			if _, err = app.SetUpSendingService(context.Background(), member.Principal, "mail_server", fields); err != nil {
				t.Fatal(err)
			}
			response, err := client.Get(base + "/api/v1/messages")
			if err != nil {
				t.Fatal(err)
			}
			var listing struct{ Total int }
			err = json.NewDecoder(response.Body).Decode(&listing)
			_ = response.Body.Close()
			if err != nil || listing.Total != 0 {
				t.Fatal("setup check sent a message", listing, err)
			}
			contact, err := app.SaveContactMethod(context.Background(), member.Principal, ContactMethodInput{ContactMethodType: "email", Fields: map[string]string{"email_address": "barbara@example.com"}, Subscriptions: []string{"info", "warn", "sos"}, SendWhileUnavailableOrMuted: true})
			if err != nil {
				t.Fatal(err)
			}
			var delivery Delivery
			delivery, err = scanDelivery(app.Store.db.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE contact_method_id=? AND purpose='contact_method_test'`, contact.ID))
			if err != nil {
				t.Fatal(err)
			}
			if work, err := app.DispatchOne(context.Background()); err != nil || !work {
				t.Fatal("outbox did not send", work, err)
			}
			var state string
			if err = app.Store.db.QueryRow(`SELECT state FROM deliveries WHERE id=?`, delivery.ID).Scan(&state); err != nil || state != "accepted" {
				t.Fatal(state, err)
			}
			response, err = client.Get(base + "/api/v1/message/latest/raw")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				raw, _ := io.ReadAll(response.Body)
				t.Fatalf("Mailpit raw API: %d %s", response.StatusCode, raw)
			}
			parsed, err := mail.ReadMessage(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, parsed.Body))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Header.Get("To") != "barbara@example.com" || subject != "MMHotline test message" || string(body) != delivery.Message || parsed.Header.Get("Message-ID") != "<"+delivery.ID+"@example.com>" {
				t.Fatalf("Mailpit message mismatch: headers=%v body=%q", parsed.Header, body)
			}
			t.Logf("Mailpit v1.31.6: %s setup, outbox acceptance, recipient, subject, body, and stable Message-ID verified", security)
		})
	}
}
