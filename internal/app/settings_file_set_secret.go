package app

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

// This file is the engine of sudo mmhotline settings set-secret
// (docs/reference/mmhotline-command.md): it changes one secret key of the
// settings file without the person opening the file. The secret keys come
// from the settings:"secret" tags of Settings, the one source that the
// settings reference's test also compares with its "secret" rows.

// alertSourceKeyPathPlaceholder stands for an alert source's ID in the key
// path of the signing secrets, as in alert_sources[ID].signing_secrets.
const alertSourceKeyPathPlaceholder = "ID"

// SettingsSecretKeyPaths lists the key path of every secret key, in the form
// a person types: alert_sources[ID].signing_secrets, where ID stands for the
// ID of one alert source.
func SettingsSecretKeyPaths() []string {
	var paths []string
	collectSecretKeyPaths(reflect.TypeFor[Settings](), "", &paths)
	return paths
}

func collectSecretKeyPaths(sectionType reflect.Type, prefix string, paths *[]string) {
	for index := range sectionType.NumField() {
		field := sectionType.Field(index)
		name, options, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if options == "inline" && fieldType.Kind() == reflect.Struct {
			collectSecretKeyPaths(fieldType, prefix, paths)
			continue
		}
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		switch {
		case strings.Contains(","+field.Tag.Get("settings")+",", ",secret,"):
			*paths = append(*paths, path)
		case fieldType.Kind() == reflect.Struct:
			collectSecretKeyPaths(fieldType, path+".", paths)
		case fieldType.Kind() == reflect.Slice && fieldType.Elem().Kind() == reflect.Struct:
			collectSecretKeyPaths(fieldType.Elem(), path+"["+alertSourceKeyPathPlaceholder+"].", paths)
		}
	}
}

var listItemKeyPathPattern = regexp.MustCompile(`\[[^\]]*\]`)

// SettingsSecretKeyPathRefusal says why keyPath is not a secret key, with
// the list of the secret keys; empty when it is one.
func SettingsSecretKeyPathRefusal(keyPath string) string {
	paths := SettingsSecretKeyPaths()
	if slices.Contains(paths, listItemKeyPathPattern.ReplaceAllString(keyPath, "["+alertSourceKeyPathPlaceholder+"]")) && !strings.Contains(keyPath, "[]") {
		return ""
	}
	return fmt.Sprintf("%s is not a secret key, and set-secret changes only secret keys. The secret keys are:\n  %s\nIn alert_sources[ID].signing_secrets, ID is the alert source's ID, such as monitoring.", keyPath, strings.Join(paths, "\n  "))
}

// SettingsSecretEdit is a settings file's text after set-secret, and what
// the edit did to a list of signing secrets.
type SettingsSecretEdit struct {
	Text []byte
	// KeptEarlierSigningSecret is true when the source's list still holds
	// its earlier signing secret next to the new one.
	KeptEarlierSigningSecret bool
}

// EditSettingsSecret sets the secret key keyPath in the settings file text
// raw to value, keeping comments and key order, and creates missing
// sections. It checks the result and returns the problems at that key; the
// caller writes nothing while there are problems. An error is a refusal
// that never contains value.
//
// For alert_sources[ID].signing_secrets the source keeps its latest earlier
// signing secret when that secret is valid, so the sender keeps working
// until it gets the new one; an earlier secret that is not valid, such as the
// example's placeholder, is dropped. The list holds at most two secrets, the
// most that the settings check accepts, so a third drops the oldest.
func EditSettingsSecret(raw []byte, keyPath, value string) (SettingsSecretEdit, []SettingsProblem, error) {
	var edit SettingsSecretEdit
	if refusal := SettingsSecretKeyPathRefusal(keyPath); refusal != "" {
		return edit, nil, errors.New(refusal)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return edit, nil, fmt.Errorf("the settings file is not valid YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return edit, nil, errors.New("the settings file must be a section of keys, each on its own line as key: value")
	}
	section := document.Content[0]
	checkPrefix := keyPath
	segments := strings.Split(keyPath, ".")
	for position, segment := range segments {
		last := position == len(segments)-1
		name, sourceID, isListItem := strings.Cut(strings.TrimSuffix(segment, "]"), "[")
		if isListItem {
			var err error
			var index int
			if section, index, err = alertSourceMapping(section, name, sourceID); err != nil {
				return edit, nil, err
			}
			checkPrefix = strings.Replace(keyPath, name+"["+sourceID+"]", fmt.Sprintf("%s[%d]", name, index), 1)
			continue
		}
		if last {
			if name == "signing_secrets" {
				edit.KeptEarlierSigningSecret = setSigningSecrets(section, value)
			} else {
				setScalar(section, name, value)
			}
			break
		}
		var err error
		if section, err = sectionOf(section, name, strings.Join(segments[:position+1], ".")); err != nil {
			return edit, nil, err
		}
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return edit, nil, fmt.Errorf("MMHotline cannot write the settings file: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return edit, nil, fmt.Errorf("MMHotline cannot write the settings file: %w", err)
	}
	edit.Text = buffer.Bytes()
	_, all, _ := parseSettings(edit.Text)
	var problems []SettingsProblem
	for _, problem := range all {
		if problem.KeyPath == "" {
			return edit, nil, errors.New("the settings file has a problem that set-secret cannot fix: " + problem.Problem)
		}
		if problem.KeyPath == checkPrefix || strings.HasPrefix(problem.KeyPath, checkPrefix+"[") {
			problem.KeyPath = keyPath + strings.TrimPrefix(problem.KeyPath, checkPrefix)
			problems = append(problems, problem)
		}
	}
	return edit, problems, nil
}

// alertSourceMapping finds the alert source with the ID sourceID in the
// list named name, and its position in the list.
func alertSourceMapping(parent *yaml.Node, name, sourceID string) (*yaml.Node, int, error) {
	list := mappingValue(parent, name)
	var knownIDs []string
	if list != nil && list.Kind == yaml.SequenceNode {
		for index, item := range list.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			if id := mappingValue(item, "alert_source_id"); id != nil {
				if id.Value == sourceID {
					return item, index, nil
				}
				knownIDs = append(knownIDs, id.Value)
			}
		}
	}
	known := "The settings file has no alert sources."
	if len(knownIDs) > 0 {
		known = "Its alert sources are: " + strings.Join(knownIDs, ", ") + "."
	}
	return nil, 0, fmt.Errorf("the settings file has no alert source with the ID %s. %s", sourceID, known)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

// sectionOf returns the section named name inside parent, and creates it
// when it is missing or has no value.
func sectionOf(parent *yaml.Node, name, path string) (*yaml.Node, error) {
	section := mappingValue(parent, name)
	if section == nil {
		section = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, section)
		return section, nil
	}
	switch {
	case section.Kind == yaml.ScalarNode && section.Tag == "!!null":
		*section = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case section.Kind != yaml.MappingNode:
		return nil, fmt.Errorf("%s in the settings file is not a section of keys, so set-secret cannot add a key to it", path)
	}
	// A section written as {} is a block section once it has a key.
	section.Style = 0
	return section, nil
}

func quotedValue(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
}

func setScalar(section *yaml.Node, key, value string) {
	if existing := mappingValue(section, key); existing != nil {
		*existing = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle, HeadComment: existing.HeadComment, LineComment: existing.LineComment, FootComment: existing.FootComment}
		return
	}
	section.Content = append(section.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, quotedValue(value))
}

func setSigningSecrets(source *yaml.Node, value string) (keptEarlier bool) {
	list := mappingValue(source, "signing_secrets")
	var kept []*yaml.Node
	if list != nil && list.Kind == yaml.SequenceNode {
		for _, item := range list.Content {
			if decoded, err := hex.DecodeString(item.Value); err == nil && len(decoded) == 32 && item.Value != value {
				kept = append(kept, item)
			}
		}
	}
	if len(kept) > 1 {
		kept = kept[len(kept)-1:]
	}
	content := append(kept, quotedValue(value))
	if list == nil {
		source.Content = append(source.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "signing_secrets"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle, Content: content})
		return false
	}
	list.Kind, list.Tag, list.Style, list.Value, list.Content = yaml.SequenceNode, "!!seq", yaml.FlowStyle, "", content
	return len(kept) > 0
}

// WriteSettingsFileText replaces the settings file at path with text and
// keeps its owner, group, and mode. It writes a temporary file in the same
// directory, which has the owner and mode before it holds any text, and
// renames it over the file, so a reader sees the old file or the new one.
func WriteSettingsFileText(path string, text []byte) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("MMHotline cannot read the settings file: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mmhotline_settings_")
	if err != nil {
		return fmt.Errorf("MMHotline cannot write next to the settings file: %w", err)
	}
	defer func() {
		if err != nil {
			temporary.Close()
			os.Remove(temporary.Name())
		}
	}()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err = temporary.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return fmt.Errorf("MMHotline cannot give the new settings file the owner of the old one: %w", err)
		}
	}
	if err = temporary.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err = temporary.Write(text); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
