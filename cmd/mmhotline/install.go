package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmhotline/internal/app"
)

const installStateDir = "/var/lib/mmhotline"
const installCommandSocket = "/run/mmhotline/command.sock"
const installBinaryPath = "/usr/local/bin/mmhotline"
const installSettingsPath = app.InstalledSettingsFilePath
const installBackupSettingsPath = "/etc/mmhotline/backup.env"
const installBackupLitestreamConfigPath = "/etc/mmhotline/litestream.yml"

// installBackupWorkingDirectory is where Litestream keeps its working files
// for mmhotline.db. Systemd makes /var/lib/mmhotline-backup for the backup
// unit (StateDirectory=), owned by mmhotline-backup with mode 0700, so the
// mmhotline user cannot write there. Beside the database, in the state
// directory that mmhotline writes, the files would keep the owner of
// whichever user ran Litestream before, and giving them to mmhotline-backup
// as root would mean walking a directory that another user can change.
const installBackupWorkingDirectory = "/var/lib/mmhotline-backup/mmhotline.db-litestream"
const settingsFileReferenceURL = "https://github.com/monkeymaxxing/mmhotline/blob/main/docs/reference/settings-file.md"
const cloudflareAccessGuideURL = "https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/put-mmhotline-behind-cloudflare-access.md"

// installCommands is the only boundary for external programs, including discovery.
type installCommands interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
}
type systemInstallCommands struct{}

func (systemInstallCommands) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (systemInstallCommands) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// installer keeps host inputs together. Tests use a temporary root and the
// current user's IDs; production always uses the real root and UID/GID 0.
type installer struct {
	root, operatingSystem, executable string
	effectiveUID, rootUID, rootGID    int
	commands                          installCommands
	lookupUser                        func(string) (*user.User, error)
	output                            io.Writer
	people                            func(context.Context, string) (app.CommandResult, error)
	// listBackup lists the bucket in backup.env with litestream ltx.
	listBackup   func(ctx context.Context, litestream string, settings []string) (string, error)
	readyTimeout time.Duration
}

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	flags.SetOutput(&flagOutput)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), `usage: sudo mmhotline install
       Installs MMHotline on the Linux server where the command runs, with the settings in `+installSettingsPath+`.
       --help  Show help`)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(os.Stderr, flagOutput.String())
			return nil
		}
		fmt.Fprint(os.Stderr, flagOutput.String())
		return commandError{reportedFlagError{err}}
	}
	if flags.NArg() != 0 {
		return commandError{errors.New("install accepts no positional arguments; see sudo mmhotline install --help")}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	install := installer{
		operatingSystem: runtime.GOOS, effectiveUID: os.Geteuid(), executable: executable,
		commands: systemInstallCommands{}, lookupUser: user.Lookup, output: os.Stdout,
		people: func(ctx context.Context, socket string) (app.CommandResult, error) {
			return sendCommandContext(ctx, socket, app.CommandRequest{Group: "people", Action: "list", UnixUser: "root"})
		}, listBackup: listBackupWithLitestream, readyTimeout: 30 * time.Second,
	}
	return install.run(context.Background())
}

func (i *installer) path(path string) string { return filepath.Join(i.root, path) }
func (i *installer) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := i.commands.Run(ctx, name, args...)
	if err != nil {
		return output, fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (i *installer) checks() error {
	if i.operatingSystem != "linux" {
		return errors.New("install requires Linux; run sudo mmhotline install on the team's Linux server")
	}
	if i.effectiveUID != 0 {
		return errors.New("install requires root; run sudo mmhotline install")
	}
	if _, err := os.Stat(i.path("/run/systemd/system")); err != nil {
		return fmt.Errorf("install requires running systemd at /run/systemd/system; boot the server with systemd: %w", err)
	}
	fmt.Fprintln(i.output, "Checks: Linux, root, and systemd confirmed")
	return nil
}

// readSettingsFile reads the settings file, which must exist and pass both
// its owner and mode check and its static check before install changes
// anything: the service reads it at every start.
func (i *installer) readSettingsFile() (app.Settings, error) {
	path := i.path(installSettingsPath)
	guide := settingsFileReferenceURL
	if exists, err := installFileExists(path); err != nil {
		return app.Settings{}, err
	} else if !exists {
		return app.Settings{}, commandError{fmt.Errorf("install requires the settings file %s; create the settings file as root with sudoedit %s and set owner root and mode 0640; see the required settings in %s", installSettingsPath, installSettingsPath, guide)}
	}
	file, problems := app.ReadSettingsFile(path, []uint32{uint32(i.rootUID)})
	if len(problems) > 0 {
		return app.Settings{}, commandError{fmt.Errorf("the settings file %s has problems, so install changed nothing:\n%s\nFor the required settings, see %s", installSettingsPath, settingsProblemLines(problems), guide)}
	}
	return file.Settings, nil
}

func (i *installer) run(ctx context.Context) error {
	if err := i.checks(); err != nil {
		return err
	}
	databaseExists, err := installFileExists(i.path(installStateDir + "/mmhotline.db"))
	if err != nil {
		return err
	}
	settings, err := i.readSettingsFile()
	if err != nil {
		return err
	}
	account, err := i.lookupUser("mmhotline")
	var unknownUser user.UnknownUserError
	if errors.As(err, &unknownUser) {
		// The group may exist already: a deploy tool creates it before the
		// first install, so it can give the settings file the group
		// mmhotline that MMHotline reads it through. --force keeps an
		// existing group.
		if _, err = i.command(ctx, "groupadd", "--system", "--force", "mmhotline"); err != nil {
			return err
		}
		if _, err = i.command(ctx, "useradd", "--system", "--gid", "mmhotline", "--home-dir", installStateDir, "--no-create-home", "--shell", "/usr/sbin/nologin", "mmhotline"); err != nil {
			return err
		}
		account, err = i.lookupUser("mmhotline")
		fmt.Fprintln(i.output, "Unix user: created mmhotline with no login shell")
	} else if err == nil {
		fmt.Fprintln(i.output, "Unix user: mmhotline already exists")
	}
	if err != nil {
		return fmt.Errorf("look up mmhotline Unix user: %w", err)
	}
	serviceUID, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	serviceGID, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}

	triageUID, triageGID, err := i.ensureUser(ctx, "mmhotline-triage-agent", "--system", "--user-group", "--home-dir", "/var/lib/mmhotline-triage-agent", "--no-create-home", "--shell", "/usr/sbin/nologin")
	if err != nil {
		return err
	}
	// The review agent has its own user and group, so a triage run, which
	// may be running code planted in the triage agent, can neither read a
	// review run's processes nor write anything the review agent reads. A
	// review gets a new home in its run's private /tmp each time, so the
	// user has no home directory.
	if _, _, err := i.ensureUser(ctx, "mmhotline-review-agent", "--system", "--user-group", "--home-dir", "/nonexistent", "--no-create-home", "--shell", "/usr/sbin/nologin"); err != nil {
		return err
	}

	_, triageHomeErr := os.Stat(i.path("/var/lib/mmhotline-triage-agent"))
	if err := os.MkdirAll(i.path("/var/lib/mmhotline-triage-agent"), 0700); err != nil {
		return err
	}
	homeChanged, err := ensureInstallMetadata(i.path("/var/lib/mmhotline-triage-agent"), 0700, triageUID, triageGID)
	if err != nil {
		return err
	}
	i.report("Triage agent home", homeChanged || os.IsNotExist(triageHomeErr), "/var/lib/mmhotline-triage-agent")
	socketChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-triage-agent-run.socket", []byte(installTriageAgentSocketUnit), 0644)
	if err != nil {
		return err
	}
	runChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-triage-agent-run@.service", []byte(installTriageAgentRunUnit), 0644)
	if err != nil {
		return err
	}
	if err := i.ensureService(ctx, "mmhotline-triage-agent-run.socket", socketChanged || runChanged, false); err != nil {
		return err
	}
	reviewSocketChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-review-agent-run.socket", []byte(installReviewAgentSocketUnit), 0644)
	if err != nil {
		return err
	}
	reviewRunChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-review-agent-run@.service", []byte(installReviewAgentRunUnit), 0644)
	if err != nil {
		return err
	}
	if err := i.ensureService(ctx, "mmhotline-review-agent-run.socket", reviewSocketChanged || reviewRunChanged, false); err != nil {
		return err
	}
	// The fleet view units are written whatever the settings file says: with
	// them in place, adding fleet_view_command needs only settings apply.
	// Without it nothing connects to the socket.
	fleetViewSocketChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-fleet-view.socket", []byte(installFleetViewSocketUnit), 0644)
	if err != nil {
		return err
	}
	fleetViewServiceChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-fleet-view@.service", []byte(installFleetViewServiceUnit), 0644)
	if err != nil {
		return err
	}
	if err := i.ensureService(ctx, "mmhotline-fleet-view.socket", fleetViewSocketChanged || fleetViewServiceChanged, false); err != nil {
		return err
	}

	binary, err := os.ReadFile(i.executable)
	if err != nil {
		return err
	}
	binaryChanged := false
	sourceInfo, err := os.Stat(i.executable)
	if err != nil {
		return err
	}
	targetInfo, targetErr := os.Stat(i.path(installBinaryPath))
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return targetErr
	}
	if targetErr != nil || !os.SameFile(sourceInfo, targetInfo) {
		if err := os.MkdirAll(filepath.Dir(i.path(installBinaryPath)), 0755); err != nil {
			return err
		}
		binaryChanged, err = i.writeManagedFile(installBinaryPath, binary, 0755)
		if err != nil {
			return err
		}
	} else {
		binaryChanged, err = ensureInstallMetadata(i.path(installBinaryPath), 0755, i.rootUID, i.rootGID)
		if err != nil {
			return err
		}
	}
	i.report("Binary", binaryChanged, installBinaryPath)
	// Group mmhotline may write the state directory and the database files:
	// the backup's own user, in group mmhotline, writes the database and
	// SQLite's working files beside it.
	_, dataDirErr := os.Stat(i.path(installStateDir))
	if err := os.MkdirAll(i.path(installStateDir), 0770); err != nil {
		return err
	}
	changed, err := ensureInstallMetadata(i.path(installStateDir), 0770, serviceUID, serviceGID)
	if err != nil {
		return err
	}
	i.report("Data directory", changed || os.IsNotExist(dataDirErr), installStateDir)
	if !databaseExists {
		if err := app.Setup(ctx, i.path(installStateDir), settings.TeamName); err != nil {
			return err
		}
		// The marker stays until the backup's bucket is checked, so a
		// later install does not take this new database for a restored one.
		if err := os.WriteFile(i.path(installNewDatabaseMarkerPath), []byte("This database was created by mmhotline install. Install checks that the backup bucket holds no earlier backup before it starts the backup, then removes this file.\n"), 0600); err != nil {
			return err
		}
		fmt.Fprintln(i.output, "Database: created mmhotline.db for", strings.TrimSpace(settings.TeamName))
	} else {
		fmt.Fprintln(i.output, "Database: mmhotline.db already exists; kept")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := i.path(installStateDir + "/mmhotline.db" + suffix)
		if exists, err := installFileExists(path); err != nil {
			return err
		} else if exists {
			if _, err := ensureInstallMetadata(path, 0660, serviceUID, serviceGID); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(i.path("/etc/mmhotline"), 0755); err != nil {
		return err
	}
	// MMHotline's own user reads the settings file through its group; the
	// group exists only once install has created the user.
	settingsChanged, err := ensureInstallMetadata(i.path(installSettingsPath), 0640, i.rootUID, serviceGID)
	if err != nil {
		return err
	}
	if settingsChanged {
		fmt.Fprintln(i.output, "Settings file: set owner root, group mmhotline, and mode 0640, so MMHotline can read the settings file and only root can change the settings file:", installSettingsPath)
	} else {
		fmt.Fprintln(i.output, "Settings file: already has owner root, group mmhotline, and mode 0640:", installSettingsPath)
	}
	// The checksum makes upgrades detectable even when this command is running
	// from the installed path. No extra state file or database write is needed.
	unit := installServiceUnit(fmt.Sprintf("%x", sha256.Sum256(binary)))
	unitChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline.service", []byte(unit), 0644)
	if err != nil {
		return err
	}
	if err := i.ensureService(ctx, "mmhotline", unitChanged, binaryChanged || !databaseExists || settingsChanged); err != nil {
		return i.serviceError(ctx, err)
	}
	result, err := i.waitForService(ctx)
	if err != nil {
		return err
	}
	if err := i.backup(ctx); err != nil {
		return err
	}
	fmt.Fprintln(i.output, "MMHotline:", result.WebURL)
	// An http://localhost web URL means the team runs without Cloudflare.
	if settings.CloudflareAccess == nil && strings.HasPrefix(result.WebURL, "https://") {
		fmt.Fprintf(i.output, "Next: set up cloudflared and a Cloudflare Access application for %s, add cloudflare_access to %s, and apply the file, with your deploy tool or with sudo mmhotline settings apply (%s)\n", result.WebURL, installSettingsPath, cloudflareAccessGuideURL)
	}
	return nil
}

func (i *installer) report(step string, changed bool, path string) {
	if changed {
		fmt.Fprintf(i.output, "%s: installed or corrected %s\n", step, path)
	} else {
		fmt.Fprintf(i.output, "%s: already correct (%s)\n", step, path)
	}
}

// ensureUser looks up the Unix user name and, when it is missing, creates it
// with useradd's arguments. It returns the user's IDs.
func (i *installer) ensureUser(ctx context.Context, name string, useraddArgs ...string) (uid, gid int, err error) {
	account, err := i.lookupUser(name)
	var unknownUser user.UnknownUserError
	if errors.As(err, &unknownUser) {
		if _, err = i.command(ctx, "useradd", append(useraddArgs, name)...); err != nil {
			return 0, 0, err
		}
		account, err = i.lookupUser(name)
		fmt.Fprintln(i.output, "Unix user: created", name, "with no login shell")
	} else if err == nil {
		fmt.Fprintln(i.output, "Unix user:", name, "already exists")
	}
	if err != nil {
		return 0, 0, fmt.Errorf("look up %s Unix user: %w", name, err)
	}
	if uid, err = strconv.Atoi(account.Uid); err != nil {
		return 0, 0, err
	}
	gid, err = strconv.Atoi(account.Gid)
	return uid, gid, err
}
func installFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s must be a regular file; move the existing path aside before installing", path)
	}
	return true, nil
}

// installMetadataChecked runs between ensureInstallMetadata's check of a file
// and its change; a test puts a symbolic link at the path there.
var installMetadataChecked = func(string) {}

// ensureInstallMetadata gives the file or directory at path its owner and
// mode. It checks and changes the one file it opened, never the path again:
// the mmhotline user can rename entries in its state directory while root
// installs, and a symbolic link put at path would lead a change by path to
// another file. O_NOFOLLOW refuses a symbolic link; O_NONBLOCK keeps the open
// from waiting on a named pipe, which the type check then refuses.
func ensureInstallMetadata(path string, mode os.FileMode, uid, gid int) (bool, error) {
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ELOOP) {
		return false, fmt.Errorf("%s must not be a symbolic link; move the symbolic link aside before installing", path)
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(descriptor), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return false, fmt.Errorf("%s must be a regular file or a directory; move the existing path aside before installing", path)
	}
	installMetadataChecked(path)
	changed := false
	owner := info.Sys().(*syscall.Stat_t)
	if int(owner.Uid) != uid || int(owner.Gid) != gid {
		if err := file.Chown(uid, gid); err != nil {
			return false, err
		}
		changed = true
	}
	if info.Mode().Perm() != mode {
		if err := file.Chmod(mode); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}
func (i *installer) writeManagedFile(path string, contents []byte, mode os.FileMode) (bool, error) {
	path = i.path(path)
	exists, err := installFileExists(path)
	if err != nil {
		return false, err
	}
	if exists {
		current, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if bytes.Equal(current, contents) {
			return ensureInstallMetadata(path, mode, i.rootUID, i.rootGID)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mmhotline-install-*")
	if err != nil {
		return false, err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	if err := file.Chown(i.rootUID, i.rootGID); err != nil {
		return false, err
	}
	if err := file.Chmod(mode); err != nil {
		return false, err
	}
	if _, err := file.Write(contents); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

func (i *installer) ensureService(ctx context.Context, name string, unitChanged, restart bool) error {
	unitMessage := "unit already correct"
	if unitChanged {
		if _, err := i.command(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		unitMessage = "unit updated"
	}
	enabled, enabledErr := i.commands.Run(ctx, "systemctl", "is-enabled", name)
	enableMessage := "already enabled"
	if enabledErr != nil || strings.TrimSpace(string(enabled)) != "enabled" {
		if _, err := i.command(ctx, "systemctl", "enable", name); err != nil {
			return err
		}
		enableMessage = "enabled"
	}
	_, activeErr := i.commands.Run(ctx, "systemctl", "is-active", "--quiet", name)
	runningMessage := "already running"
	if unitChanged || restart || activeErr != nil {
		if _, err := i.command(ctx, "systemctl", "restart", name); err != nil {
			return err
		}
		runningMessage = "restarted"
	}
	fmt.Fprintf(i.output, "Service: %s %s; %s; %s\n", name, unitMessage, enableMessage, runningMessage)
	return nil
}
func (i *installer) waitForService(ctx context.Context) (app.CommandResult, error) {
	readyCtx, cancel := context.WithTimeout(ctx, i.readyTimeout)
	defer cancel()
	var lastErr error
	for {
		result, err := i.people(readyCtx, i.path(installCommandSocket))
		if err == nil {
			return result, nil
		}
		lastErr = err
		select {
		case <-readyCtx.Done():
			return app.CommandResult{}, i.serviceError(ctx, fmt.Errorf("mmhotline command socket did not answer within %s: %w", i.readyTimeout, lastErr))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// serviceError reads diagnostics outside the expired readiness deadline.
func (i *installer) serviceError(ctx context.Context, cause error) error {
	journalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	journal, err := i.commands.Run(journalCtx, "journalctl", "-u", "mmhotline", "--no-pager", "-n", "30")
	if err != nil {
		return fmt.Errorf("%w\nCannot read the mmhotline service journal: %v\n%s", cause, err, journal)
	}
	return fmt.Errorf("%w\nLast mmhotline service journal lines:\n%s", cause, journal)
}

// backupDocumentationURL is the page that says how to set up, check, and
// restore the backup.
const backupDocumentationURL = "https://github.com/monkeymaxxing/mmhotline/blob/main/docs/how-to/back-up-and-restore-the-database.md"

// installNewDatabaseMarkerPath marks a database that install created and
// whose backup bucket it has not yet checked. It lives in the state
// directory, so the restore block, which replaces that directory, and a
// deploy tool that restores the database before install never carry it.
const installNewDatabaseMarkerPath = installStateDir + "/new-database-waits-for-the-backup-bucket-check"

// backup sets up the backup service. For a database that install created,
// a bucket that already holds a backup means a new server: the backup then
// stays stopped, on this run and every later one until the restore or a
// bucket folder that holds no backup, because Litestream would save the
// empty database as the bucket's newest state, and the restore would bring
// back nothing. A database that install did not create, such as one a
// deploy tool restored, is backed up as usual.
func (i *installer) backup(ctx context.Context) error {
	exists, err := installFileExists(i.path(installBackupSettingsPath))
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintln(i.output, "Backup: not set up; see", backupDocumentationURL+"#set-up")
		return nil
	}
	litestream, err := i.commands.LookPath("litestream")
	if err != nil {
		return errors.New("Backup: /etc/mmhotline/backup.env exists but litestream is missing; install Litestream 0.5.18 as " + backupDocumentationURL + "#set-up says, and rerun sudo mmhotline install")
	}
	// Litestream runs as its own user, so mmhotline serve cannot read the
	// bucket keys in Litestream's process. Its group is mmhotline, which
	// may write the database; files Litestream creates get that group.
	if _, _, err := i.ensureUser(ctx, "mmhotline-backup", "--system", "--gid", "mmhotline", "--home-dir", "/nonexistent", "--no-create-home", "--shell", "/usr/sbin/nologin"); err != nil {
		return err
	}
	// Litestream, running as mmhotline-backup, reads the file, and only root
	// changes it. The file holds no credential.
	configChanged, err := i.writeManagedFile(installBackupLitestreamConfigPath, []byte(installBackupLitestreamConfig), 0644)
	if err != nil {
		return err
	}
	unitChanged, err := i.writeManagedFile("/etc/systemd/system/mmhotline-backup.service", []byte(installBackupUnit(litestream)), 0644)
	if err != nil {
		return err
	}
	newDatabase, err := installFileExists(i.path(installNewDatabaseMarkerPath))
	if err != nil {
		return err
	}
	if newDatabase {
		settings, err := readEnvironmentFile(i.path(installBackupSettingsPath))
		if err != nil {
			return err
		}
		listing, err := i.listBackup(ctx, litestream, settings)
		if err != nil {
			return fmt.Errorf("Backup: not started: this server's database is new, and MMHotline could not list the bucket in MMHOTLINE_BACKUP_REPLICA_URL to check that it holds no backup of an earlier server: %s. Fix %s, then run sudo mmhotline install again. If the bucket holds a backup, restore it as %s#restore says; to start empty, set MMHOTLINE_BACKUP_REPLICA_URL to a folder that holds no backup", hideSettingValues(err.Error(), settings), installBackupSettingsPath, backupDocumentationURL)
		}
		if newest, _ := newestUploadedTransaction(listing); newest != 0 {
			return fmt.Errorf("Backup: not started: this server's database is new, and the bucket in MMHOTLINE_BACKUP_REPLICA_URL already holds a backup, up to transaction %016x. Starting the backup now would save the empty database as the newest backup. To bring the backup back, restore it as %s#restore says; the restore starts the backup. To start empty instead, set MMHOTLINE_BACKUP_REPLICA_URL in %s to a folder that holds no backup, such as s3://BUCKET/mmhotline-new, and run sudo mmhotline install again", newest, backupDocumentationURL, installBackupSettingsPath)
		}
		if err := os.Remove(i.path(installNewDatabaseMarkerPath)); err != nil {
			return err
		}
	}
	if err := i.ensureService(ctx, "mmhotline-backup", unitChanged, configChanged); err != nil {
		return err
	}
	fmt.Fprintln(i.output, "Backup: Litestream service configured; check that uploads keep up with sudo mmhotline backup check-uploads")
	return nil
}

// installServiceRestrictions takes from both services what neither uses, so a
// fault in MMHotline or Litestream reaches less of the server. Resource limit
// calls (@resources) stay allowed: custom schedule children set their own.
// /proc/sys stays visible (no ProcSubset=pid), read-only through
// ProtectKernelTunables: MMX, which runs in MMHotline's service, reads the
// server's boot ID at /proc/sys/kernel/random/boot_id to tell a restart of
// the server from a lost run, and without it every run that changes a
// guarded file, such as a WireGuard peer list, fails as unconfirmed.
const installServiceRestrictions = `NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/mmhotline
ProtectHome=yes
PrivateTmp=yes
CapabilityBoundingSet=
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
`

// StartLimitIntervalSec=0 in both units: systemd otherwise stops restarting
// a service that failed 5 times in 10 seconds, and a fault that repeats at
// every start would leave MMHotline, intake included, or the backup down
// for good. With RestartSec=2, systemd tries again every 2 seconds.
//
// SystemCallFilter=seccomp, in MMHotline's unit only: the custom schedule
// child loads its own seccomp filter, and @system-service leaves out the
// seccomp system call, so without it every custom schedule fails with
// "failed loading seccomp filter: operation not permitted".
func installServiceUnit(binarySHA256 string) string {
	return `# Managed by mmhotline install. Binary SHA256: ` + binarySHA256 + `
[Unit]
Description=MMHotline on-call service
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=notify
User=mmhotline
ExecStart=/usr/local/bin/mmhotline serve --state-dir /var/lib/mmhotline --settings-file ` + installSettingsPath + ` --command-socket /run/mmhotline/command.sock --triage-agent-run-socket /run/mmhotline-triage-agent/run.sock --review-agent-run-socket /run/mmhotline-review-agent/run.sock --triage-agent-proxy-socket /run/mmhotline/triage-agent-proxy.sock --fleet-view-socket /run/mmhotline-fleet-view.sock
RuntimeDirectory=mmhotline
RuntimeDirectoryMode=0700
UMask=0077
Restart=always
RestartSec=2
` + installServiceRestrictions + `SystemCallFilter=seccomp

[Install]
WantedBy=multi-user.target
`
}

// installBackupLitestreamConfig is Litestream's configuration for the backup
// unit. Only a configuration file sets where Litestream keeps its working
// files (meta-path); the replicate command's arguments cannot. Litestream
// puts the replica URL from backup.env in place of the variable when it
// reads the file. The double quotes keep a URL's ?, &, and # in one YAML
// string; a URL never holds an unencoded double quote or backslash.
const installBackupLitestreamConfig = `# Managed by mmhotline install.
dbs:
  - path: ` + installStateDir + `/mmhotline.db
    meta-path: ` + installBackupWorkingDirectory + `
    replica:
      url: "${MMHOTLINE_BACKUP_REPLICA_URL}"
`

func installBackupUnit(litestreamPath string) string {
	// systemd parses quotes, specifiers (%) and environment expansion ($), even
	// without a shell. Quote the discovered executable and escape both markers.
	executable := strconv.Quote(strings.ReplaceAll(strings.ReplaceAll(litestreamPath, "%", "%%"), "$", "$$"))
	return `# Managed by mmhotline install.
[Unit]
Description=MMHotline continuous database backup with Litestream
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
User=mmhotline-backup
Group=mmhotline
EnvironmentFile=/etc/mmhotline/backup.env
StateDirectory=mmhotline-backup
StateDirectoryMode=0700
ExecStart=` + executable + ` replicate -config ` + installBackupLitestreamConfigPath + `
UMask=0077
Restart=always
RestartSec=2
` + installServiceRestrictions + `
[Install]
WantedBy=multi-user.target
`
}

const installTriageAgentSocketUnit = `# Managed by mmhotline install.
[Unit]
Description=MMHotline triage agent runs: one sandboxed run per connection from MMHotline

[Socket]
ListenStream=/run/mmhotline-triage-agent/run.sock
SocketUser=mmhotline
SocketGroup=mmhotline
SocketMode=0600
Accept=yes
MaxConnections=4

[Install]
WantedBy=sockets.target
`

// Systemd switches users because MMHotline has no privileges; each run gets
// its own control group. The proxy socket is the only way out. JavaScript
// engines need executable writable memory, so MemoryDenyWriteExecute is omitted.
// The one-hour ceiling stops a hung agent from holding the triage session.
// A failed instance is collected, in [Unit] where systemd reads it: a run
// that fails exits non-zero, and one at the ceiling is killed, and each
// would otherwise stay as a failed unit that monitoring reports.
const installTriageAgentRunUnit = `# Managed by mmhotline install.
[Unit]
Description=One sandboxed MMHotline triage agent run
CollectMode=inactive-or-failed

[Service]
User=mmhotline-triage-agent
Group=mmhotline-triage-agent
ExecStart=/usr/local/bin/mmhotline triage-agent run-inside-sandbox --proxy-socket /run/mmhotline-triage-agent-proxy.sock
StandardInput=socket
StandardOutput=socket
StandardError=journal
Environment=HOME=/var/lib/mmhotline-triage-agent
WorkingDirectory=/var/lib/mmhotline-triage-agent
UMask=0077
RuntimeMaxSec=3600
PrivateNetwork=yes
BindPaths=/run/mmhotline/triage-agent-proxy.sock:/run/mmhotline-triage-agent-proxy.sock
InaccessiblePaths=-/var/lib/mmhotline -/etc/mmhotline -/run/mmhotline
ReadWritePaths=/var/lib/mmhotline-triage-agent
` + installAgentRunRestrictions

const installReviewAgentSocketUnit = `# Managed by mmhotline install.
[Unit]
Description=MMHotline review agent runs: one sandboxed run per connection from MMHotline

[Socket]
ListenStream=/run/mmhotline-review-agent/run.sock
SocketUser=mmhotline
SocketGroup=mmhotline
SocketMode=0600
Accept=yes
MaxConnections=4

[Install]
WantedBy=sockets.target
`

// A review agent run has a triage agent run's sandbox under the review
// agent's own user, which shares nothing writable with the triage agent: the
// review gets a new home in its run's private /tmp and writes nowhere else.
// The triage agent's home is hidden from it as well.
const installReviewAgentRunUnit = `# Managed by mmhotline install.
[Unit]
Description=One sandboxed MMHotline review agent run
CollectMode=inactive-or-failed

[Service]
User=mmhotline-review-agent
Group=mmhotline-review-agent
ExecStart=/usr/local/bin/mmhotline triage-agent run-inside-sandbox --proxy-socket /run/mmhotline-triage-agent-proxy.sock
StandardInput=socket
StandardOutput=socket
StandardError=journal
UMask=0077
RuntimeMaxSec=3600
PrivateNetwork=yes
BindPaths=/run/mmhotline/triage-agent-proxy.sock:/run/mmhotline-triage-agent-proxy.sock
InaccessiblePaths=-/var/lib/mmhotline -/etc/mmhotline -/run/mmhotline -/var/lib/mmhotline-triage-agent
` + installAgentRunRestrictions

// installAgentRunRestrictions are the restrictions of both agents' runs.
const installAgentRunRestrictions = `NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
`

const installFleetViewSocketUnit = `# Managed by mmhotline install.
[Unit]
Description=MMHotline's fleet view: one run of the settings file's fleet_view_command per connection from MMHotline

[Socket]
ListenStream=/run/mmhotline-fleet-view.sock
SocketUser=mmhotline
SocketGroup=mmhotline
SocketMode=0600
Accept=yes
MaxConnections=2

[Install]
WantedBy=sockets.target
`

// The fleet view command runs as root because the monitoring tool's fleet
// view admits only root, and MMHotline's own service has NoNewPrivileges and
// no sudo. mmhotline run-fleet-view-command reads the command from the
// settings file, which only root can change, and runs it with no shell. The
// command only reads: the file system stays read-only, MMHotline's database,
// Litestream's copies of its changes, and backup.env are hidden, and only
// Unix sockets are allowed, because the fleet view comes from a local socket. The settings file stays readable,
// because run-fleet-view-command reads it; the command, the team's own root
// program, could read it too, but has no network to send it anywhere. The 20-second ceiling, twice MMHotline's wait, ends a hung
// command; MaxConnections=2 lets one more start while a timed-out one is
// still being ended, and no more. A failed instance is collected, so the
// runs every 10 seconds never pile up as failed units.
var installFleetViewServiceUnit = `# Managed by mmhotline install.
[Unit]
Description=One run of MMHotline's fleet view command, as root
CollectMode=inactive-or-failed

[Service]
ExecStart=/usr/local/bin/mmhotline run-fleet-view-command --settings-file ` + installSettingsPath + `
StandardInput=socket
StandardOutput=socket
StandardError=journal
UMask=0077
RuntimeMaxSec=20
PrivateNetwork=yes
InaccessiblePaths=-/var/lib/mmhotline -/var/lib/mmhotline-backup -/var/lib/mmhotline-triage-agent -/etc/mmhotline/backup.env -/run/mmhotline
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
CapabilityBoundingSet=
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
`
