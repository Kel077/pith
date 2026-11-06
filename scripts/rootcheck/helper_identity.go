package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
)

func (s *session) freezeHelperIdentity() error {
	id, err := identity.Inspect(s.Helper, s.Version, helper.ProtocolVersion)
	if err != nil {
		return fmt.Errorf("inspect reviewed helper: %w", err)
	}
	if id.SHA256 != s.HelperSHA256 {
		return errors.New("helper changed while freezing the session identity")
	}
	s.HelperIdentity = &id
	return nil
}

func (s *session) frozenHelperIdentity() (identity.Identity, error) {
	if s.HelperIdentity == nil {
		return identity.Identity{}, errors.New("session has no frozen helper identity; use its verified uninstall plan")
	}
	id := *s.HelperIdentity
	if err := id.Validate(); err != nil || id.SHA256 != s.HelperSHA256 {
		return identity.Identity{}, errors.New("frozen helper identity does not match the reviewed session")
	}
	return id, nil
}

func (s *session) expectedHelper() (identity.Identity, error) {
	id, err := s.frozenHelperIdentity()
	if err != nil {
		return identity.Identity{}, err
	}
	if id.Protocol != helper.ProtocolVersion {
		return identity.Identity{}, errors.New("session helper protocol is unsupported; use its verified uninstall plan")
	}
	return id, nil
}

// Retain a verified recovery source in the private session. Its stable path
// keeps the reviewed uninstall script usable after the build input is replaced.
func (s *session) freezeCleanupHelper() (helper.InstallPlan, error) {
	id, err := s.frozenHelperIdentity()
	if err != nil {
		return helper.InstallPlan{}, err
	}
	if err := privateTempDirectory(s.Dir); err != nil {
		return helper.InstallPlan{}, err
	}
	in, err := os.Open(s.Helper)
	if err != nil {
		return helper.InstallPlan{}, err
	}
	defer in.Close()
	path := filepath.Join(s.Dir, "cleanup-helper")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return helper.InstallPlan{}, err
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, identity.MaxFileSize+1))
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return helper.InstallPlan{}, err
	}
	if _, err := s.cleanupIdentity(path); err != nil {
		return helper.InstallPlan{}, err
	}
	return helper.PlanVerifiedUninstall(path, id)
}

// A complete SHA-256 match is required before reading a missing legacy CDHash.
// Newly read metadata cannot replace the session's artifact trust anchor.
func (s *session) cleanupIdentity(path string) (identity.Identity, error) {
	checksum, err := fileHash(path)
	if err != nil || checksum != s.HelperSHA256 {
		return identity.Identity{}, errors.New("cleanup helper is not the session's reviewed executable")
	}
	if s.HelperIdentity == nil {
		id, err := identity.Inspect(path, s.Version, helper.ProtocolVersion)
		if err != nil || id.SHA256 != s.HelperSHA256 {
			return identity.Identity{}, errors.New("legacy cleanup helper identity could not be verified")
		}
		return id, nil
	}
	id, err := s.frozenHelperIdentity()
	if err != nil {
		return identity.Identity{}, err
	}
	actual, err := identity.Inspect(path, id.Version, id.Protocol)
	if err != nil || actual != id {
		return identity.Identity{}, errors.New("cleanup helper differs from the frozen session identity")
	}
	return id, nil
}

func (s *session) reviewedUninstallPlan(installed string) (helper.InstallPlan, error) {
	source := filepath.Join(s.Dir, "cleanup-helper")
	if s.HelperIdentity == nil {
		source = installed
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			source = s.Helper
		}
	}
	id, err := s.cleanupIdentity(source)
	if err != nil {
		return helper.InstallPlan{}, err
	}
	return helper.PlanVerifiedUninstall(source, id)
}
