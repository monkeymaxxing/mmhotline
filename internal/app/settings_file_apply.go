package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// settingsFilePartsSchema records, for each part of the settings file, what
// applied: the fingerprint of the value in effect, when, why the latest
// attempt failed, and the notes of the latest apply, a JSON list of
// SettingsNote, for settings show. Pages and show commands read the
// settings themselves from the tables each part writes.
const settingsFilePartsSchema = `
CREATE TABLE IF NOT EXISTS settings_file_parts (
 part TEXT PRIMARY KEY,
 applied_fingerprint TEXT NOT NULL DEFAULT '',
 applied_at INTEGER NOT NULL DEFAULT 0,
 applied_values TEXT NOT NULL DEFAULT '',
 failure_reason TEXT NOT NULL DEFAULT '',
 failed_at INTEGER NOT NULL DEFAULT 0,
 notes TEXT NOT NULL DEFAULT ''
);
`

// ServerSettings are the settings serve listens and signs people in with.
// They change only at a restart (CA5).
type ServerSettings struct {
	WebURL                   string                    `json:"web_url"`
	WebListenAddress         string                    `json:"web_listen_address"`
	AlertIntakeListenAddress string                    `json:"alert_intake_listen_address"`
	CloudflareAccess         *SettingsCloudflareAccess `json:"cloudflare_access,omitempty"`
}

func (s Settings) server() ServerSettings {
	webURL, _ := ParseWebURL(s.WebURL)
	return ServerSettings{WebURL: webURL, WebListenAddress: s.WebListenAddress, AlertIntakeListenAddress: s.AlertIntakeListenAddress, CloudflareAccess: s.CloudflareAccess}
}

// settingsFileRuntime is what the running MMHotline knows of its settings
// file.
type settingsFileRuntime struct {
	path             string
	allowedOwnerUIDs []uint32
	// applying lets one apply run at a time; a second waits for it.
	applying sync.Mutex
	// atStartInTheBackground is the apply of the parts with an outside test
	// at start, which serve waits for before it closes the database.
	atStartInTheBackground sync.WaitGroup
	// atStart is the file read at start, whose parts apply once the
	// listeners are open; nil when start fell back to the settings last
	// applied.
	atStart *SettingsFile
	mu      sync.RWMutex
	// firstCommentLine is the first comment line of the file as last read.
	firstCommentLine string
	// server is what this process listens and signs in with.
	server ServerSettings
	// serverFailure is why serve could not listen with the file's server
	// settings and started with those it last applied.
	serverFailure string
	// fleetViewCommand is the applied fleet_view_command; the root fleet
	// view unit reads the file itself, so this only says whether there is
	// one.
	fleetViewCommand []string
}

// UseSettingsFile tells MMHotline where its settings file is, and which
// owners it accepts (CheckSettingsFileOwnerAndMode).
func (a *App) UseSettingsFile(path string, allowedOwnerUIDs []uint32) {
	a.settingsFile.path, a.settingsFile.allowedOwnerUIDs = path, allowedOwnerUIDs
}

// SettingsFilePath is the settings file serve was given.
func (a *App) SettingsFilePath() string { return a.settingsFile.path }

// settingsFileFirstCommentLine is the settings file's first comment line,
// as last read, which the pages show beside its path.
func (a *App) settingsFileFirstCommentLine() string {
	a.settingsFile.mu.RLock()
	defer a.settingsFile.mu.RUnlock()
	return a.settingsFile.firstCommentLine
}

// fleetViewCommand is the applied fleet_view_command, or nil.
func (a *App) fleetViewCommand() []string {
	a.settingsFile.mu.RLock()
	defer a.settingsFile.mu.RUnlock()
	return a.settingsFile.fleetViewCommand
}

// settingsFileNoticeView is what a page needs to say where its read-only
// settings are set.
type settingsFileNoticeView struct {
	SettingsFile, SettingsFileFirstLine string
}

func (a *App) settingsFileNoticeView() settingsFileNoticeView {
	return settingsFileNoticeView{a.settingsFile.path, a.settingsFileFirstCommentLine()}
}

// settingsFileNotice is template text for the line that says where a page's
// read-only settings are set: keys is the template text naming the keys, and
// the page's data embeds settingsFileNoticeView.
func settingsFileNotice(keys string) string {
	return `<p class="muted">Set in <code>{{.SettingsFile}}</code>, in ` + keys + `.{{if .SettingsFileFirstLine}} The file's first line says: {{.SettingsFileFirstLine}}{{else}} An administrator edits the settings file with sudoedit, then runs <code>sudo mmhotline settings apply</code>.{{end}}</p>`
}

func (a *App) settingsPrincipal() Principal {
	return Principal{SettingsFile: a.settingsFile.path}
}

// settingsFilePartRow is one row of settings_file_parts.
type settingsFilePartRow struct {
	appliedFingerprint, appliedValues, failureReason, notes string
	appliedAt, failedAt                                     int64
}

func readSettingsFileParts(ctx context.Context, tx *sql.Tx) (map[string]settingsFilePartRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT part,applied_fingerprint,applied_at,applied_values,failure_reason,failed_at,notes FROM settings_file_parts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := map[string]settingsFilePartRow{}
	for rows.Next() {
		var name string
		var row settingsFilePartRow
		if err := rows.Scan(&name, &row.appliedFingerprint, &row.appliedAt, &row.appliedValues, &row.failureReason, &row.failedAt, &row.notes); err != nil {
			return nil, err
		}
		parts[name] = row
	}
	return parts, rows.Err()
}

// settingsFingerprint is the SHA-256 of a part's value as JSON. It is only
// compared, never shown: a part's value can hold secrets.
func settingsFingerprint(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// SettingsFilePartOutcome is what happened to one part.
type SettingsFilePartOutcome struct {
	// Part names the part by its key paths, such as people.
	Part string `json:"part"`
	// State is applied, unchanged (apply: the value in effect already),
	// failed (the earlier setting stays), or never_applied (show).
	State         string     `json:"state"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
	// LeftOutOfTheSettingsFile says the setting in effect is the part's
	// section left out of the file, so the part is not set up, as
	// sending-services list says. For a failed part, it is the setting
	// that stays.
	LeftOutOfTheSettingsFile bool `json:"left_out_of_the_settings_file,omitempty"`
	// Notes are the file's notes about the part's keys, such as a kind of
	// action without a setting, and its next step when it applied for the
	// first time.
	Notes []SettingsNote `json:"notes,omitempty"`
}

// keyPaths are the key paths the part's name lists.
func (part settingsFilePart) keyPaths() []string { return strings.Split(part.name, ", ") }

// notesOf are the notes whose key path is one of the part's or inside one.
func (part settingsFilePart) notesOf(notes []SettingsNote) []SettingsNote {
	var own []SettingsNote
	for _, note := range notes {
		for _, path := range part.keyPaths() {
			if note.KeyPath == path || strings.HasPrefix(note.KeyPath, path+".") || strings.HasPrefix(note.KeyPath, path+"[") {
				own = append(own, note)
				break
			}
		}
	}
	return own
}

// absentFingerprint is the fingerprint of a section the file leaves out.
var absentFingerprint, _ = settingsFingerprint(nil)

// applySettingsParts applies each part in order: a part whose value is the
// one in effect, with no failure since, is not tested or written again;
// any other part is checked, tested, and written, and a failure keeps its
// earlier setting. Each part's outcome carries the file's notes about its
// keys, which settings show reads back. The caller holds
// settingsFile.applying.
func (a *App) applySettingsParts(ctx context.Context, settings Settings, notes []SettingsNote, parts []settingsFilePart) ([]SettingsFilePartOutcome, error) {
	secrets := settingsSecretValues(reflect.ValueOf(settings), false)
	var outcomes []SettingsFilePartOutcome
	for _, part := range parts {
		value, err := part.value(ctx, a, settings)
		if err != nil {
			return outcomes, err
		}
		fingerprint, err := settingsFingerprint(value)
		if err != nil {
			return outcomes, err
		}
		var row settingsFilePartRow
		err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			rows, err := readSettingsFileParts(ctx, tx)
			row = rows[part.name]
			return err
		})
		if err != nil {
			return outcomes, err
		}
		partNotes := part.notesOf(notes)
		if row.appliedFingerprint == fingerprint && row.failureReason == "" {
			if err = a.saveSettingsPartNotes(ctx, part.name, partNotes); err != nil {
				return outcomes, err
			}
			outcomes = append(outcomes, SettingsFilePartOutcome{Part: part.name, State: "unchanged", AppliedAt: unixTime(row.appliedAt), Notes: partNotes, LeftOutOfTheSettingsFile: row.appliedFingerprint == absentFingerprint})
			continue
		}
		now := a.Now().UTC()
		applyErr := part.apply(ctx, a, settings)
		var values string
		if applyErr == nil && part.keepsValues {
			encoded, err := json.Marshal(value)
			if err != nil {
				return outcomes, err
			}
			values = string(encoded)
		}
		// A part that applies with its section for the first time, or
		// again after its section was left out, shows its next step.
		if applyErr == nil && part.nextStep != "" && fingerprint != absentFingerprint && (row.appliedFingerprint == "" || row.appliedFingerprint == absentFingerprint) {
			partNotes = append(partNotes, SettingsNote{KeyPath: part.name, Note: "Next step: " + part.nextStep})
		}
		encodedNotes, err := settingsNotesJSON(partNotes)
		if err != nil {
			return outcomes, err
		}
		reason := settingsPartFailureReason(applyErr, secrets)
		err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
			if applyErr != nil {
				_, err := tx.ExecContext(ctx, `INSERT INTO settings_file_parts(part,failure_reason,failed_at,notes) VALUES(?,?,?,?)
ON CONFLICT(part) DO UPDATE SET failure_reason=excluded.failure_reason,failed_at=excluded.failed_at,notes=excluded.notes`, part.name, reason, now.Unix(), encodedNotes)
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO settings_file_parts(part,applied_fingerprint,applied_at,applied_values,failure_reason,failed_at,notes) VALUES(?,?,?,?,'',0,?)
ON CONFLICT(part) DO UPDATE SET applied_fingerprint=excluded.applied_fingerprint,applied_at=excluded.applied_at,applied_values=excluded.applied_values,failure_reason='',failed_at=0,notes=excluded.notes`, part.name, fingerprint, now.Unix(), values, encodedNotes)
			return err
		})
		if err != nil {
			return outcomes, err
		}
		if applyErr != nil {
			outcomes = append(outcomes, SettingsFilePartOutcome{Part: part.name, State: "failed", AppliedAt: unixTime(row.appliedAt), FailureReason: reason, FailedAt: &now, Notes: partNotes, LeftOutOfTheSettingsFile: row.appliedFingerprint == absentFingerprint})
			continue
		}
		outcomes = append(outcomes, SettingsFilePartOutcome{Part: part.name, State: "applied", AppliedAt: &now, Notes: partNotes, LeftOutOfTheSettingsFile: fingerprint == absentFingerprint})
	}
	return outcomes, nil
}

// saveSettingsPartNotes keeps an unchanged part's notes of this apply.
func (a *App) saveSettingsPartNotes(ctx context.Context, part string, notes []SettingsNote) error {
	encoded, err := settingsNotesJSON(notes)
	if err != nil {
		return err
	}
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE settings_file_parts SET notes=? WHERE part=?`, encoded, part)
		return err
	})
}

// settingsNotesJSON is the notes column's value: ” for none.
func settingsNotesJSON(notes []SettingsNote) (string, error) {
	if len(notes) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(notes)
	return string(encoded), err
}

// settingsPartFailureReason is why a part failed, in the operation's own
// words, with the value of every secret of the file removed: a part's apply
// never puts a secret in its error, and an outside service that quotes one
// back still never gets it shown (confirmed change 3).
func settingsPartFailureReason(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	reason := strings.TrimPrefix(refusalText(err), errSettingsPartRefused.Error()+": ")
	if errorStatus(err) == 500 && !errors.Is(err, errSettingsPartRefused) {
		reason = "MMHotline could not save the settings file part: " + err.Error()
	}
	for _, secret := range secrets {
		reason = strings.ReplaceAll(reason, secret, "[secret removed]")
	}
	return reason
}

// errSettingsPartRefused marks a part's own refusal that is not an
// operation's, such as an inventory without rules for running the agent's playbooks.
var errSettingsPartRefused = errors.New("refused")

func unixTime(seconds int64) *time.Time {
	if seconds == 0 {
		return nil
	}
	at := time.Unix(seconds, 0).UTC()
	return &at
}

// SettingsFileView is what settings apply and settings show print.
type SettingsFileView struct {
	SettingsFile     string `json:"settings_file"`
	FirstCommentLine string `json:"first_comment_line"`
	// LastAppliedAt is when a part last applied (show).
	LastAppliedAt *time.Time `json:"last_applied_at,omitempty"`
	// Problems are the file's problems; with any, nothing applied (apply).
	Problems []SettingsProblem `json:"problems,omitempty"`
	// RestartNeededFor lists the restart keys that changed; nothing applied,
	// and MMHotline must restart first (apply).
	RestartNeededFor []string `json:"restart_needed_for,omitempty"`
	// RestartedFor lists the restart keys that changed, when settings apply
	// restarted MMHotline before it applied the rest.
	RestartedFor []string                  `json:"restarted_for,omitempty"`
	Parts        []SettingsFilePartOutcome `json:"parts"`
}

// FailedParts names the parts that failed.
func (view SettingsFileView) FailedParts() []string {
	var failed []string
	for _, part := range view.Parts {
		if part.State == "failed" {
			failed = append(failed, part.Part)
		}
	}
	return failed
}

// Text is the view as the commands print it.
func (view SettingsFileView) Text() string {
	lines := []string{"Settings file: " + view.SettingsFile}
	if view.FirstCommentLine != "" {
		lines = append(lines, "Settings file first comment line: "+view.FirstCommentLine)
	}
	if view.LastAppliedAt != nil {
		lines = append(lines, "Last applied: "+commandTime(*view.LastAppliedAt))
	}
	if len(view.RestartedFor) > 0 {
		lines = append(lines, "Restarted MMHotline first, because these keys change only when it restarts: "+strings.Join(view.RestartedFor, ", ")+".")
	}
	for _, problem := range view.Problems {
		lines = append(lines, problem.String())
	}
	for _, part := range view.Parts {
		switch part.State {
		case "applied":
			if part.LeftOutOfTheSettingsFile {
				lines = append(lines, part.Part+": not set up, because the settings file leaves it out.")
			} else if view.LastAppliedAt != nil {
				lines = append(lines, fmt.Sprintf("%s: applied at %s.", part.Part, commandTime(*part.AppliedAt)))
			} else {
				lines = append(lines, part.Part+": applied.")
			}
		case "unchanged":
			lines = append(lines, part.Part+": unchanged.")
		case "never_applied":
			lines = append(lines, part.Part+": not applied yet.")
		case "failed":
			kept := "No value has ever applied for the settings file part, so the part is not set."
			if part.LeftOutOfTheSettingsFile {
				kept = "It stays not set up, as before."
			} else if part.AppliedAt != nil {
				kept = "The earlier setting, applied at " + commandTime(*part.AppliedAt) + ", stays."
			}
			for _, known := range settingsFileParts {
				if known.name == part.Part && known.failedButApplied != "" {
					kept = known.failedButApplied
				}
			}
			lines = append(lines, fmt.Sprintf("%s: did not apply: %s. %s", part.Part, strings.TrimSuffix(part.FailureReason, "."), kept))
		}
		for _, note := range part.Notes {
			lines = append(lines, "  "+note.String())
		}
	}
	return strings.Join(lines, "\n")
}

// restartKeysThatDiffer names the server keys whose values differ.
func restartKeysThatDiffer(running, file ServerSettings) []string {
	var keys []string
	for _, key := range []struct {
		name         string
		running, new any
	}{
		{"web_url", running.WebURL, file.WebURL},
		{"web_listen_address", running.WebListenAddress, file.WebListenAddress},
		{"alert_intake_listen_address", running.AlertIntakeListenAddress, file.AlertIntakeListenAddress},
		{"cloudflare_access", running.CloudflareAccess, file.CloudflareAccess},
	} {
		if !reflect.DeepEqual(key.running, key.new) {
			keys = append(keys, key.name)
		}
	}
	return keys
}

// ApplySettingsFile reads the settings file at the path serve was given,
// checks it with its owner and mode, and applies each part, for sudo
// mmhotline settings apply. A file with problems applies nothing. When a
// restart key changed, it applies nothing and names the keys: the command
// restarts MMHotline, which takes the server keys at start, and asks again.
func (a *App) ApplySettingsFile(ctx context.Context) (SettingsFileView, error) {
	a.settingsFile.applying.Lock()
	defer a.settingsFile.applying.Unlock()
	file, problems := ReadSettingsFile(a.settingsFile.path, a.settingsFile.allowedOwnerUIDs)
	view := SettingsFileView{SettingsFile: a.settingsFile.path, FirstCommentLine: file.FirstCommentLine, Problems: problems, Parts: []SettingsFilePartOutcome{}}
	if len(problems) > 0 {
		return view, nil
	}
	a.settingsFile.mu.Lock()
	a.settingsFile.firstCommentLine = file.FirstCommentLine
	running := a.settingsFile.server
	a.settingsFile.mu.Unlock()
	if view.RestartNeededFor = restartKeysThatDiffer(running, file.Settings.server()); len(view.RestartNeededFor) > 0 {
		return view, nil
	}
	var err error
	view.Parts, err = a.applySettingsParts(ctx, file.Settings, file.Notes, settingsFileParts)
	return view, err
}

// SettingsFileState is what settings show prints: each part as applied, or
// failed with why and the earlier setting kept, with the notes of the last
// apply and the part's notes from MMHotline's state.
func (a *App) SettingsFileState(ctx context.Context) (SettingsFileView, error) {
	view := SettingsFileView{SettingsFile: a.settingsFile.path, FirstCommentLine: a.settingsFileFirstCommentLine(), Parts: []SettingsFilePartOutcome{}}
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rows, err := readSettingsFileParts(ctx, tx)
		if err != nil {
			return err
		}
		var last int64
		for _, part := range settingsFileParts {
			row, found := rows[part.name]
			outcome := SettingsFilePartOutcome{Part: part.name, State: "applied", AppliedAt: unixTime(row.appliedAt), LeftOutOfTheSettingsFile: found && row.appliedAt != 0 && row.appliedFingerprint == absentFingerprint}
			switch {
			case !found || (row.appliedAt == 0 && row.failureReason == ""):
				outcome.State = "never_applied"
			case row.failureReason != "":
				outcome.State, outcome.FailureReason, outcome.FailedAt = "failed", row.failureReason, unixTime(row.failedAt)
			}
			if row.notes != "" {
				if err := json.Unmarshal([]byte(row.notes), &outcome.Notes); err != nil {
					return err
				}
			}
			if part.shownNotes != nil {
				notes, err := part.shownNotes(ctx, tx)
				if err != nil {
					return err
				}
				for _, note := range notes {
					outcome.Notes = append(outcome.Notes, SettingsNote{KeyPath: part.name, Note: note})
				}
			}
			last = max(last, row.appliedAt)
			view.Parts = append(view.Parts, outcome)
		}
		view.LastAppliedAt = unixTime(last)
		return nil
	})
	return view, err
}

// LoadSettingsFileAtStart reads the settings file as serve starts (CA7) and
// returns the server settings to listen and sign in with. When the file
// fails its owner and mode check or its static check, MMHotline starts with
// the settings it last applied and warns the administrators, so a typo never
// stops alerts; with nothing applied yet, it refuses to start and names the
// problems.
func (a *App) LoadSettingsFileAtStart(ctx context.Context) (ServerSettings, error) {
	file, problems := ReadSettingsFile(a.settingsFile.path, a.settingsFile.allowedOwnerUIDs)
	runtime := &a.settingsFile
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.firstCommentLine = file.FirstCommentLine
	if len(problems) == 0 {
		runtime.atStart = &file
		runtime.server, runtime.fleetViewCommand = file.Settings.server(), file.Settings.FleetViewCommand
		return runtime.server, nil
	}
	var server ServerSettings
	var fleetViewCommand []string
	found := false
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		server, fleetViewCommand, found, err = lastAppliedSettings(ctx, tx)
		if err != nil || !found {
			return err
		}
		var keyPaths []string
		for _, problem := range problems {
			keyPath := problem.KeyPath
			if keyPath == "" {
				keyPath = "the file as a whole, such as its owner and mode"
			}
			keyPaths = append(keyPaths, keyPath)
		}
		// The warning names the problems by key path only, so it can never
		// carry a secret's value.
		message := fmt.Sprintf("MMHotline started with the settings it last applied, because the settings file %s has problems at: %s. Alerts keep flowing with the earlier settings. To see each problem, run sudo mmhotline settings check --settings-file %s on the server; fix them, then apply the file, with your deploy tool or with sudo mmhotline settings apply.", a.settingsFile.path, strings.Join(keyPaths, "; "), a.settingsFile.path)
		return RecordAdministratorNotice(ctx, tx, "warn", "The settings file has problems; MMHotline uses its earlier settings", message, a.Now().UTC())
	})
	if err != nil {
		return ServerSettings{}, err
	}
	if !found {
		var lines []string
		for _, problem := range problems {
			lines = append(lines, problem.String())
		}
		return ServerSettings{}, fmt.Errorf("the settings file %s has problems, and MMHotline has never applied a settings file, so it cannot start:\n%s", a.settingsFile.path, strings.Join(lines, "\n"))
	}
	runtime.server, runtime.fleetViewCommand = server, fleetViewCommand
	return server, nil
}

// lastAppliedSettings reads the server settings and the fleet view command
// that last applied; found is false when no server settings applied yet.
func lastAppliedSettings(ctx context.Context, tx *sql.Tx) (server ServerSettings, fleetViewCommand []string, found bool, err error) {
	rows, err := readSettingsFileParts(ctx, tx)
	if err != nil {
		return server, nil, false, err
	}
	serverRow, fleetRow := rows[settingsServerPart.name], rows[settingsFleetViewCommandPart.name]
	if serverRow.appliedValues == "" {
		return server, nil, false, nil
	}
	if err = json.Unmarshal([]byte(serverRow.appliedValues), &server); err != nil {
		return server, nil, false, err
	}
	if fleetRow.appliedValues != "" {
		err = json.Unmarshal([]byte(fleetRow.appliedValues), &fleetViewCommand)
	}
	return server, fleetViewCommand, true, err
}

// UseLastAppliedServerSettings is serve's way out when it cannot listen
// with the file's server settings, such as on an address the server does
// not have, which would stop alerts: it starts with the server settings it
// last applied, records why the server part failed, and warns the
// administrators. found is false when none applied before.
func (a *App) UseLastAppliedServerSettings(ctx context.Context, reason string) (ServerSettings, bool, error) {
	var server ServerSettings
	found := false
	now := a.Now().UTC()
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		server, _, found, err = lastAppliedSettings(ctx, tx)
		if err != nil || !found {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE settings_file_parts SET failure_reason=?,failed_at=? WHERE part=?`, reason, now.Unix(), settingsServerPart.name); err != nil {
			return err
		}
		return RecordAdministratorNotice(ctx, tx, "warn", "MMHotline uses its earlier server settings", fmt.Sprintf("%s, so MMHotline started with the web_url, listen addresses, and cloudflare_access it last applied. Alerts keep flowing with the earlier server settings. Fix the settings file %s, then apply it, with your deploy tool or with sudo mmhotline settings apply.", reason, a.settingsFile.path), now)
	})
	if err != nil || !found {
		return server, found, err
	}
	a.settingsFile.mu.Lock()
	a.settingsFile.server, a.settingsFile.serverFailure = server, reason
	a.settingsFile.mu.Unlock()
	return server, true, nil
}

// ApplySettingsFileAtStart applies the parts of the file read at start, as
// settings apply does: the parts without an outside test now, before serve
// answers, and the others in the background, while the earlier setting of
// each serves until it passes. After a fallback it applies nothing.
func (a *App) ApplySettingsFileAtStart(ctx context.Context, report func(error)) error {
	file := a.settingsFile.atStart
	if file == nil {
		return nil
	}
	var now, later []settingsFilePart
	for _, part := range settingsFileParts {
		if part.hasOutsideTest {
			later = append(later, part)
		} else {
			now = append(now, part)
		}
	}
	a.settingsFile.applying.Lock()
	_, err := a.applySettingsParts(ctx, file.Settings, file.Notes, now)
	a.settingsFile.applying.Unlock()
	if err != nil || len(later) == 0 {
		return err
	}
	a.settingsFile.atStartInTheBackground.Go(func() {
		a.settingsFile.applying.Lock()
		defer a.settingsFile.applying.Unlock()
		if _, err := a.applySettingsParts(ctx, file.Settings, file.Notes, later); err != nil && ctx.Err() == nil {
			report(fmt.Errorf("apply the settings file: %w", err))
		}
	})
	return nil
}

// WaitForSettingsFileAppliedAtStart waits for the parts with an outside
// test that ApplySettingsFileAtStart applies in the background; serve waits
// for them after it cancels their context, before it closes the database.
func (a *App) WaitForSettingsFileAppliedAtStart() {
	a.settingsFile.atStartInTheBackground.Wait()
}

// settingsCommand answers settings apply and settings show on the command
// socket, which only root reaches.
func (a *App) settingsCommand(ctx context.Context, action string, result *CommandResult) error {
	var view SettingsFileView
	var err error
	switch action {
	case "apply":
		view, err = a.ApplySettingsFile(ctx)
	case "show":
		view, err = a.SettingsFileState(ctx)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	result.SettingsFile = &view
	failed := view.FailedParts()
	switch {
	case len(view.Problems) > 0:
		result.Message = "The settings file has problems, so nothing applied and the earlier settings stay. Fix the settings file problems, then apply the settings file again."
	case len(view.RestartNeededFor) > 0:
		result.Message = "These keys change only when MMHotline restarts: " + strings.Join(view.RestartNeededFor, ", ") + ". Nothing applied yet."
	case len(failed) == 1:
		result.Message = "1 part did not apply and keeps its earlier setting: " + failed[0] + "."
	case len(failed) > 1:
		result.Message = fmt.Sprintf("%d parts did not apply, and each keeps its earlier setting: %s.", len(failed), strings.Join(failed, "; "))
	case !slices.ContainsFunc(view.Parts, func(part SettingsFilePartOutcome) bool { return part.State == "never_applied" }):
		result.Message = "Every part of the settings file is applied."
	}
	return nil
}
