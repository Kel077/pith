package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestVersionFromTag(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "v1.20.3-rc.1", "v2.0.0-rc.2+build.1"} {
		if got, err := versionFromTag(tag); err != nil || got != tag[1:] {
			t.Fatalf("%q: %q, %v", tag, got, err)
		}
	}
	for _, tag := range []string{"", "main", "v1.2", "v01.2.3", "v1.2.3-01", "v1.2.3-", "v1.2.3+", "v1.2.3/../asset", "v1.2.3\nother=value", "v1.2.3;touch bad", "v1.2.3$(id)", "v1.2.3."} {
		if _, err := versionFromTag(tag); err == nil {
			t.Errorf("accepted invalid tag %q", tag)
		}
	}
}

func TestReleaseNotes(t *testing.T) {
	changelog := "# Changelog\n\n## [Unreleased]\nNot released.\n\n" +
		"```md\n## [0.1.0] - 2026-01-01\nNot a release.\n```\n\n" +
		"## [0.1.0] - 2026-11-05\n\n### Added\n\n- Initial release.\n\n" +
		"```md\n## An example inside the notes\n```\n\n" +
		"## [0.0.1] - 2026-10-01\nOlder release.\n"
	want := "### Added\n\n- Initial release.\n\n```md\n## An example inside the notes\n```\n"
	for _, text := range []string{changelog, strings.ReplaceAll(changelog, "\n", "\r\n")} {
		got, err := releaseNotes(text, "0.1.0")
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	for name, text := range map[string]string{
		"missing":   "## [Unreleased]\nPending.\n",
		"undated":   "## [0.1.0]\nPending.\n",
		"bad date":  "## [0.1.0] - 2026-02-30\nPending.\n",
		"empty":     "## [0.1.0] - 2026-11-05\n\n## [0.0.1] - 2026-10-01\nOlder.\n",
		"duplicate": "## [0.1.0] - 2026-11-05\nFirst.\n## [0.1.0] - 2026-11-05\nSecond.\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := releaseNotes(text, "0.1.0"); err == nil {
				t.Fatal("accepted invalid changelog")
			}
		})
	}
}

func releaseFixture(t *testing.T) (config, changelog, build string) {
	t.Helper()
	dir := t.TempDir()
	config, changelog, build = filepath.Join(dir, "mygo.json"), filepath.Join(dir, "CHANGELOG.md"), filepath.Join(dir, "build")
	writeFixture(t, config, `{"version":"0.1.0","slug":"pith"}`)
	writeFixture(t, changelog, "# Changelog\n\n## [0.1.0] - 2026-11-05\n\nRelease notes.\n")
	writeFixture(t, filepath.Join(build, "pith-0.1.0-arm64.zip"), "ZIP fixture")
	writeFixture(t, filepath.Join(build, "darwin-arm64", "pith-0.1.0-arm64.dmg"), "DMG fixture")
	return
}

func TestMetadata(t *testing.T) {
	config, changelog, _ := releaseFixture(t)
	version, notes, err := metadata("v0.1.0", config, changelog)
	if err != nil || version != "0.1.0" || notes != "Release notes.\n" {
		t.Fatalf("%q, %q, %v", version, notes, err)
	}
	if _, _, err := metadata("v0.2.0", config, changelog); err == nil {
		t.Fatal("accepted version mismatch")
	}
	writeFixture(t, config, `{"version":"0.1.0","slug":"other"}`)
	if _, _, err := metadata("v0.1.0", config, changelog); err == nil {
		t.Fatal("accepted unexpected output naming")
	}
}

func TestStageAndVerify(t *testing.T) {
	for _, includeDMG := range []bool{false, true} {
		t.Run(map[bool]string{false: "zip only", true: "zip and dmg"}[includeDMG], func(t *testing.T) {
			config, changelog, build := releaseFixture(t)
			dir := t.TempDir()
			if err := run([]string{"metadata", "-tag", "v0.1.0", "-config", config, "-changelog", changelog, "-out", filepath.Join(dir, "release-notes.md")}); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "assets")
			if err := stage("v0.1.0", build, out, includeDMG); err != nil {
				t.Fatal(err)
			}
			verify := []string{"verify", "-tag", "v0.1.0", "-config", config, "-changelog", changelog, "-dir", dir}
			if err := run(verify); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != map[bool]int{false: 2, true: 3}[includeDMG] {
				t.Fatalf("unexpected asset count: %d", len(entries))
			}
			if err := stage("v0.1.0", build, out, includeDMG); err == nil {
				t.Fatal("accepted stale output directory")
			}
			writeFixture(t, filepath.Join(dir, "release-notes.md"), "Different notes.\n")
			if err := run(verify); err == nil {
				t.Fatal("accepted changed notes after artifact transfer")
			}
		})
	}
}

func TestVerifyRejectsChangedAssets(t *testing.T) {
	for _, change := range []string{"tamper", "extra", "missing", "unlisted", "duplicate", "traversal", "symlink"} {
		t.Run(change, func(t *testing.T) {
			_, _, build := releaseFixture(t)
			out := filepath.Join(t.TempDir(), "assets")
			if err := stage("v0.1.0", build, out, true); err != nil {
				t.Fatal(err)
			}
			zip := filepath.Join(out, "pith-0.1.0-arm64.zip")
			sumsPath := filepath.Join(out, "SHA256SUMS")
			sums, err := os.ReadFile(sumsPath)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "tamper":
				writeFixture(t, zip, "changed ZIP")
			case "extra":
				writeFixture(t, filepath.Join(out, "unexpected.zip"), "another archive")
			case "missing":
				err = os.Remove(zip)
			case "unlisted":
				writeFixture(t, sumsPath, strings.Split(string(sums), "\n")[0]+"\n")
			case "duplicate":
				writeFixture(t, sumsPath, string(sums)+strings.Split(string(sums), "\n")[0]+"\n")
			case "traversal":
				writeFixture(t, sumsPath, strings.ReplaceAll(string(sums), "pith-0.1.0-arm64.zip", "../pith-0.1.0-arm64.zip"))
			case "symlink":
				if err = os.Remove(zip); err == nil {
					err = os.Symlink(filepath.Join(build, "pith-0.1.0-arm64.zip"), zip)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyAssets(out, "0.1.0"); err == nil {
				t.Fatal("accepted changed release assets")
			}
		})
	}
}

func TestStageRejectsInvalidSource(t *testing.T) {
	for _, change := range []string{"empty", "missing", "symlink"} {
		t.Run(change, func(t *testing.T) {
			_, _, build := releaseFixture(t)
			zip := filepath.Join(build, "pith-0.1.0-arm64.zip")
			var err error
			switch change {
			case "empty":
				err = os.Truncate(zip, 0)
			case "missing":
				err = os.Remove(zip)
			case "symlink":
				if err = os.Remove(zip); err == nil {
					err = os.Symlink(filepath.Join(build, "darwin-arm64", "pith-0.1.0-arm64.dmg"), zip)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := stage("v0.1.0", build, filepath.Join(t.TempDir(), "assets"), false); err == nil {
				t.Fatal("accepted invalid source")
			}
		})
	}
}
