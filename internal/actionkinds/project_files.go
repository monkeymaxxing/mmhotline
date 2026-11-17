package actionkinds

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The files of the systems repository that a playbook's node tasks copy or
// render onto the servers, such as file.source: one_time_playbooks/.payload.
// mmx explain lists only the path, so people who approve and the review
// agent read the content from the commit:
// docs/changing-mmhotline/design/triage-agent.md#the-files-a-playbook-copies.

// ProjectFile is one path a node task reads from the controller machine.
type ProjectFile struct {
	TaskPath string // such as fix/task_1
	Field    string // such as file.source
	// Written is the path as mmx explain lists it, resolved for the task's
	// node when MMX knew it before the run.
	Written string
	// Path is the file or directory in the systems repository, relative to
	// its root, such as roles/web/files/site.conf; empty when Written is
	// absolute, leaves the project, or is not known before the run.
	Path string
	// Absolute: Written names a file anywhere on MMHotline's server.
	Absolute bool
	// Directory: the task copies a whole directory (directory.source) or
	// reads every key file in one.
	Directory bool
	// KnownBeforeRun is false when MMX knows the path only during the run.
	KnownBeforeRun bool
}

// projectFileArguments are the module arguments that name files on the
// controller machine, as MMX's modules list them (ControllerFiles): a file
// to copy or a template to render, and the directories whose files a task
// copies or reads.
var (
	projectFileArguments      = []string{"source", "template", "signing_key_source", "package_file_source", "key_files_on_controller_machine"}
	projectDirectoryArguments = []string{"key_directories_on_controller_machine"}
	trailingIndex             = regexp.MustCompile(`\[[0-9]+\]$`)
)

type explainedTaskWithSource struct {
	explainedTask
	Source struct {
		File string `json:"file"`
	} `json:"source"`
}

// ProjectFiles lists the paths the playbook's node tasks read from the
// controller machine, in the order mmx explain lists the tasks, each path
// once for each task.
func ProjectFiles(output []byte) ([]ProjectFile, error) {
	var explained struct {
		FleetStages []struct {
			NodeTasks []explainedTaskWithSource `json:"node_tasks"`
		} `json:"fleet_stages"`
	}
	if err := json.Unmarshal(output, &explained); err != nil {
		return nil, fmt.Errorf("mmx explain printed JSON MMHotline cannot read: %w", err)
	}
	var files []ProjectFile
	seen := map[[3]string]bool{}
	for _, stage := range explained.FleetStages {
		for _, task := range stage.NodeTasks {
			// MMX reads a relative path from the role's directory inside a
			// role, roles/NAME/main.yml, and from the project root otherwise.
			base := ""
			if strings.HasPrefix(task.Source.File, "roles/") {
				base = path.Dir(task.Source.File)
			}
			for _, argument := range task.Arguments {
				rest, isModuleArgument := strings.CutPrefix(argument.Field, task.Action+".")
				if !isModuleArgument {
					continue
				}
				rest = trailingIndex.ReplaceAllString(rest, "")
				name := rest[strings.LastIndex(rest, ".")+1:]
				directory := slices.Contains(projectDirectoryArguments, name) || task.Action == "directory" && rest == "source"
				if !directory && !slices.Contains(projectFileArguments, name) {
					continue
				}
				known := argument.Status == "literal" || argument.Status == "resolved"
				var written []string
				switch value := argument.Value.(type) {
				case string:
					written = []string{value}
				case []any:
					for _, item := range value {
						if text, isText := item.(string); isText {
							written = append(written, text)
						}
					}
				}
				if !known {
					written = []string{"(known only during the run)"}
				}
				for _, text := range written {
					file := ProjectFile{TaskPath: task.Path, Field: task.Action + "." + rest, Written: text, Directory: directory, KnownBeforeRun: known}
					switch {
					case !known:
					case path.IsAbs(text):
						file.Absolute = true
					default:
						if joined := path.Join(base, text); joined != ".." && !strings.HasPrefix(joined, "../") {
							file.Path = joined
						}
					}
					key := [3]string{file.TaskPath, file.Field, file.Written}
					if !seen[key] {
						seen[key] = true
						files = append(files, file)
					}
				}
			}
		}
	}
	return files, nil
}
