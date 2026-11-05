package api

import (
	"encoding/json"
	"fmt"
	"time"
)

// File kinds: what a File holds.
const (
	FileRules  = "rules"  // rules to add to a scene
	FileBackup = "backup" // a whole configuration, to replace the user's
)

// File is a file Pith exports and imports: the rules of a scene, or a backup
// of the whole configuration. It is JSON.
type File struct {
	Kind     string    `json:"pith"`    // FileRules or FileBackup
	Version  int       `json:"version"` // the ConfigVersion of its content
	App      string    `json:"app"`     // the version of Pith that wrote it
	Exported time.Time `json:"exported"`
	Rules    []Rule    `json:"rules,omitempty"`  // of FileRules
	Config   *Config   `json:"config,omitempty"` // of FileBackup
}

// ParseFile reads a File and checks its kind and version. It does not
// validate the rules or the configuration: the caller does, after giving
// imported rules new IDs.
func ParseFile(data []byte) (File, error) {
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, &Error{Code: ErrInvalidConfig, Detail: "not a Pith file: " + err.Error()}
	}
	switch {
	case f.Kind != FileRules && f.Kind != FileBackup:
		return File{}, &Error{Code: ErrInvalidConfig, Detail: fmt.Sprintf("not a Pith file (kind %q)", f.Kind)}
	case f.Version < 1 || f.Version > ConfigVersion:
		return File{}, &Error{Code: ErrInvalidConfig, Detail: fmt.Sprintf("format version %d; this Pith reads 1 to %d", f.Version, ConfigVersion)}
	case f.Kind == FileBackup && f.Config == nil:
		return File{}, &Error{Code: ErrInvalidConfig, Detail: "backup without a configuration"}
	}
	return f, nil
}
