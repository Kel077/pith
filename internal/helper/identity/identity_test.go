package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixtureMachO() ([]byte, []byte) {
	directory := make([]byte, 64)
	be, le := binary.BigEndian, binary.LittleEndian
	be.PutUint32(directory[0:4], 0xfade0c02)
	be.PutUint32(directory[4:8], uint32(len(directory)))
	be.PutUint32(directory[8:12], 0x20400)
	be.PutUint32(directory[12:16], 2)
	directory[36], directory[37] = 32, 2
	signature := make([]byte, 20)
	be.PutUint32(signature[0:4], 0xfade0cc0)
	be.PutUint32(signature[4:8], uint32(len(signature)+len(directory)))
	be.PutUint32(signature[8:12], 1)
	be.PutUint32(signature[16:20], 20)
	signature = append(signature, directory...)
	file := make([]byte, 48)
	le.PutUint32(file[0:4], 0xfeedfacf)
	le.PutUint32(file[4:8], 0x0100000c)
	le.PutUint32(file[12:16], 2)
	le.PutUint32(file[16:20], 1)
	le.PutUint32(file[20:24], 16)
	le.PutUint32(file[32:36], 0x1d)
	le.PutUint32(file[36:40], 16)
	le.PutUint32(file[40:44], uint32(len(file)))
	le.PutUint32(file[44:48], uint32(len(signature)))
	return append(file, signature...), directory
}

func TestCodeHash(t *testing.T) {
	file, directory := fixtureMachO()
	h := sha256.Sum256(directory)
	got, err := codeHash(bytes.NewReader(file), int64(len(file)))
	if err != nil || got != fmt.Sprintf("%x", h[:20]) {
		t.Fatalf("got %q, %v", got, err)
	}
	for name, mutate := range map[string]func([]byte){
		"wrong CPU":       func(b []byte) { b[4] = 7 },
		"oversized loads": func(b []byte) { binary.LittleEndian.PutUint32(b[20:24], 1<<24) },
		"short command":   func(b []byte) { b[36] = 0 },
		"bad offset":      func(b []byte) { binary.LittleEndian.PutUint32(b[40:44], 0xffffffff) },
		"bad blob count":  func(b []byte) { binary.BigEndian.PutUint32(b[56:60], 0xffffffff) },
		"bad slot":        func(b []byte) { binary.BigEndian.PutUint32(b[60:64], 0x1000) },
		"bad hash":        func(b []byte) { b[68+37] = 1 },
		"not ad hoc":      func(b []byte) { binary.BigEndian.PutUint32(b[68+12:68+16], 0) },
		"bad blob length": func(b []byte) { binary.BigEndian.PutUint32(b[68+4:68+8], 0xffffffff) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := bytes.Clone(file)
			mutate(bad)
			if _, err := codeHash(bytes.NewReader(bad), int64(len(bad))); err == nil {
				t.Fatal("accepted malformed identity")
			}
		})
	}
	for end := range len(file) {
		if _, err := codeHash(bytes.NewReader(file[:end]), int64(end)); err == nil {
			t.Fatalf("accepted truncation at %d", end)
		}
	}
}

func TestInspectAndParse(t *testing.T) {
	file, _ := fixtureMachO()
	path := filepath.Join(t.TempDir(), "pithd")
	if err := os.WriteFile(path, file, 0755); err != nil {
		t.Fatal(err)
	}
	id, err := Inspect(path, "0.1.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(file)
	if id.SHA256 != fmt.Sprintf("%x", h) || id.Version != "0.1.0" || id.Protocol != 1 {
		t.Fatal("incorrect identity", id)
	}
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil || parsed != id {
		t.Fatal(parsed, err)
	}
	merged := id
	merged.SHA256 = merged.CDHash
	if err := merged.Validate(); err == nil {
		t.Fatal("SHA-256 and CDHash roles were merged instead of validating both lengths")
	}
	for _, raw := range []string{string(data) + "{}", strings.Replace(string(data), `"schema":1`, `"schema":2`, 1), strings.Replace(string(data), `"version":"0.1.0"`, `"unexpected":true`, 1), `{}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(link, "0.1.0", 1); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestNativeGoExecutableIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native ARM64 Mach-O only")
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(path, "test", 1); err != nil {
		t.Fatal(err)
	}
}

func FuzzCodeHash(f *testing.F) {
	file, _ := fixtureMachO()
	f.Add(file)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = codeHash(bytes.NewReader(data), int64(len(data)))
	})
}
