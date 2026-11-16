package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The check reads Litestream 0.5's local directory layout and the output of
// `litestream ltx`. Whenever the documented Litestream version changes, run
// `sudo mmhotline backup check-uploads` on a server against a real bucket,
// once caught up and once with uploads stopped (a wrong key), before the
// release (docs/changing-mmhotline/design/development.md#release-gates).
//
// litestreamCapturedChangesDirectory is where Litestream keeps each change
// it captured from mmhotline.db, as a file named
// <min_txid>-<max_txid>.ltx, until the change is uploaded and compacted.
// Litestream always keeps the newest file, so the directory is empty only
// before Litestream first ran.
const litestreamCapturedChangesDirectory = installBackupWorkingDirectory + "/ltx/0"

// backedUpDatabaseFile is the database Litestream backs up. SQLite appends
// each committed transaction to its write-ahead log, the file of the same
// name with -wal added, and Litestream captures the transaction from there.
const backedUpDatabaseFile = installStateDir + "/mmhotline.db"

// backupUploadsMayLagBy is how long a captured change may wait for its
// upload before the check fails. Litestream uploads within about a second;
// five minutes leaves room for a slow upload and for a short outage that
// Litestream rides out by retrying, and still tells the team of a stopped
// backup within one 10-minute monitoring interval.
const backupUploadsMayLagBy = 5 * time.Minute

// backupCaptureWait is how long the check waits for Litestream to capture
// the database's newest change. Litestream looks at the write-ahead log
// every second, so a running Litestream captures a transaction
// within a second or two. Thirty seconds also covers a Litestream that
// systemd is restarting, while a check of a stopped backup still ends
// within a minute.
const backupCaptureWait = 30 * time.Second

var capturedChangeFileName = regexp.MustCompile(`^[0-9a-f]{16}-([0-9a-f]{16})\.ltx$`)

const backupCheckUploadsUsage = `usage: sudo mmhotline backup check-uploads [--json]
Checks whether MMHotline's continuous backup keeps up with the database.
Litestream must capture each change in the database within 30 seconds, and
the backup in object storage must hold every change Litestream captured,
except changes captured within the last 5 minutes.
Run backup check-uploads as root on the MMHotline server. The check reads
/etc/mmhotline/backup.env, the database's write-ahead log, and Litestream's
captured changes, then lists the backup with litestream ltx. The check
changes nothing.
       --json  Print one JSON object instead of one line
       --help  Show help
Exit codes: 0 the backup keeps up;
1 the backup stopped: Litestream did not capture a change in the database
within 30 seconds, or a change captured more than 5 minutes ago is not in
the backup;
3 cannot tell, such as backup not set up, not started, or not listable;
4 bad arguments.`

func runBackup(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintln(os.Stderr, backupCheckUploadsUsage)
		return nil
	}
	if len(args) == 0 || args[0] != "check-uploads" {
		return commandError{errors.New(backupCheckUploadsUsage)}
	}
	flags := flag.NewFlagSet("backup check-uploads", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	flags.Usage = func() { fmt.Fprintln(flags.Output(), backupCheckUploadsUsage) }
	asJSON := flags.Bool("json", false, "print one JSON object")
	if err := flags.Parse(args[1:]); err != nil {
		fmt.Fprint(os.Stderr, flagOutput.String())
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return commandError{reportedFlagError{err}}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("backup check-uploads accepts no positional arguments; see mmhotline backup check-uploads --help")}
	}
	check := backupUploadsCheck{settingsFile: installBackupSettingsPath, capturedChangesDirectory: litestreamCapturedChangesDirectory, databaseFile: backedUpDatabaseFile, now: time.Now(), captureWait: backupCaptureWait}
	return printBackupUploadsAnswer(os.Stdout, *asJSON, check.answer())
}

// backupUploadsCheck holds the check's inputs; tests use temporary paths.
type backupUploadsCheck struct {
	settingsFile, capturedChangesDirectory, databaseFile string
	now                                                  time.Time
	captureWait                                          time.Duration
}

// backupUploadsAnswer is the check's whole result: the printed line is
// Message, and --json prints the struct. UploadsKeepUp is null when the
// check cannot tell. Transactions are as Litestream writes them, 16
// hexadecimal digits.
type backupUploadsAnswer struct {
	exitCode                      int
	Message                       string     `json:"message"`
	UploadsKeepUp                 *bool      `json:"uploads_keep_up"`
	NewestCapturedTransaction     string     `json:"newest_captured_transaction,omitempty"`
	NewestUploadedTransaction     string     `json:"newest_uploaded_transaction,omitempty"`
	OldestMissingChangeCapturedAt *time.Time `json:"oldest_missing_change_captured_at,omitempty"`
}

func printBackupUploadsAnswer(stdout io.Writer, asJSON bool, answer backupUploadsAnswer) error {
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(answer); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(stdout, answer.Message)
	}
	if answer.exitCode == 0 {
		return nil
	}
	return checkAnswer{answer.exitCode}
}

type capturedChange struct {
	name           string
	maxTransaction uint64
	capturedAt     time.Time
}

func (check backupUploadsCheck) answer() backupUploadsAnswer {
	cannotTell := func(format string, args ...any) backupUploadsAnswer {
		return backupUploadsAnswer{exitCode: 3, Message: fmt.Sprintf(format, args...)}
	}
	guide := backupDocumentationURL + "#set-up"
	settings, err := readEnvironmentFile(check.settingsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return cannotTell("MMHotline's backup is not set up: %s is missing. To set it up, follow %s", check.settingsFile, guide)
	}
	if err != nil {
		return cannotTell("Cannot read MMHotline's backup settings: %v. Run the check as root: sudo mmhotline backup check-uploads", err)
	}
	replicaURL := ""
	for _, setting := range settings {
		if name, value, _ := strings.Cut(setting, "="); name == "MMHOTLINE_BACKUP_REPLICA_URL" {
			replicaURL = value
		}
	}
	if replicaURL == "" {
		return cannotTell("%s sets no MMHOTLINE_BACKUP_REPLICA_URL; set it as %s says", check.settingsFile, guide)
	}
	litestream, err := exec.LookPath("litestream")
	if err != nil {
		return cannotTell("%s exists but litestream is missing; install Litestream 0.5.18 as %s says", check.settingsFile, guide)
	}
	captured, err := readCapturedChanges(check.capturedChangesDirectory)
	if err != nil {
		return cannotTell("Cannot read the changes Litestream captured: %v", err)
	}
	// A running backup captures the database within seconds of starting, so
	// nothing captured means the backup service never ran, such as when
	// install was not run again after backup.env was added.
	if len(captured) == 0 {
		return cannotTell("Litestream has captured no change of the database, so the backup has not started; check it with sudo systemctl status mmhotline-backup, and run sudo mmhotline install again if the service does not exist")
	}

	// Comparing the backup with Litestream's captured changes alone would
	// pass a Litestream that keeps running but captures nothing, such as one
	// that cannot write its working files.
	databaseCaptured, err := check.databaseChangesCaptured()
	if err != nil {
		return cannotTell("Cannot compare the database with the changes Litestream captured: %v", err)
	}
	if !databaseCaptured {
		newestCaptured := fmt.Sprintf("%016x", newestCapturedTransaction(captured))
		return backupUploadsAnswer{exitCode: 1, UploadsKeepUp: new(false), NewestCapturedTransaction: newestCaptured,
			Message: fmt.Sprintf("MMHotline's backup has stopped: the database has changes that Litestream has not captured after waiting %d seconds; Litestream has captured up to transaction %s. Read why with: sudo journalctl -u mmhotline-backup", int(check.captureWait.Seconds()), newestCaptured)}
	}
	// Litestream may have captured more while the check waited.
	if captured, err = readCapturedChanges(check.capturedChangesDirectory); err != nil {
		return cannotTell("Cannot read the changes Litestream captured: %v", err)
	}
	newestCaptured := newestCapturedTransaction(captured)

	listing, err := listBackupWithLitestream(context.Background(), litestream, settings)
	if err != nil {
		return cannotTell("Cannot list MMHotline's backup: %s", hideSettingValues(err.Error(), settings))
	}
	newestUploaded, found := newestUploadedTransaction(listing)
	if !found {
		return cannotTell("Cannot read the list of MMHotline's backup: litestream ltx printed no max_txid column. The check reads the list of Litestream 0.5.18; compare litestream version with the version %s installs", guide)
	}

	answer := backupUploadsAnswer{NewestCapturedTransaction: fmt.Sprintf("%016x", newestCaptured)}
	uploadedText := "the backup holds no transaction yet"
	if newestUploaded != 0 {
		answer.NewestUploadedTransaction = fmt.Sprintf("%016x", newestUploaded)
		uploadedText = "the newest in the backup is transaction " + answer.NewestUploadedTransaction
	}
	// The check's rule: every captured change the backup lacks was captured
	// less than backupUploadsMayLagBy ago.
	var oldestMissing time.Time
	for _, change := range captured {
		if change.maxTransaction > newestUploaded && (oldestMissing.IsZero() || change.capturedAt.Before(oldestMissing)) {
			oldestMissing = change.capturedAt
		}
	}
	if oldestMissing.IsZero() {
		answer.UploadsKeepUp = new(true)
		answer.Message = fmt.Sprintf("MMHotline's backup holds every change Litestream captured, up to transaction %s.", answer.NewestCapturedTransaction)
		return answer
	}
	answer.OldestMissingChangeCapturedAt = new(oldestMissing.UTC())
	behind := max(check.now.Sub(oldestMissing), 0)
	if behind > backupUploadsMayLagBy {
		answer.exitCode, answer.UploadsKeepUp = 1, new(false)
		answer.Message = fmt.Sprintf("MMHotline's backup is missing changes captured %d minutes ago: %s; Litestream has captured up to %s. Read why with: sudo journalctl -u mmhotline-backup", int(behind.Minutes()), uploadedText, answer.NewestCapturedTransaction)
		return answer
	}
	answer.UploadsKeepUp = new(true)
	answer.Message = fmt.Sprintf("MMHotline's backup is %d seconds behind, within %d minutes: %s; Litestream has captured up to %s.", int(behind.Seconds()), int(backupUploadsMayLagBy.Minutes()), uploadedText, answer.NewestCapturedTransaction)
	return answer
}

func newestCapturedTransaction(captured []capturedChange) uint64 {
	newest := uint64(0)
	for _, change := range captured {
		newest = max(newest, change.maxTransaction)
	}
	return newest
}

// walPosition is a place in the database's write-ahead log: the end of a
// committed transaction, in the log that salt names. SQLite gives the log
// new salts each time it starts the log over from the beginning.
type walPosition struct {
	salt [2]uint32
	end  int64
}

// databaseChangesCaptured reports whether Litestream has captured the
// database's newest change, waiting up to check.captureWait for it.
//
// The newest change is normally the newest committed transaction in the
// write-ahead log. Litestream's newest captured change records, in its LTX
// header, how far into which log it captured. Litestream starts a log over
// only after capturing all of it, so a capture made during the wait from a
// log with other salts follows the transaction too.
//
// With no committed transaction in the log, the newest change is the last
// write of the database file. A running Litestream keeps a committed
// transaction in the log: it holds the log open and writes to the log right
// after it empties it. An empty or missing log therefore means the log went
// into the database file without Litestream, such as at a clean restart of
// mmhotline serve while Litestream was not connected. Litestream's newest
// captured change must then be newer than the database file.
func (check backupUploadsCheck) databaseChangesCaptured() (bool, error) {
	database, err := newestCommittedWALPosition(check.databaseFile + "-wal")
	if err != nil {
		return false, err
	}
	var databaseWrittenAt time.Time
	if database.end == 0 {
		info, err := os.Lstat(check.databaseFile)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		databaseWrittenAt = info.ModTime()
	}
	deadline := time.Now().Add(check.captureWait)
	firstTransaction := uint64(0)
	for first := true; ; first = false {
		transaction, captured, capturedAt, err := newestCapturedWALPosition(check.capturedChangesDirectory)
		if err != nil {
			return false, err
		}
		if first {
			firstTransaction = transaction
		}
		if database.end == 0 && !capturedAt.Before(databaseWrittenAt) ||
			database.end != 0 && (captured.salt == database.salt && captured.end >= database.end || captured.salt != database.salt && transaction > firstTransaction) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// newestCommittedWALPosition reads the end of the newest committed
// transaction in the write-ahead log at path. It checks each frame's salts
// and running checksum as SQLite does when it reads the log, and as
// Litestream does when it captures; the log ends at the first frame that
// fails. A missing or empty log, or one that has not started, has no
// committed transaction: the zero position.
func newestCommittedWALPosition(path string) (walPosition, error) {
	file, err := openRegularFileForCheck(path)
	if errors.Is(err, fs.ErrNotExist) {
		return walPosition{}, nil
	}
	if err != nil {
		return walPosition{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	header := make([]byte, 32)
	if _, err := io.ReadFull(reader, header); err != nil {
		return walPosition{}, endOfLog(err)
	}
	var order binary.ByteOrder
	switch binary.BigEndian.Uint32(header) {
	case 0x377f0682:
		order = binary.LittleEndian
	case 0x377f0683:
		order = binary.BigEndian
	default:
		return walPosition{}, nil
	}
	pageSize := binary.BigEndian.Uint32(header[8:])
	sum := walChecksum(order, [2]uint32{}, header[:24])
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || sum != [2]uint32{binary.BigEndian.Uint32(header[24:]), binary.BigEndian.Uint32(header[28:])} {
		return walPosition{}, nil
	}
	salt := [2]uint32{binary.BigEndian.Uint32(header[16:]), binary.BigEndian.Uint32(header[20:])}
	var newest walPosition
	frame := make([]byte, 24+pageSize)
	for end := int64(len(header)) + int64(len(frame)); ; end += int64(len(frame)) {
		if _, err := io.ReadFull(reader, frame); err != nil {
			return newest, endOfLog(err)
		}
		if salt != [2]uint32{binary.BigEndian.Uint32(frame[8:]), binary.BigEndian.Uint32(frame[12:])} {
			return newest, nil
		}
		sum = walChecksum(order, walChecksum(order, sum, frame[:8]), frame[24:])
		if sum != [2]uint32{binary.BigEndian.Uint32(frame[16:]), binary.BigEndian.Uint32(frame[20:])} {
			return newest, nil
		}
		// A frame with the database's size after the commit ends a transaction.
		if binary.BigEndian.Uint32(frame[4:]) != 0 {
			newest = walPosition{salt: salt, end: end}
		}
	}
}

// endOfLog ends the log at a short read, as SQLite does at a partly written
// frame, and returns any other read error.
func endOfLog(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

// walChecksum continues SQLite's write-ahead log checksum over data.
func walChecksum(order binary.ByteOrder, sum [2]uint32, data []byte) [2]uint32 {
	for i := 0; i+8 <= len(data); i += 8 {
		sum[0] += order.Uint32(data[i:]) + sum[1]
		sum[1] += order.Uint32(data[i+4:]) + sum[0]
	}
	return sum
}

// newestCapturedWALPosition reads the newest captured change's transaction
// and, from its LTX header, the end of the log Litestream captured it to.
// Litestream 0.5.18's LTX header (github.com/superfly/ltx 0.5.2) is 100
// bytes, starting with LTX1, with the log's offset at byte 48, the size
// captured at byte 56, and the log's two salts at bytes 64 and 68. Litestream
// may remove an older file while the check lists the directory, so a file
// gone before it opens is listed again.
func newestCapturedWALPosition(directory string) (transaction uint64, position walPosition, capturedAt time.Time, err error) {
	for {
		captured, err := readCapturedChanges(directory)
		if err != nil {
			return 0, walPosition{}, time.Time{}, err
		}
		if len(captured) == 0 {
			return 0, walPosition{}, time.Time{}, fmt.Errorf("%s holds no captured change", directory)
		}
		newest := slices.MaxFunc(captured, func(a, b capturedChange) int { return cmp.Compare(a.maxTransaction, b.maxTransaction) })
		path := filepath.Join(directory, newest.name)
		file, err := openRegularFileForCheck(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, walPosition{}, time.Time{}, err
		}
		header := make([]byte, 100)
		_, err = io.ReadFull(file, header)
		file.Close()
		if err != nil || string(header[:4]) != "LTX1" {
			return 0, walPosition{}, time.Time{}, fmt.Errorf("%s has no LTX header", path)
		}
		position = walPosition{
			salt: [2]uint32{binary.BigEndian.Uint32(header[64:]), binary.BigEndian.Uint32(header[68:])},
			end:  int64(binary.BigEndian.Uint64(header[48:]) + binary.BigEndian.Uint64(header[56:])),
		}
		return newest.maxTransaction, position, newest.capturedAt, nil
	}
}

// openRegularFileForCheck opens a regular file for reading without following
// a symbolic link or waiting on a named pipe. The files the check reads, as
// root, are in directories that the mmhotline and mmhotline-backup users
// can write.
func openRegularFileForCheck(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// readCapturedChanges reads the newest transaction of each captured change
// and when Litestream wrote it. A missing directory holds no change.
func readCapturedChanges(directory string) ([]capturedChange, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var captured []capturedChange
	for _, entry := range entries {
		match := capturedChangeFileName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // Litestream removed it after its upload and compaction.
		}
		if err != nil {
			return nil, err
		}
		transaction, _ := strconv.ParseUint(match[1], 16, 64)
		captured = append(captured, capturedChange{name: entry.Name(), maxTransaction: transaction, capturedAt: info.ModTime()})
	}
	return captured, nil
}

// newestUploadedTransaction reads `litestream ltx`'s table by its header's
// column name; Litestream also logs to standard output, so the header need
// not be the first line. An empty backup has the header and no rows.
func newestUploadedTransaction(listing string) (newest uint64, found bool) {
	column := -1
	for line := range strings.Lines(listing) {
		fields := strings.Fields(line)
		if column < 0 {
			column = slices.Index(fields, "max_txid")
			continue
		}
		if column >= len(fields) || !isTransaction(fields[column]) {
			continue
		}
		transaction, _ := strconv.ParseUint(fields[column], 16, 64)
		newest = max(newest, transaction)
	}
	return newest, column >= 0
}

func isTransaction(text string) bool {
	return len(text) == 16 && strings.Trim(text, "0123456789abcdef") == ""
}

// lastErrorLine is Litestream's own error line, "Error: ...", or else the
// last line it printed.
func lastErrorLine(listingErrors, listing string, err error) string {
	lastLine := func(output, prefix string) string {
		last := ""
		for line := range strings.Lines(output) {
			if line = strings.TrimSpace(line); line != "" && strings.HasPrefix(line, prefix) {
				last = line
			}
		}
		return last
	}
	for _, line := range []string{lastLine(listingErrors, "Error: "), lastLine(listingErrors, ""), lastLine(listing, "")} {
		if line != "" {
			return line
		}
	}
	return err.Error()
}

// hideSettingValues replaces each backup setting's value with its name, so
// no credential reaches the output. Values shorter than 8 characters are
// left: they are flags and words such as a region, never a credential, and
// replacing them would garble Litestream's message.
func hideSettingValues(text string, settings []string) string {
	sorted := slices.SortedFunc(slices.Values(settings), func(a, b string) int { return len(b) - len(a) })
	for _, setting := range sorted {
		if name, value, _ := strings.Cut(setting, "="); len(value) >= 8 {
			text = strings.ReplaceAll(text, value, "$"+name)
		}
	}
	return text
}

// readEnvironmentFile reads NAME=value lines as systemd's EnvironmentFile=
// does for the lines the backup guide writes: a value may be in double or
// single quotes, lines starting with # or ; are comments, and blank lines
// are skipped. Like systemd, it skips a line that is not an assignment.
func readEnvironmentFile(path string) ([]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var settings []string
	for line := range strings.Lines(string(contents)) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !found || !environmentVariableName.MatchString(name) {
			continue
		}
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = doubleQuotedEscape.ReplaceAllString(value[1:len(value)-1], "$1")
		}
		settings = append(settings, name+"="+value)
	}
	return settings, nil
}

var environmentVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// doubleQuotedEscape matches the backslash escapes systemd removes inside
// double quotes.
var doubleQuotedEscape = regexp.MustCompile("\\\\([\"\\\\`$])")

// listBackupWithLitestream lists the backup's files with litestream ltx, for
// backup check-uploads and for install's check of a new server. Only the
// backup settings and PATH reach Litestream, as in its service. The error
// is Litestream's own error line.
func listBackupWithLitestream(ctx context.Context, litestream string, settings []string) (string, error) {
	replicaURL := ""
	for _, setting := range settings {
		if name, value, _ := strings.Cut(setting, "="); name == "MMHOTLINE_BACKUP_REPLICA_URL" {
			replicaURL = value
		}
	}
	if replicaURL == "" {
		return "", errors.New("backup.env sets no MMHOTLINE_BACKUP_REPLICA_URL")
	}
	command := exec.CommandContext(ctx, litestream, "ltx", replicaURL)
	command.Env = append([]string{"PATH=" + os.Getenv("PATH")}, settings...)
	var listing, listingErrors bytes.Buffer
	command.Stdout, command.Stderr = &listing, &listingErrors
	if err := command.Run(); err != nil {
		return "", errors.New(lastErrorLine(listingErrors.String(), listing.String(), err))
	}
	return listing.String(), nil
}
