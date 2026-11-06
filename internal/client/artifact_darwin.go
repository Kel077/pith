//go:build darwin

package client

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Kel077/pith/internal/helper/identity"
	"golang.org/x/sys/unix"
)

func openArtifact(path string, installed bool) (*os.File, artifactStamp, error) {
	if !filepath.IsAbs(path) {
		return nil, artifactStamp{}, errors.New("helper path must be absolute")
	}
	if installed {
		// The installed path is fixed, rooted in /Library. Its parents must not
		// permit an unprivileged replacement, even when the cached bytes match.
		for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
			var st unix.Stat_t
			if err := unix.Lstat(parent, &st); err != nil {
				return nil, artifactStamp{}, err
			}
			if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
				return nil, artifactStamp{}, errors.New("unverifiable helper parent directory")
			}
			if parent == "/" {
				break
			}
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, artifactStamp{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	stamp, err := artifactMetadata(f, installed)
	if err != nil {
		f.Close()
		return nil, artifactStamp{}, err
	}
	return f, stamp, nil
}

func artifactMetadata(f *os.File, installed bool) (artifactStamp, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return artifactStamp{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0111 == 0 || st.Mode&0022 != 0 || st.Size <= 0 || st.Size > identity.MaxFileSize || (installed && st.Uid != 0) {
		return artifactStamp{}, errors.New("unverifiable helper file metadata")
	}
	return artifactStamp{Device: uint64(st.Dev), Inode: st.Ino, Size: st.Size, Mode: uint32(st.Mode), UID: st.Uid, GID: st.Gid,
		Modified: [2]int64{st.Mtim.Sec, st.Mtim.Nsec}, Changed: [2]int64{st.Ctim.Sec, st.Ctim.Nsec}}, nil
}
