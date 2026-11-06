// Package identity describes the exact helper artifact expected by a client.
// Reading an identity does not authenticate it; callers supply their trust anchor.
package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const Schema = 1
const MaxFileSize = 128 << 20

type Identity struct {
	Schema   int    `json:"schema"`
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	CDHash   string `json:"cdHash"`
}

func (id Identity) Validate() error {
	if id.Schema != Schema || id.Protocol <= 0 || id.Version == "" || len(id.Version) > 128 {
		return errors.New("invalid helper identity metadata")
	}
	for _, c := range id.Version {
		if c < 33 || c > 126 {
			return errors.New("invalid helper display version")
		}
	}
	for _, digest := range []struct {
		value string
		size  int
	}{{id.SHA256, sha256.Size}, {id.CDHash, 20}} {
		decoded, err := hex.DecodeString(digest.value)
		if err != nil || len(decoded) != digest.size || strings.ToLower(digest.value) != digest.value || bytes.Equal(decoded, make([]byte, digest.size)) {
			return errors.New("invalid helper identity digest")
		}
	}
	return nil
}

func Parse(data []byte) (Identity, error) {
	var id Identity
	if len(data) > 2048 {
		return id, errors.New("oversized helper identity")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&id); err != nil {
		return Identity{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Identity{}, errors.New("trailing helper identity data")
	}
	return id, id.Validate()
}

// Inspect reads the same open executable for its complete digest and CDHash.
// It is used on a trusted build input; it does not validate code page signatures.
func Inspect(path, version string, protocol int) (Identity, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return Identity{}, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Size() <= 0 || before.Size() > MaxFileSize {
		return Identity{}, errors.New("helper must be a bounded regular executable")
	}
	f, err := os.Open(path)
	if err != nil {
		return Identity{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return Identity{}, errors.New("helper identity changed while opening")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, before.Size())); err != nil {
		return Identity{}, err
	}
	cdHash, err := codeHash(f, before.Size())
	if err != nil {
		return Identity{}, err
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return Identity{}, errors.New("helper changed while reading")
	}
	id := Identity{Schema: Schema, Protocol: protocol, Version: version, SHA256: hex.EncodeToString(h.Sum(nil)), CDHash: cdHash}
	return id, id.Validate()
}

// Wire formats: XNU mach-o/loader.h and osfmk/kern/cs_blobs.h. Only the
// single SHA-256 CodeDirectory emitted by our ARM64 ad-hoc build is accepted.
func codeHash(r io.ReaderAt, size int64) (string, error) {
	var header [32]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return "", err
	}
	le := binary.LittleEndian
	if le.Uint32(header[0:4]) != 0xfeedfacf || le.Uint32(header[4:8]) != 0x0100000c || le.Uint32(header[12:16]) != 2 {
		return "", errors.New("helper must be a thin ARM64 Mach-O executable")
	}
	n, loadSize := le.Uint32(header[16:20]), le.Uint32(header[20:24])
	if n == 0 || n > 4096 || loadSize > 1<<20 || int64(loadSize) > size-32 {
		return "", errors.New("invalid Mach-O load command bounds")
	}
	loads := make([]byte, int(loadSize))
	if _, err := r.ReadAt(loads, 32); err != nil {
		return "", err
	}
	var signature []byte
	for i := uint32(0); i < n; i++ {
		if len(loads) < 8 {
			return "", errors.New("truncated Mach-O load command")
		}
		command, length := le.Uint32(loads[0:4]), le.Uint32(loads[4:8])
		if length < 8 || length%8 != 0 || uint64(length) > uint64(len(loads)) {
			return "", errors.New("invalid Mach-O load command length")
		}
		if command == 0x1d { // LC_CODE_SIGNATURE
			if length != 16 || signature != nil {
				return "", errors.New("ambiguous Mach-O signature")
			}
			offset, count := int64(le.Uint32(loads[8:12])), int64(le.Uint32(loads[12:16]))
			if count < 12 || count > 16<<20 || offset < 32+int64(loadSize) || offset > size-count {
				return "", errors.New("invalid Mach-O signature bounds")
			}
			signature = make([]byte, int(count))
			if _, err := r.ReadAt(signature, offset); err != nil {
				return "", err
			}
		}
		loads = loads[length:]
	}
	if len(loads) != 0 || signature == nil {
		return "", errors.New("missing or malformed Mach-O signature")
	}
	return signatureHash(signature)
}

func signatureHash(data []byte) (string, error) {
	be := binary.BigEndian
	if len(data) < 12 || be.Uint32(data[0:4]) != 0xfade0cc0 {
		return "", errors.New("invalid signature superblob")
	}
	length, count := uint64(be.Uint32(data[4:8])), uint64(be.Uint32(data[8:12]))
	if length > uint64(len(data)) || count == 0 || count > 32 || length < 12+count*8 {
		return "", errors.New("invalid signature index bounds")
	}
	data = data[:length]
	var directory []byte
	for i := uint64(0); i < count; i++ {
		slot := be.Uint32(data[12+i*8 : 16+i*8])
		offset := uint64(be.Uint32(data[16+i*8 : 20+i*8]))
		if offset < 12+count*8 || offset > length-8 {
			return "", errors.New("invalid signature blob offset")
		}
		blobSize := uint64(be.Uint32(data[offset+4 : offset+8]))
		if blobSize < 8 || blobSize > length-offset {
			return "", errors.New("invalid signature blob length")
		}
		blob := data[offset : offset+blobSize]
		if be.Uint32(blob[:4]) == 0xfade0c02 {
			if slot != 0 || directory != nil || len(blob) < 44 || blob[36] != 32 || blob[37] != 2 || be.Uint32(blob[12:16])&2 == 0 {
				return "", errors.New("expected one ad-hoc SHA-256 CodeDirectory")
			}
			directory = blob
		} else if slot == 0 {
			return "", errors.New("missing primary CodeDirectory")
		}
	}
	if directory == nil {
		return "", errors.New("missing CodeDirectory")
	}
	h := sha256.Sum256(directory)
	return fmt.Sprintf("%x", h[:20]), nil
}
