// Command release validates release metadata and stages verified build assets.
// It never builds, mounts disk images, uses credentials or contacts GitHub.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?(\+([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$`)

func versionFromTag(tag string) (string, error) {
	if len(tag) > 128 || !versionPattern.MatchString(tag) {
		return "", errors.New("release tag must be v followed by a full semantic version")
	}
	version := tag[1:]
	base := strings.SplitN(version, "+", 2)[0]
	if _, pre, ok := strings.Cut(base, "-"); ok {
		for _, id := range strings.Split(pre, ".") {
			if len(id) > 1 && id[0] == '0' && strings.Trim(id, "0123456789") == "" {
				return "", errors.New("numeric prerelease identifiers must not have leading zeroes")
			}
		}
	}
	return version, nil
}

func releaseNotes(changelog, version string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(changelog, "\r\n", "\n"), "\n")
	marker := "## [" + version + "]"
	start, end, count := -1, len(lines), 0
	var fence byte
	fenceLen := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if fence == 0 && n >= 3 {
				fence, fenceLen = trimmed[0], n
			} else if trimmed[0] == fence && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
			continue
		}
		if fence != 0 || !strings.HasPrefix(line, "## ") {
			continue
		}
		if start >= 0 && end == len(lines) {
			end = i
		}
		if line != marker && !strings.HasPrefix(line, marker+" ") {
			continue
		}
		count++
		date, ok := strings.CutPrefix(line, marker+" - ")
		if _, err := time.Parse("2006-01-02", date); !ok || err != nil {
			return "", errors.New("matching changelog section needs a YYYY-MM-DD release date")
		}
		if start < 0 {
			start = i + 1
		}
	}
	if count != 1 {
		return "", fmt.Errorf("expected one changelog section for %s, found %d", version, count)
	}
	notes := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
	if notes == "" {
		return "", errors.New("matching changelog section is empty")
	}
	return notes + "\n", nil
}

func metadata(tag, configPath, changelogPath string) (string, string, error) {
	version, err := versionFromTag(tag)
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", err
	}
	var config struct{ Version, Slug string }
	if err := json.Unmarshal(data, &config); err != nil {
		return "", "", err
	}
	if config.Version != version {
		return "", "", errors.New("release tag does not match mygo.json version")
	}
	if config.Slug != "pith" {
		return "", "", errors.New("release asset naming requires the pith slug")
	}
	data, err = os.ReadFile(changelogPath)
	if err != nil {
		return "", "", err
	}
	notes, err := releaseNotes(string(data), version)
	return version, notes, err
}

func assetBase(version string) string { return "pith-" + version + "-arm64" }

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("expected a nonempty regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		f.Close()
		return nil, fmt.Errorf("file identity changed while opening: %s", path)
	}
	return f, nil
}

func checksum(path string) (string, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func stage(tag, buildDir, out string, dmg bool) error {
	version, err := versionFromTag(tag)
	if err != nil {
		return err
	}
	base := assetBase(version)
	sources := []string{filepath.Join(buildDir, base+".zip")}
	if dmg {
		sources = append(sources, filepath.Join(buildDir, "darwin-arm64", base+".dmg"))
	}
	// Refuse stale output rather than accidentally publish an old build.
	if err := os.Mkdir(out, 0755); err != nil {
		return err
	}
	var sums strings.Builder
	for _, source := range sources {
		name := filepath.Base(source)
		f, err := openRegular(source)
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(filepath.Join(out, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			f.Close()
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(dst, h), f)
		if err := errors.Join(copyErr, f.Close(), dst.Close()); err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", h.Sum(nil), name)
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(sums.String()), 0644)
}

func verifyAssets(dir, version string) error {
	base := assetBase(version)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	assets := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type() != 0 || (name != "SHA256SUMS" && name != base+".zip" && name != base+".dmg") {
			return fmt.Errorf("unexpected release asset: %s", name)
		}
		assets[name] = true
	}
	if !assets["SHA256SUMS"] || !assets[base+".zip"] {
		return errors.New("release requires a ZIP and SHA256SUMS")
	}
	data, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		return errors.New("SHA256SUMS must end with a newline")
	}
	delete(assets, "SHA256SUMS")
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		decoded, err := hex.DecodeString(digest)
		if !ok || err != nil || len(decoded) != sha256.Size || !assets[name] {
			return errors.New("invalid, duplicate or unexpected checksum entry")
		}
		actual, err := checksum(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if digest != actual {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
		delete(assets, name)
	}
	if len(assets) != 0 {
		return errors.New("SHA256SUMS does not list every release archive")
	}
	return nil
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected metadata, stage or verify")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	tag := flags.String("tag", "", "existing v-prefixed release tag")
	config := flags.String("config", "mygo.json", "app configuration")
	changelog := flags.String("changelog", "CHANGELOG.md", "dated release notes")
	buildDir := flags.String("build-dir", "build", "build directory")
	out := flags.String("out", "", "new assets directory (stage) or notes file (metadata)")
	dir := flags.String("dir", "", "downloaded release directory (verify)")
	dmg := flags.Bool("dmg", false, "include the successfully built DMG")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch args[0] {
	case "metadata":
		_, notes, err := metadata(*tag, *config, *changelog)
		if err != nil {
			return err
		}
		return os.WriteFile(*out, []byte(notes), 0644)
	case "stage":
		return stage(*tag, *buildDir, *out, *dmg)
	case "verify":
		version, notes, err := metadata(*tag, *config, *changelog)
		if err != nil {
			return err
		}
		actual, err := os.ReadFile(filepath.Join(*dir, "release-notes.md"))
		if err != nil {
			return err
		}
		if string(actual) != notes {
			return errors.New("downloaded release notes differ from the tagged changelog")
		}
		return verifyAssets(filepath.Join(*dir, "assets"), version)
	default:
		return errors.New("expected metadata, stage or verify")
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pith release:", err)
		os.Exit(1)
	}
}
