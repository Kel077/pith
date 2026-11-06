package main

import (
	"errors"

	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
)

func (s *session) expectedHelper() (identity.Identity, error) {
	id, err := identity.Inspect(s.Helper, s.Version, helper.ProtocolVersion)
	if err != nil || id.SHA256 != s.HelperSHA256 {
		return identity.Identity{}, errors.New("helper differs from the frozen session identity; prepare a new session")
	}
	return id, nil
}
