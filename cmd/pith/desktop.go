package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/Kel077/pith/internal/api"
)

// desktop is the operating system, as MyGo gives it.
type desktop struct{}

func (desktop) InstalledApps() ([]api.AppRef, error) {
	apps, err := mygo.Shell.InstalledApps()
	if err != nil {
		return nil, err
	}
	out := make([]api.AppRef, 0, len(apps))
	for _, a := range apps {
		ref := api.AppRef{Path: a.Path, Name: a.Name}
		if a.ID != a.Path {
			ref.BundleID = a.ID
		}
		out = append(out, ref)
	}
	return out, nil
}

func (desktop) Icon(path string, size int) ([]byte, error) { return mygo.Shell.FileIcon(path, size) }

func (desktop) ChooseProgram() (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title:       "Choose a Program",
		DefaultPath: "/Applications",
		ButtonLabel: "Choose",
	})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}

func (desktop) Save(name string, data []byte) error {
	// A name is a file name, never a path; the dialog starts in Downloads.
	name = strings.NewReplacer("/", "-", ":", "-").Replace(name)
	if home, err := os.UserHomeDir(); err == nil {
		name = filepath.Join(home, "Downloads", name)
	}
	path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
		DefaultPath: name,
		Filters:     []mygo.FileFilter{{Name: "Pith", Extensions: []string{"json"}}},
	})
	if err != nil || path == "" {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (desktop) Open() ([]byte, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Filters: []mygo.FileFilter{{Name: "Pith", Extensions: []string{"json"}}},
	})
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	return os.ReadFile(paths[0])
}

func (desktop) OpenURL(url string)           { mygo.Shell.OpenExternal(url) }
func (desktop) Reveal(path string)           { mygo.Shell.ShowItemInFolder(path) }
func (desktop) SetOpenAtLogin(on bool) error { return mygo.App.SetOpenAtLogin(on) }
func (desktop) PreferredLanguages() []string { return mygo.App.PreferredLanguages() }
func (desktop) Quit()                        { mygo.App.Quit() }

// statePath is the file of the interface's state between launches.
func statePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Pith", "ui-state.json")
}

func (desktop) ReadState(name string) string {
	var state map[string]string
	if data, err := os.ReadFile(statePath()); err == nil {
		json.Unmarshal(data, &state)
	}
	return state[name]
}

func (desktop) WriteState(name, value string) {
	path := statePath()
	if path == "" {
		return
	}
	state := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &state)
	}
	if state[name] == value {
		return
	}
	state[name] = value
	data, _ := json.MarshalIndent(state, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}
