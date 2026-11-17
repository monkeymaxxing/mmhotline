package systemsrepository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// TreeEntry is one file of a commit's tree, as git ls-tree -r -l lists it.
type TreeEntry struct {
	Path   string
	Mode   string // such as 100644, 100755, or 120000 for a symlink
	Object string
	// Size is the blob's size in bytes; -1 for a submodule, which has none.
	Size int64
}

// FilesAtCommit lists the files at path in commitID, every file below path
// when it is a directory, in Git's order. The path is literal: Git reads no
// pattern in it.
func FilesAtCommit(ctx context.Context, directory, commitID, path string) ([]TreeEntry, error) {
	output, err := runGit(ctx, directory, []string{"GIT_LITERAL_PATHSPECS=1"}, "ls-tree", "-r", "-l", "-z", commitID, "--", path)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, record := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> <type> <object> <size>\t<path>"
		fields, name, found := strings.Cut(record, "\t")
		parts := strings.Fields(fields)
		if !found || len(parts) != 4 {
			return nil, fmt.Errorf("unexpected git ls-tree record %q", record)
		}
		size := int64(-1)
		if parts[3] != "-" {
			if size, err = strconv.ParseInt(parts[3], 10, 64); err != nil {
				return nil, fmt.Errorf("unexpected git ls-tree size %q", parts[3])
			}
		}
		entries = append(entries, TreeEntry{Path: name, Mode: parts[0], Object: parts[2], Size: size})
	}
	return entries, nil
}

// BlobContent reads one blob that FilesAtCommit listed.
func BlobContent(ctx context.Context, directory, object string) (string, error) {
	return runGit(ctx, directory, nil, "cat-file", "blob", object)
}
