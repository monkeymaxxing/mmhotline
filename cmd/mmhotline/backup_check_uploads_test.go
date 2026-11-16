package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	fakeBackupReplicaURL       = "s3://bucket/mmhotline?endpoint=https://fsn1.example.com&region=fsn1"
	fakeBackupAccessKeyID      = "FAKEACCESSKEYID12345"
	fakeBackupSecretAccessKey  = "fake/secret+access=key"
	backupCheckTime            = "2026-10-28T12:00:00Z"
	fakeLitestreamListingStart = `#!/bin/sh
[ "$1 $2" = "ltx ` + fakeBackupReplicaURL + `" ] || { echo "Error: wrong arguments: $*" >&2; exit 1; }
[ "$LITESTREAM_ACCESS_KEY_ID $LITESTREAM_SECRET_ACCESS_KEY" = "` + fakeBackupAccessKeyID + ` ` + fakeBackupSecretAccessKey + `" ] || { echo "Error: no credentials" >&2; exit 1; }
[ -z "${MMHOTLINE_WEB_URL-}" ] || { echo "Error: MMHotline's own settings reached Litestream" >&2; exit 1; }
echo 'time=2026-10-28T12:00:00Z level=INFO msg="initialized db" path=/var/lib/mmhotline/mmhotline.db'
cat <<'LISTING'
`
)

// backupUploadsCheckFixture writes backup.env as the backup guide does,
// Litestream's captured changes with their ages, and a fake litestream
// that answers `litestream ltx` with listing only when it gets the replica
// URL and the credentials, and none of MMHotline's own settings.
func backupUploadsCheckFixture(t *testing.T, listing string, capturedAges map[string]time.Duration) backupUploadsCheck {
	t.Helper()
	return backupUploadsCheckFixtureWithLitestream(t, fakeLitestreamListingStart+listing+"LISTING\n", capturedAges)
}

func backupUploadsCheckFixtureWithLitestream(t *testing.T, litestreamScript string, capturedAges map[string]time.Duration) backupUploadsCheck {
	t.Helper()
	directory := t.TempDir()
	now, err := time.Parse(time.RFC3339, backupCheckTime)
	if err != nil {
		t.Fatal(err)
	}
	check := backupUploadsCheck{
		settingsFile:             filepath.Join(directory, "backup.env"),
		capturedChangesDirectory: filepath.Join(directory, "mmhotline.db-litestream", "ltx", "0"),
		databaseFile:             filepath.Join(directory, "state", "mmhotline.db"),
		now:                      now,
		captureWait:              time.Second,
	}
	if err := os.Mkdir(filepath.Dir(check.databaseFile), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := "# Litestream's settings for MMHotline's backup\n\nMMHOTLINE_BACKUP_REPLICA_URL=\"" + fakeBackupReplicaURL + "\"\n" +
		"LITESTREAM_ACCESS_KEY_ID=\"" + fakeBackupAccessKeyID + "\"\nLITESTREAM_SECRET_ACCESS_KEY=\"" + fakeBackupSecretAccessKey + "\"\n"
	if err := os.WriteFile(check.settingsFile, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if capturedAges != nil {
		if err := os.MkdirAll(check.capturedChangesDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, age := range capturedAges {
		path := filepath.Join(check.capturedChangesDirectory, name)
		if err := os.WriteFile(path, []byte("LTX"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	programs := filepath.Join(directory, "programs")
	if err := os.Mkdir(programs, 0o700); err != nil {
		t.Fatal(err)
	}
	if litestreamScript != "" {
		if err := os.WriteFile(filepath.Join(programs, "litestream"), []byte(litestreamScript), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", programs+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("MMHOTLINE_WEB_URL", "https://mmhotline.example.com")
	return check
}

func answerBackupUploadsCheck(t *testing.T, check backupUploadsCheck, asJSON bool) (string, int) {
	t.Helper()
	var output bytes.Buffer
	err := printBackupUploadsAnswer(&output, asJSON, check.answer())
	return output.String(), exitCode(err)
}

type backupUploadsJSON struct {
	Message                       string  `json:"message"`
	UploadsKeepUp                 *bool   `json:"uploads_keep_up"`
	NewestCapturedTransaction     string  `json:"newest_captured_transaction"`
	NewestUploadedTransaction     string  `json:"newest_uploaded_transaction"`
	OldestMissingChangeCapturedAt *string `json:"oldest_missing_change_captured_at"`
}

func answerBackupUploadsCheckAsJSON(t *testing.T, check backupUploadsCheck) (backupUploadsJSON, int) {
	t.Helper()
	output, code := answerBackupUploadsCheck(t, check, true)
	var answer backupUploadsJSON
	if err := json.Unmarshal([]byte(output), &answer); err != nil || !strings.Contains(output, `"uploads_keep_up":`) {
		t.Fatalf("--json printed %q: %v", output, err)
	}
	return answer, code
}

// Uploads keep up when the backup holds the newest captured transaction, or
// when each captured change it lacks was captured less than 5 minutes ago;
// an older missing change means uploads stopped.
func TestBackupCheckUploadsComparesCapturedChangesWithTheBackup(t *testing.T) {
	const listing = "level  min_txid          max_txid          size  created\n" +
		"0      0000000000000006  0000000000000009  4120  2026-10-28T11:40:00Z\n"
	t.Run("caught up", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, listing+"0      000000000000000a  000000000000000a  4120  2026-10-28T11:50:00Z\n", map[string]time.Duration{
			"0000000000000006-0000000000000009.ltx": 20 * time.Minute,
			"000000000000000a-000000000000000a.ltx": 10 * time.Minute,
		})
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 0 || output != "MMHotline's backup holds every change Litestream captured, up to transaction 000000000000000a.\n" {
			t.Fatalf("exit %d: %q", code, output)
		}
		answer, code := answerBackupUploadsCheckAsJSON(t, check)
		if code != 0 || answer.UploadsKeepUp == nil || !*answer.UploadsKeepUp || answer.NewestCapturedTransaction != "000000000000000a" || answer.NewestUploadedTransaction != "000000000000000a" || answer.OldestMissingChangeCapturedAt != nil {
			t.Fatalf("exit %d: %+v", code, answer)
		}
	})
	t.Run("behind by 30 seconds", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, listing, map[string]time.Duration{
			"0000000000000006-0000000000000009.ltx": 20 * time.Minute,
			"000000000000000a-000000000000000a.ltx": 30 * time.Second,
		})
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 0 || !strings.HasPrefix(output, "MMHotline's backup is 30 seconds behind, within 5 minutes") {
			t.Fatalf("exit %d: %q", code, output)
		}
		answer, code := answerBackupUploadsCheckAsJSON(t, check)
		if code != 0 || answer.UploadsKeepUp == nil || !*answer.UploadsKeepUp || answer.OldestMissingChangeCapturedAt == nil || *answer.OldestMissingChangeCapturedAt != "2026-10-28T11:59:30Z" {
			t.Fatalf("exit %d: %+v", code, answer)
		}
	})
	t.Run("a change captured 12 minutes ago is missing", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, listing, map[string]time.Duration{
			"0000000000000006-0000000000000009.ltx": 20 * time.Minute,
			"000000000000000a-000000000000000a.ltx": 12*time.Minute + 30*time.Second,
			"000000000000000b-000000000000000b.ltx": 30 * time.Second,
		})
		output, code := answerBackupUploadsCheck(t, check, false)
		want := "MMHotline's backup is missing changes captured 12 minutes ago: the newest in the backup is transaction 0000000000000009; Litestream has captured up to 000000000000000b. Read why with: sudo journalctl -u mmhotline-backup\n"
		if code != 1 || output != want {
			t.Fatalf("exit %d: %q", code, output)
		}
		answer, code := answerBackupUploadsCheckAsJSON(t, check)
		if code != 1 || answer.UploadsKeepUp == nil || *answer.UploadsKeepUp || answer.NewestUploadedTransaction != "0000000000000009" || answer.NewestCapturedTransaction != "000000000000000b" || answer.OldestMissingChangeCapturedAt == nil || *answer.OldestMissingChangeCapturedAt != "2026-10-28T11:47:30Z" {
			t.Fatalf("exit %d: %+v", code, answer)
		}
	})
	t.Run("an empty backup and a change captured 6 minutes ago", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, "level  min_txid  max_txid  size  created\n", map[string]time.Duration{
			"0000000000000001-0000000000000001.ltx": 6 * time.Minute,
		})
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 1 || !strings.HasPrefix(output, "MMHotline's backup is missing changes captured 6 minutes ago: the backup holds no transaction yet; Litestream has captured up to 0000000000000001.") {
			t.Fatalf("exit %d: %q", code, output)
		}
	})
	t.Run("the backup lists files of a higher level, with max_txid in another column", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, "max_txid          created               level  min_txid          size\n"+
			"000000000000000a  2026-10-28T11:00:00Z  1      0000000000000001  9000\n", map[string]time.Duration{
			"000000000000000a-000000000000000a.ltx": time.Hour,
		})
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 0 || !strings.Contains(output, "holds every change Litestream captured, up to transaction 000000000000000a") {
			t.Fatalf("exit %d: %q", code, output)
		}
	})
}

// Litestream keeps its newest captured change, so an empty or missing
// directory means it has captured nothing yet: nothing waits for an upload.
func TestBackupCheckUploadsBeforeLitestreamCapturedAChange(t *testing.T) {
	for name, capturedAges := range map[string]map[string]time.Duration{"missing directory": nil, "empty directory": {}} {
		t.Run(name, func(t *testing.T) {
			check := backupUploadsCheckFixtureWithLitestream(t, "#!/bin/sh\necho 'Error: not listed' >&2\nexit 1\n", capturedAges)
			output, code := answerBackupUploadsCheck(t, check, false)
			if code != 3 || !strings.HasPrefix(output, "Litestream has captured no change of the database, so the backup has not started;") {
				t.Fatalf("exit %d: %q", code, output)
			}
		})
	}
}

// The check exits 3 when it cannot tell, and never prints a credential.
func TestBackupCheckUploadsCannotTell(t *testing.T) {
	captured := map[string]time.Duration{"0000000000000001-0000000000000001.ltx": time.Hour}
	t.Run("the listing fails", func(t *testing.T) {
		check := backupUploadsCheckFixtureWithLitestream(t, `#!/bin/sh
echo 'time=2026-10-28T12:00:00Z level=INFO msg="initialized db"'
echo "Error: operation error S3: ListObjectsV2, InvalidAccessKeyId: $LITESTREAM_ACCESS_KEY_ID with $LITESTREAM_SECRET_ACCESS_KEY for $2" >&2
exit 1
`, captured)
		for _, asJSON := range []bool{false, true} {
			output, code := answerBackupUploadsCheck(t, check, asJSON)
			if code != 3 || !strings.Contains(output, "Cannot list MMHotline's backup: Error: operation error S3: ListObjectsV2, InvalidAccessKeyId: ") {
				t.Fatalf("exit %d: %q", code, output)
			}
			for _, credential := range []string{fakeBackupAccessKeyID, fakeBackupSecretAccessKey} {
				if strings.Contains(output, credential) {
					t.Fatalf("the output shows the credential %q: %q", credential, output)
				}
			}
		}
		answer, _ := answerBackupUploadsCheckAsJSON(t, check)
		if answer.UploadsKeepUp != nil {
			t.Fatalf("uploads_keep_up must be null when the check cannot tell: %+v", answer)
		}
	})
	t.Run("backup.env is missing", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, "level  min_txid  max_txid  size  created\n", captured)
		if err := os.Remove(check.settingsFile); err != nil {
			t.Fatal(err)
		}
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 3 || !strings.HasPrefix(output, "MMHotline's backup is not set up: "+check.settingsFile+" is missing. To set it up, follow "+backupDocumentationURL+"#set-up") {
			t.Fatalf("exit %d: %q", code, output)
		}
	})
	t.Run("litestream is missing", func(t *testing.T) {
		check := backupUploadsCheckFixtureWithLitestream(t, "", captured)
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 3 || !strings.Contains(output, "litestream is missing") {
			t.Fatalf("exit %d: %q", code, output)
		}
	})
	t.Run("the listing has no max_txid column", func(t *testing.T) {
		check := backupUploadsCheckFixture(t, "level  min  max  size  created\n0  0000000000000001  0000000000000001  4120  2026-10-28T11:00:00Z\n", captured)
		output, code := answerBackupUploadsCheck(t, check, false)
		if code != 3 || !strings.Contains(output, "no max_txid column") {
			t.Fatalf("exit %d: %q", code, output)
		}
	})
}

// backup.env is read as systemd's EnvironmentFile= reads it, for the lines
// the backup guide writes.
func TestReadEnvironmentFileAsSystemdDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.env")
	contents := "# comment\n; also a comment\n\n  QUOTED=\"a \\\"quoted\\\" value with \\\\ and \\$\"\nSINGLE='$kept as is'\nPLAIN = plain value \nnot an assignment\n9INVALID=x\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := readEnvironmentFile(path)
	want := []string{`QUOTED=a "quoted" value with \ and $`, "SINGLE=$kept as is", "PLAIN=plain value"}
	if err != nil || strings.Join(settings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q %v", settings, err)
	}
}

func TestBackupCheckUploadsArguments(t *testing.T) {
	for _, args := range [][]string{{"backup", "--help"}, {"backup", "check-uploads", "--help"}} {
		output, err := cliProcess(t, args...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "usage: sudo mmhotline backup check-uploads [--json]") || !strings.Contains(string(output), "3 cannot tell") {
			t.Fatalf("%v: %s %v", args, output, err)
		}
	}
	output, err := cliProcess(t, "--help").Output()
	if err != nil || !strings.Contains(string(output), "mmhotline backup check-uploads [--json]") {
		t.Fatalf("--help: %s %v", output, err)
	}
	for _, args := range [][]string{{"backup"}, {"backup", "check"}, {"backup", "check-uploads", "--unknown"}, {"backup", "check-uploads", "extra"}} {
		output, err := cliProcess(t, args...).CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 4 {
			t.Fatalf("%v: %s %v", args, output, err)
		}
	}
}

// writeCapturedChange writes a captured change as Litestream 0.5.18 does: a
// file named by its transaction, whose LTX header records the part of the
// write-ahead log the change came from, by offset, size, and the log's salts.
func writeCapturedChange(t *testing.T, directory string, transaction uint64, salt []byte, walOffset, walSize int64) {
	t.Helper()
	header := make([]byte, 100)
	copy(header, "LTX1")
	binary.BigEndian.PutUint64(header[16:], transaction)
	binary.BigEndian.PutUint64(header[24:], transaction)
	binary.BigEndian.PutUint64(header[48:], uint64(walOffset))
	binary.BigEndian.PutUint64(header[56:], uint64(walSize))
	copy(header[64:72], salt)
	path := filepath.Join(directory, fmt.Sprintf("%016x-%016x.ltx", transaction, transaction))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Error(err)
		return
	}
	// Litestream renames a finished file into place, as here.
	if err := os.WriteFile(path+".tmp", append(header, "pages"...), 0o600); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Error(err)
	}
}

// The backup must hold every change in the database, not only every change
// Litestream captured. A Litestream that keeps running but captures nothing,
// such as one that cannot write its working folder, fails the check once the
// short wait for Litestream to capture the newest change ends.
func TestBackupCheckUploadsFailsWhenTheDatabaseIsAheadOfLitestream(t *testing.T) {
	const listing = "level  min_txid          max_txid          size  created\n" +
		"0      000000000000000a  000000000000000a  4120  2026-10-28T11:50:00Z\n"
	check := backupUploadsCheckFixture(t, listing, nil)
	check.captureWait = 300 * time.Millisecond
	db, err := sql.Open("sqlite", "file:"+check.databaseFile)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	// As with mmhotline serve, the open connection keeps the write-ahead log.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE notes (body TEXT); INSERT INTO notes VALUES ('first')`); err != nil {
		t.Fatal(err)
	}
	walFile := check.databaseFile + "-wal"
	walSize := func() int64 {
		info, err := os.Stat(walFile)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	wal, err := os.ReadFile(walFile)
	if err != nil {
		t.Fatal(err)
	}
	salt, capturedEnd := wal[16:24], int64(len(wal))
	writeCapturedChange(t, check.capturedChangesDirectory, 0xa, salt, 32, capturedEnd-32)
	output, code := answerBackupUploadsCheck(t, check, false)
	if code != 0 || output != "MMHotline's backup holds every change Litestream captured, up to transaction 000000000000000a.\n" {
		t.Fatalf("Litestream captured the whole log: exit %d: %q", code, output)
	}

	// MMHotline writes, and Litestream captures nothing more.
	if _, err := db.Exec(`INSERT INTO notes VALUES ('second')`); err != nil {
		t.Fatal(err)
	}
	stopped := func(output string) bool {
		return strings.HasPrefix(output, "MMHotline's backup has stopped: the database has changes that Litestream has not captured") &&
			strings.HasSuffix(output, "Litestream has captured up to transaction 000000000000000a. Read why with: sudo journalctl -u mmhotline-backup\n")
	}
	if output, code = answerBackupUploadsCheck(t, check, false); code != 1 || !stopped(output) {
		t.Fatalf("Litestream captured nothing new: exit %d: %q", code, output)
	}
	answer, code := answerBackupUploadsCheckAsJSON(t, check)
	if code != 1 || answer.UploadsKeepUp == nil || *answer.UploadsKeepUp || answer.NewestCapturedTransaction != "000000000000000a" {
		t.Fatalf("exit %d: %+v", code, answer)
	}

	// A capture that reaches as far in a log with other salts is not a
	// capture of this log.
	writeCapturedChange(t, check.capturedChangesDirectory, 0xb, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 32, walSize()-32)
	if output, code = answerBackupUploadsCheck(t, check, false); code != 1 || !strings.HasPrefix(output, "MMHotline's backup has stopped:") {
		t.Fatalf("a capture of another log: exit %d: %q", code, output)
	}

	// Litestream captures the change during the wait, so the check goes on to
	// compare the captured changes with the backup.
	check.captureWait = 10 * time.Second
	newEnd := walSize()
	go func() {
		time.Sleep(500 * time.Millisecond)
		writeCapturedChange(t, check.capturedChangesDirectory, 0xc, salt, capturedEnd, newEnd-capturedEnd)
	}()
	if output, code = answerBackupUploadsCheck(t, check, false); code != 0 || !strings.HasPrefix(output, "MMHotline's backup is 0 seconds behind, within 5 minutes: the newest in the backup is transaction 000000000000000a; Litestream has captured up to 000000000000000c.") {
		t.Fatalf("Litestream captured during the wait: exit %d: %q", code, output)
	}

	// The file system dates a file to the clock's last tick, several
	// milliseconds, so the capture just written and the database file's write
	// below could carry the same date. Litestream captured this change well
	// before MMHotline's last write, so the test dates it so.
	capturedEarlier := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(check.capturedChangesDirectory, "000000000000000c-000000000000000c.ltx"), capturedEarlier, capturedEarlier); err != nil {
		t.Fatal(err)
	}

	// MMHotline writes and closes the database while Litestream is not
	// connected, as at a clean restart: SQLite copies the log into the
	// database file and removes the log. A capture older than the database
	// file's last write leaves the change uncaptured.
	if _, err := db.Exec(`INSERT INTO notes VALUES ('third')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(walFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the closed database kept its write-ahead log: %v", err)
	}
	check.captureWait = 300 * time.Millisecond
	if output, code = answerBackupUploadsCheck(t, check, false); code != 1 || !strings.HasPrefix(output, "MMHotline's backup has stopped: the database has changes that Litestream has not captured") {
		t.Fatalf("the change went into the database file uncaptured: exit %d: %q", code, output)
	}
	// Litestream reconnects and captures the database again.
	writeCapturedChange(t, check.capturedChangesDirectory, 0xd, []byte{8, 7, 6, 5, 4, 3, 2, 1}, 32, 0)
	if output, code = answerBackupUploadsCheck(t, check, false); code != 0 {
		t.Fatalf("Litestream captured after the database file's last write: exit %d: %q", code, output)
	}
}
