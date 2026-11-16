package triageagent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A ChatGPT sign-in is the whole content of Codex's auth.json after codex
// login: docs/changing-mmhotline/design/triage-agent.md#codex-with-a-chatgpt-sign-in. MMHotline
// keeps the newest copy and lends it to each run; the run writes it to
// $CODEX_HOME/auth.json, and sends it back each time Codex refreshes it.
// The field names follow Codex's own (openai/codex rust-v0.166.0,
// login/src/auth/storage.rs and login/src/token_data.rs).

// MaximumChatGPTSignInSize bounds a ChatGPT sign-in. Real files are a few
// KiB; 64 KiB keeps one far below the run socket's 1 MiB line.
const MaximumChatGPTSignInSize = 64 << 10

// ChatGPTSignInChangedEvent is the run's event with a changed auth.json.
const ChatGPTSignInChangedEvent = "chatgpt_sign_in_changed"

// chatGPTSignInCheckInterval is how often a run reads auth.json while Codex
// runs. A run killed during a refresh loses at most this much: Codex's new
// refresh token is sent within a second of being written.
var chatGPTSignInCheckInterval = time.Second

// chatGPTSignIn is what MMHotline reads of an auth.json. It stores the text
// as received, so fields that newer Codex versions add survive.
type chatGPTSignIn struct {
	AuthMode            *string         `json:"auth_mode"`
	OpenAIAPIKey        *string         `json:"OPENAI_API_KEY"`
	PersonalAccessToken json.RawMessage `json:"personal_access_token"`
	BedrockAPIKey       json.RawMessage `json:"bedrock_api_key"`
	BedrockAccessKeys   json.RawMessage `json:"bedrock_access_keys"`
	Tokens              *struct {
		IDToken      string  `json:"id_token"`
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		AccountID    *string `json:"account_id"`
		LoginOwnerID *string `json:"login_owner_id"`
	} `json:"tokens"`
	LastRefresh *string `json:"last_refresh"`
	// Read from the fields above.
	userID      string
	lastRefresh time.Time
}

// parseChatGPTSignIn reads text as an auth.json that Codex uses as a ChatGPT
// sign-in. Its errors never quote the content.
func parseChatGPTSignIn(text string) (chatGPTSignIn, error) {
	var signIn chatGPTSignIn
	if len(text) > MaximumChatGPTSignInSize {
		return signIn, fmt.Errorf("is larger than %d KiB", MaximumChatGPTSignInSize>>10)
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&signIn); err != nil || decoder.Decode(new(any)) != io.EOF {
		return signIn, errors.New("is not one JSON object with the shape of Codex's auth.json")
	}
	if signIn.Tokens == nil {
		return signIn, errors.New("has no tokens; it must come from codex login with a ChatGPT account")
	}
	for _, field := range []struct{ name, value string }{
		{"tokens.id_token", signIn.Tokens.IDToken}, {"tokens.access_token", signIn.Tokens.AccessToken},
		{"tokens.refresh_token", signIn.Tokens.RefreshToken}, {"tokens.account_id", stringValue(signIn.Tokens.AccountID)},
		{"last_refresh", stringValue(signIn.LastRefresh)},
	} {
		if strings.TrimSpace(field.value) == "" {
			return signIn, fmt.Errorf("lacks %s", field.name)
		}
	}
	// Codex reads a file without auth_mode as an API key or another kind of
	// sign-in when it holds one (AuthDotJson::resolved_mode).
	if signIn.AuthMode != nil && *signIn.AuthMode != "chatgpt" {
		return signIn, errors.New("has an auth_mode other than chatgpt")
	}
	if signIn.AuthMode == nil && (stringValue(signIn.OpenAIAPIKey) != "" || jsonSet(signIn.PersonalAccessToken) || jsonSet(signIn.BedrockAPIKey) || jsonSet(signIn.BedrockAccessKeys)) {
		return signIn, errors.New("has no auth_mode and holds another kind of credential, so Codex would not use it as a ChatGPT sign-in")
	}
	var err error
	if signIn.lastRefresh, err = time.Parse(time.RFC3339Nano, *signIn.LastRefresh); err != nil {
		return signIn, errors.New("has a last_refresh that is not a time")
	}
	// The ChatGPT user ID, read and not verified: it only matches a copy to
	// the stored copy, as Codex itself reads it (parse_chatgpt_jwt_claims).
	if signIn.userID, err = chatGPTUserID(signIn.Tokens.IDToken); err != nil {
		return signIn, errors.New("has a tokens.id_token that is not a JSON web token")
	}
	return signIn, nil
}

// chatGPTUserID reads the ChatGPT user ID from an ID token's claims.
func chatGPTUserID(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", errors.New("invalid ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Auth struct {
			ChatGPTUserID string `json:"chatgpt_user_id"`
			UserID        string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Auth.ChatGPTUserID != "" {
		return claims.Auth.ChatGPTUserID, nil
	}
	return claims.Auth.UserID, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func jsonSet(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null"
}

// CheckChatGPTSignInChange refuses a copy a run sent back unless it is the
// same login as the stored copy and no older: the run's home belongs to an
// agent that a log may mislead. The reason never quotes either copy.
func CheckChatGPTSignInChange(stored, returned string) error {
	newer, err := parseChatGPTSignIn(returned)
	if err != nil {
		return fmt.Errorf("the changed auth.json %w", err)
	}
	older, err := parseChatGPTSignIn(stored)
	if err != nil {
		return fmt.Errorf("the stored ChatGPT sign-in %w", err)
	}
	switch {
	case stringValue(newer.Tokens.AccountID) != stringValue(older.Tokens.AccountID):
		return errors.New("the changed auth.json is for another ChatGPT account")
	case newer.userID != older.userID:
		return errors.New("the changed auth.json is for another ChatGPT user")
	case newer.Tokens.LoginOwnerID != nil && older.Tokens.LoginOwnerID != nil && *newer.Tokens.LoginOwnerID != *older.Tokens.LoginOwnerID:
		return errors.New("the changed auth.json is another login")
	case newer.lastRefresh.Before(older.lastRefresh):
		return errors.New("the changed auth.json has an older last_refresh than the stored copy")
	// Codex's login may store an API key from a token exchange, and a
	// refresh keeps it; a run may not add or change one.
	case stringValue(newer.OpenAIAPIKey) != "" && stringValue(newer.OpenAIAPIKey) != stringValue(older.OpenAIAPIKey):
		return errors.New("the changed auth.json adds or changes an OPENAI_API_KEY")
	}
	return nil
}

// SameChatGPTSignIn tells whether two ChatGPT sign-ins are copies of one
// login: the same refresh token, or the same login_owner_id. Two copies of
// one login end each other when either refreshes; two logins of one account
// do not.
func SameChatGPTSignIn(first, second string) bool {
	one, errOne := parseChatGPTSignIn(first)
	other, errOther := parseChatGPTSignIn(second)
	if errOne != nil || errOther != nil {
		return false
	}
	ownerOne, ownerOther := stringValue(one.Tokens.LoginOwnerID), stringValue(other.Tokens.LoginOwnerID)
	return one.Tokens.RefreshToken == other.Tokens.RefreshToken || ownerOne != "" && ownerOne == ownerOther
}

// ChatGPTSignInSecrets are the token values of a ChatGPT sign-in, which the
// run's output and reasons never show.
func ChatGPTSignInSecrets(text string) []string {
	signIn, err := parseChatGPTSignIn(text)
	if err != nil {
		return nil
	}
	var secrets []string
	for _, value := range []string{signIn.Tokens.IDToken, signIn.Tokens.AccessToken, signIn.Tokens.RefreshToken, stringValue(signIn.OpenAIAPIKey)} {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

// chatGPTSignInFile is a run's auth.json: written before Codex starts, read
// once a second while it runs and once after, each change sent to MMHotline,
// and deleted at the run's end.
type chatGPTSignInFile struct {
	path   string
	sent   []byte
	writer *eventWriter
	stop   chan struct{}
	done   chan struct{}
	// ended is set by the first end. The run's final event ends the file,
	// and a deferred end covers a run that failed before its final event;
	// a second end must not delete the auth.json of the next run, which
	// shares the agent's home.
	ended bool
}

// writeChatGPTSignIn writes signIn to $CODEX_HOME/auth.json with mode 0600,
// where CODEX_HOME is home's .codex directory.
func writeChatGPTSignIn(home, signIn string, writer *eventWriter) (*chatGPTSignInFile, error) {
	directory := filepath.Join(home, ".codex")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "auth.json")
	// A file left by a run that was killed, or one the agent put there, is
	// replaced, not written through.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = file.WriteString(signIn)
	if err = errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	return &chatGPTSignInFile{path: path, sent: []byte(signIn), writer: writer}, nil
}

// watch sends each change while Codex runs.
func (file *chatGPTSignInFile) watch() {
	file.stop, file.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(file.done)
		ticker := time.NewTicker(chatGPTSignInCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-file.stop:
				return
			case <-ticker.C:
				file.sendIfChanged()
			}
		}
	}()
}

// end stops the watch, sends a last change, and deletes the file, before
// the run's final event: MMHotline handles the change while the run still
// holds its place, so the next run gets the newest copy.
func (file *chatGPTSignInFile) end() {
	if file.ended {
		return
	}
	file.ended = true
	if file.stop != nil {
		close(file.stop)
		<-file.done
		file.stop = nil
	}
	file.sendIfChanged()
	_ = os.Remove(file.path)
}

// sendIfChanged sends the file's content when it differs from what was last
// written or sent. A file Codex is halfway through writing is sent too;
// MMHotline refuses its shape and keeps the earlier copy.
func (file *chatGPTSignInFile) sendIfChanged() {
	content, ok := readChatGPTSignInFile(file.path)
	if !ok || bytes.Equal(content, file.sent) {
		return
	}
	file.sent = content
	// A write that fails also fails the run's final event, which says so.
	_ = file.writer.write(Event{Event: ChatGPTSignInChangedEvent, ChatGPTSignIn: string(content)})
}

// readChatGPTSignInFile reads at most one byte past the size limit, so
// MMHotline sees that the file is too large. Only a regular file is read,
// and without waiting, so a pipe the agent put there cannot hold the run.
func readChatGPTSignInFile(path string) ([]byte, bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	content, err := io.ReadAll(io.LimitReader(file, MaximumChatGPTSignInSize+1))
	return content, err == nil
}
