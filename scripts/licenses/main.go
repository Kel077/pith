// Command licenses packages the reviewed notices for the two shipped binaries.
// It reads only their build dependency graph, not every module in the cache.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

//go:embed approvals.json
var approvalsJSON []byte

type approval struct {
	Version string            `json:"version"`
	License string            `json:"license"`
	Files   map[string]string `json:"files"`
}

type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

type goPackage struct {
	Dir, ImportPath             string
	Standard                    bool
	Module                      *module
	GoFiles, SFiles, EmbedFiles []string
}

type component struct {
	Module  string            `json:"module"`
	Version string            `json:"version,omitempty"`
	License string            `json:"license"`
	Files   map[string]string `json:"files"`
}

func main() {
	verify := flag.String("verify", "", "verify an existing licenses directory instead of writing resources")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "licenses: unexpected arguments")
		os.Exit(1)
	}
	if err := run(*verify); err != nil {
		fmt.Fprintln(os.Stderr, "licenses:", err)
		os.Exit(1)
	}
}

func run(verify string) error {
	var approvals map[string]approval
	if err := json.Unmarshal(approvalsJSON, &approvals); err != nil {
		return err
	}
	packages, err := buildPackages()
	if err != nil {
		return err
	}
	modules := make(map[string]*module)
	dirs := make(map[string]map[string]bool)
	notices := make(map[string]map[string]bool)
	for _, p := range packages {
		if p.Standard {
			continue
		}
		m := p.Module
		if m == nil || m.Replace != nil {
			return fmt.Errorf("unreviewed module or replacement for %s", p.ImportPath)
		}
		modules[m.Path] = m
		if dirs[m.Path] == nil {
			dirs[m.Path] = make(map[string]bool)
			notices[m.Path] = make(map[string]bool)
		}
		if err := addAncestors(dirs[m.Path], p.Dir, m.Dir); err != nil {
			return err
		}
		for _, file := range p.EmbedFiles {
			if err := addAncestors(dirs[m.Path], filepath.Dir(filepath.Join(p.Dir, file)), m.Dir); err != nil {
				return err
			}
		}
		if !m.Main {
			for _, name := range append(slices.Clone(p.GoFiles), p.SFiles...) {
				data, err := os.ReadFile(filepath.Join(p.Dir, name))
				if err != nil {
					return err
				}
				for _, block := range leadingNotices(data) {
					notices[m.Path][block] = true
				}
			}
		}
	}
	goEnv, err := goOutput("env", "-json", "GOROOT", "GOVERSION")
	if err != nil {
		return err
	}
	var env map[string]string
	if err := json.Unmarshal(goEnv, &env); err != nil {
		return err
	}
	modules["go"] = &module{Path: "go", Version: env["GOVERSION"], Dir: env["GOROOT"]}
	dirs["go"] = map[string]bool{env["GOROOT"]: true}

	outputs := make(map[string][]byte)
	var manifest []component
	var notice bytes.Buffer
	fmt.Fprintln(&notice, "Pith and third-party notices")
	fmt.Fprintln(&notice, "Target: darwin/arm64, CGO_ENABLED=0")
	fmt.Fprintln(&notice, "The accompanying license files are copied without changes.")
	fmt.Fprintln(&notice, "For dual-licensed typesetting code, Pith selects BSD-3-Clause.")
	for _, path := range sortedKeys(modules) {
		m := modules[path]
		a, ok := approvals[path]
		if !ok || a.Version != m.Version {
			return fmt.Errorf("unreviewed dependency %s %s; review scripts/licenses/approvals.json", path, m.Version)
		}
		files, err := reviewedFiles(m, dirs[path], a)
		if err != nil {
			return err
		}
		entry := component{Module: path, Version: m.Version, License: a.License, Files: make(map[string]string)}
		fmt.Fprintf(&notice, "\n%s %s\nLicense: %s\n", path, m.Version, a.License)
		for _, name := range sortedKeys(files) {
			out := filepath.ToSlash(filepath.Join("modules", path, name))
			outputs[out] = files[name]
			entry.Files[out] = digest(files[name])
			fmt.Fprintf(&notice, "  %s\n", out)
		}
		for _, block := range sortedKeys(notices[path]) {
			fmt.Fprintf(&notice, "\n%s\n", block)
		}
		manifest = append(manifest, entry)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	outputs["manifest.json"] = append(data, '\n')
	outputs["NOTICE.txt"] = notice.Bytes()
	if verify != "" {
		if err := verifyOutputs(verify, outputs); err != nil {
			return err
		}
		fmt.Printf("Verified licenses for %d components\n", len(manifest))
		return nil
	}
	if err := writeOutputs("resources/darwin-arm64/licenses", outputs); err != nil {
		return err
	}
	fmt.Printf("Packaged licenses for %d components\n", len(manifest))
	return nil
}

func buildPackages() ([]goPackage, error) {
	var packages []goPackage
	for _, target := range []string{"./cmd/pith", "./cmd/pithd"} {
		args := []string{"list", "-mod=readonly", "-deps", "-json"}
		if target == "./cmd/pith" && os.Getenv("MYGO_INSPECTOR") != "1" {
			// Match MyGo v0.7.4's productionTags, including caller-supplied tags.
			var tags []string
			for _, field := range strings.Fields(os.Getenv("GOFLAGS")) {
				if value, ok := strings.CutPrefix(strings.TrimLeft(field, "-"), "tags="); ok && value != "" {
					tags = append(tags, value)
				}
			}
			args = append(args, "-tags", strings.Join(append(tags, "mygo_noinspector"), ","))
		}
		data, err := goOutput(append(args, target)...)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		for {
			var p goPackage
			if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, err
			}
			packages = append(packages, p)
		}
	}
	return packages, nil
}

func goOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return data, nil
}

func addAncestors(dirs map[string]bool, dir, root string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("package or embedded resource escapes its module")
	}
	for {
		dirs[dir] = true
		if dir == root {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

func reviewedFiles(m *module, dirs map[string]bool, a approval) (map[string][]byte, error) {
	files := make(map[string][]byte)
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !noticeName(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			rel, err := filepath.Rel(m.Dir, path)
			if err != nil {
				return nil, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			name := filepath.ToSlash(rel)
			if len(data) == 0 || a.Files[name] != digest(data) {
				return nil, fmt.Errorf("unreviewed or changed notice %s/%s", m.Path, name)
			}
			files[name] = data
		}
	}
	for name := range a.Files {
		if files[name] == nil {
			return nil, fmt.Errorf("missing reviewed notice %s/%s", m.Path, name)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no reviewed license for %s", m.Path)
	}
	return files, nil
}

func noticeName(name string) bool {
	name = strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "NOTICE", "COPYING", "COPYRIGHT", "PATENTS"} {
		if name == prefix || strings.HasPrefix(name, prefix+".") || strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}
	return false
}

// Preserve relevant leading source comments, including copyrights omitted from
// a module's generic LICENSE. The comment bytes themselves are not rewritten.
func leadingNotices(data []byte) []string {
	var notices []string
	for {
		data = bytes.TrimLeft(data, " \t\r\n")
		var block []byte
		switch {
		case bytes.HasPrefix(data, []byte("/*")):
			end := bytes.Index(data[2:], []byte("*/"))
			if end < 0 {
				return notices
			}
			block, data = data[:end+4], data[end+4:]
		case bytes.HasPrefix(data, []byte("//")):
			end := 0
			for bytes.HasPrefix(data[end:], []byte("//")) {
				n := bytes.IndexByte(data[end:], '\n')
				if n < 0 {
					end = len(data)
					break
				}
				end += n + 1
			}
			block, data = data[:end], data[end:]
		default:
			return notices
		}
		lower := strings.ToLower(string(block))
		for _, marker := range []string{"copyright", "spdx-license-identifier", "licensed under", "permission is hereby", "redistribution and use"} {
			if strings.Contains(lower, marker) {
				notices = append(notices, string(block))
				break
			}
		}
	}
}

func writeOutputs(out string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".licenses-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for name, data := range files {
		path := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	return os.Rename(stage, out)
}

func verifyOutputs(dir string, files map[string][]byte) error {
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("missing or changed packaged notice %s", name)
		}
	}
	return nil
}

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
