package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kel077/pith/internal/api"
)

const bundleIdentifier = "io.github.kel077.pith.acceptance"

const bundlePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.kel077.pith.acceptance</string>
<key>CFBundleName</key><string>Pith Acceptance</string>
<key>CFBundleExecutable</key><string>PithAcceptance</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
`

const nestedPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.kel077.pith.acceptance.helper</string>
<key>CFBundleName</key><string>Nested Acceptance Helper</string>
<key>CFBundleExecutable</key><string>Helper</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
`

type bundlePlan struct {
	Tool   string `json:"sourceExecutable"`
	SHA256 string `json:"sourceSHA256"`
	Size   int64  `json:"sourceBytes"`
}

type bundleFixture struct {
	Directory string     `json:"directory"`
	App       api.AppRef `json:"app"`
	Main      string     `json:"mainExecutable"`
	Helper    string     `json:"nestedExecutable"`
	Control   string     `json:"controlExecutable"`
}

func fixturePaths(dir string) bundleFixture {
	app := filepath.Join(dir, "PithAcceptance.app")
	return bundleFixture{Directory: dir, App: api.AppRef{Path: app, BundleID: bundleIdentifier, Name: "Pith Acceptance"},
		Main:    filepath.Join(app, "Contents", "MacOS", "PithAcceptance"),
		Helper:  filepath.Join(app, "Contents", "Helpers", "Nested.app", "Contents", "MacOS", "Helper"),
		Control: filepath.Join(dir, "outside-control")}
}

func (s *session) bundlePlanBytes() ([]byte, error) {
	if s.TestProfile != nil || s.Bundle == nil || !filepath.IsAbs(s.Bundle.Tool) || len(s.Bundle.SHA256) != 64 || s.Bundle.Size <= 0 || s.Bundle.Size > 256<<20 {
		return nil, errors.New("prepare a host-ipv4 session with --app-bundle and a bounded standalone rootcheck executable")
	}
	plan := struct {
		Source        *bundlePlan   `json:"source"`
		Target        string        `json:"target"`
		ServerName    string        `json:"serverName"`
		BundleID      string        `json:"bundleId"`
		MainPlist     string        `json:"mainPlist"`
		NestedPlist   string        `json:"nestedPlist"`
		Layout        bundleFixture `json:"relativeLayout"`
		SmokeRequests int           `json:"smokeRequests"`
		BatchRequests int           `json:"batchRequests"`
		Notes         []string      `json:"notes"`
	}{s.Bundle, s.Target, s.ServerName, bundleIdentifier, bundlePlist, nestedPlist, fixturePaths("."), 1, 6, []string{
		"Three checksum-identical pure-Go executables are copied to a new private bundle-fixture-* directory under this session, never registered with Launch Services.",
		"Every child makes one ordinary unbound TLS GET to the existing frozen endpoint, with the existing eight-second request and 4 KiB response limits.",
		"Children live at most 25 seconds, wait for acknowledgment through stdin, and are reaped before their private fixture directory is removed on every ordinary return.",
		"The nested helper has a deliberately different bundle identifier; attribution must use the outermost app. The outside control must remain unmatched.",
		"Only the parent rule is configured. Batch covers nested IP, outside IP control, main/nested domain, nested block priority, and outside block control.",
		"This fixture does not establish XPC activation, WebKit delegation or background URLSession behavior.",
	}}
	data, err := json.MarshalIndent(plan, "", "  ")
	return append(data, '\n'), err
}

func (s *session) writeBundlePlan() error {
	path, err := executablePath()
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	checksum, err := fileHash(path)
	if err != nil {
		return err
	}
	s.Bundle = &bundlePlan{Tool: path, SHA256: checksum, Size: info.Size()}
	data, err := s.bundlePlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "bundle-plan.json"), data); err != nil {
		return err
	}
	s.BundlePlanSHA256 = hashBytes(data)
	return s.save()
}

func (s *session) reviewedBundlePlan() error {
	data, err := s.bundlePlanBytes()
	if err != nil {
		return err
	}
	if hashBytes(data) != s.BundlePlanSHA256 {
		return errors.New("bundle plan changed after preparation")
	}
	path := filepath.Join(s.Dir, "bundle-plan.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("bundle plan is not a private regular file")
	}
	actual, err := os.ReadFile(path)
	if err != nil || hashBytes(actual) != s.BundlePlanSHA256 {
		return errors.New("bundle review file changed after preparation")
	}
	return nil
}

func createBundleFixture(s *session) (fixture bundleFixture, cleanup func() error, err error) {
	if err := s.reviewedBundlePlan(); err != nil {
		return fixture, nil, err
	}
	if err := privateTempDirectory(s.Dir); err != nil {
		return fixture, nil, err
	}
	self, err := executablePath()
	if err != nil || self != s.Bundle.Tool {
		return fixture, nil, errors.New("run bundle acceptance with the reviewed standalone executable")
	}
	checksum, err := fileHash(self)
	if err != nil || checksum != s.Bundle.SHA256 {
		return fixture, nil, errors.New("bundle source changed after preparation")
	}
	dir, err := os.MkdirTemp(s.Dir, "bundle-fixture-")
	if err != nil {
		return fixture, nil, err
	}
	cleanup = func() error { return os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()
	fixture = fixturePaths(dir)
	for _, path := range []string{fixture.Main, fixture.Helper, fixture.Control} {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return
		}
		if err = copyBundleExecutable(s.Bundle, path); err != nil {
			return
		}
	}
	for path, data := range map[string]string{
		filepath.Join(fixture.App.Path, "Contents", "Info.plist"):               bundlePlist,
		filepath.Join(filepath.Dir(filepath.Dir(fixture.Helper)), "Info.plist"): nestedPlist,
	} {
		if err = writePrivate(path, []byte(data)); err != nil {
			return
		}
	}
	_, err = verifyBundleFixture(s, dir)
	return
}

func copyBundleExecutable(plan *bundlePlan, path string) error {
	in, err := os.Open(plan.Tool)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() != plan.Size {
		return errors.New("bundle source is not the reviewed executable")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, plan.Size+1))
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return err
	}
	checksum, err := fileHash(path)
	if err != nil || n != plan.Size || checksum != plan.SHA256 {
		return errors.New("bundle executable copy differs from the reviewed checksum")
	}
	return nil
}

func verifyBundleFixture(s *session, dir string) (bundleFixture, error) {
	f := fixturePaths(dir)
	if s.Bundle == nil {
		return f, errors.New("bundle fixture was not prepared")
	}
	if err := privateTempDirectory(dir); err != nil {
		return f, err
	}
	if filepath.Dir(dir) != s.Dir || !strings.HasPrefix(filepath.Base(dir), "bundle-fixture-") {
		return f, errors.New("bundle fixture is not a direct private child of this session")
	}
	for _, path := range []string{f.Main, f.Helper, f.Control} {
		actual, err := filepath.EvalSymlinks(path)
		if err != nil || actual != path {
			return f, errors.New("bundle fixture contains a symlink or missing executable")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || info.Size() != s.Bundle.Size {
			return f, errors.New("bundle fixture executable metadata changed")
		}
		digest, err := fileHash(path)
		if err != nil || digest != s.Bundle.SHA256 {
			return f, errors.New("bundle fixture executable checksum changed")
		}
	}
	for path, expected := range map[string]string{
		filepath.Join(f.App.Path, "Contents", "Info.plist"):               bundlePlist,
		filepath.Join(filepath.Dir(filepath.Dir(f.Helper)), "Info.plist"): nestedPlist,
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
			return f, errors.New("bundle metadata changed")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != expected {
			return f, errors.New("bundle identifier or metadata changed")
		}
	}
	return f, nil
}
