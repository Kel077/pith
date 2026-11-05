package main

import (
	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
)

type service = api.Service

// newService returns the backend: the mock, for now the only one on this
// branch.
func newService(useMock bool) (service, error) {
	return mock.New(mock.Options{Sample: true, Live: true}), nil
}
