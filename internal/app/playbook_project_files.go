package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"mmhotline/internal/actionkinds"
	"mmhotline/internal/systemsrepository"
)

// The files a playbook's node tasks copy or render from the systems
// repository, as the people who approve and the review agent read them:
// docs/changing-mmhotline/design/triage-agent.md#the-files-a-playbook-copies.

const (
	// maximumProjectFileShownBytes bounds the text of one file shown: 16 KiB
	// holds a unit file, a cron file, or a site's configuration many times
	// over. A larger file is named with its size and SHA-256, never cut,
	// because a person would approve a tail nobody showed.
	maximumProjectFileShownBytes = 16 << 10
	// maximumProjectFilesShownBytes bounds the text of all files shown, so the
	// files leave room in the review prompt (maximumReviewPromptBytes) and in
	// the kept text (maximumApprovalPlaybookTextBytes) for the playbook.
	maximumProjectFilesShownBytes = 64 << 10
	// maximumProjectFilesListed bounds the files named, such as for a
	// directory.source of a large tree; the rest are counted.
	maximumProjectFilesListed = 50
	// maximumProjectFileHashedBytes bounds a file MMHotline reads to give its
	// SHA-256; a larger one is named by its Git object.
	maximumProjectFileHashedBytes = 32 << 20
)

// projectFilesHeading starts the files' part of the text MMHotline keeps
// with a request, after the playbook's own text.
const projectFilesHeading = "Files the playbook's tasks copy or render, from the systems repository at the same commit:"

// projectFilesText says, for each file the playbook's node tasks read from
// the controller machine, its content as text at commitID, or why the
// content is not shown: a binary or large file by its size and SHA-256, a
// path outside the systems repository, or a path MMX knows only during the
// run. It is empty when the tasks read no file.
func projectFilesText(ctx context.Context, directory, commitID string, explained []byte) (string, error) {
	files, err := actionkinds.ProjectFiles(explained)
	if err != nil || len(files) == 0 {
		return "", err
	}
	var text strings.Builder
	text.WriteString(projectFilesHeading + "\n")
	shown, listed, more := 0, 0, 0
	entry := func(name, about string) {
		if listed == maximumProjectFilesListed {
			more++
			return
		}
		listed++
		fmt.Fprintf(&text, "\n--- %s (%s)\n", name, about)
	}
	for _, file := range files {
		about := file.TaskPath + " " + file.Field
		switch {
		case !file.KnownBeforeRun:
			entry(file.Written, about+": MMX knows the path only during the run, so MMHotline cannot show the file")
			continue
		case file.Absolute:
			entry(file.Written, about+": a path on MMHotline's own server, outside the systems repository; MMHotline does not show it")
			continue
		case file.Path == "":
			entry(file.Written, about+": the path leaves the systems repository, which MMX refuses")
			continue
		}
		entries, err := systemsrepository.FilesAtCommit(ctx, directory, commitID, file.Path)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			entry(file.Path, about+": no file at this path in the commit")
			continue
		}
		for _, found := range entries {
			if listed == maximumProjectFilesListed {
				more++
				continue
			}
			switch {
			case found.Mode == "120000":
				entry(found.Path, about+": a symlink, which MMX refuses as a source")
				continue
			case found.Size < 0:
				entry(found.Path, about+": a submodule, whose files MMHotline does not show")
				continue
			case found.Size > maximumProjectFileHashedBytes:
				entry(found.Path, fmt.Sprintf("%s: %d bytes, Git object %s; too large for MMHotline to show or hash", about, found.Size, found.Object))
				continue
			}
			content, err := systemsrepository.BlobContent(ctx, directory, found.Object)
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256([]byte(content))
			described := fmt.Sprintf("%s: %d bytes, SHA-256 %s", about, len(content), hex.EncodeToString(sum[:]))
			switch {
			case !utf8.ValidString(content) || strings.ContainsRune(content, 0):
				entry(found.Path, described+"; a binary file, whose content MMHotline does not show")
			case len(content) > maximumProjectFileShownBytes:
				entry(found.Path, described+fmt.Sprintf("; more than the %d KiB MMHotline shows of one file, so the commit holds its text", maximumProjectFileShownBytes>>10))
			case shown+len(content) > maximumProjectFilesShownBytes:
				entry(found.Path, described+fmt.Sprintf("; the files above already fill the %d KiB MMHotline shows, so the commit holds its text", maximumProjectFilesShownBytes>>10))
			default:
				entry(found.Path, described)
				text.WriteString(content)
				if !strings.HasSuffix(content, "\n") {
					text.WriteString("\n")
				}
				shown += len(content)
			}
		}
	}
	if more > 0 {
		fmt.Fprintf(&text, "\n--- and %d more files, which MMHotline does not list\n", more)
	}
	return text.String(), nil
}

// keptText is the text MMHotline keeps with an approval request or a review:
// the playbook's text, then the files' text. The playbook's text is cut, as
// keptPlaybookText cuts it, to leave room for the files', which is bounded,
// so a long playbook never pushes the files out of sight.
func (j playbookJudgment) keptText() string {
	if j.projectFilesText == "" {
		return j.playbookText
	}
	// The cut line, about 70 bytes, and the line break between the parts.
	room := maximumApprovalPlaybookTextBytes - len(j.projectFilesText) - 128
	return keptPlaybookTextWithin(j.playbookText, room) + "\n" + j.projectFilesText
}
