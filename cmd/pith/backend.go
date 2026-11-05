package main

import (
	"github.com/egoist/mygo"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/client"
	"github.com/Kel077/pith/internal/mock"
)

type service = api.Service

// newService returns the backend: the client of the helper, or with
// useMock an in-memory backend that simulates routing.
func newService(useMock bool) (service, error) {
	if useMock {
		return mock.New(mock.Options{Sample: true, Live: true, HelperInstalled: true, Running: true}), nil
	}
	res, err := mygo.App.Path(mygo.PathResources)
	if err != nil {
		return nil, err
	}
	return client.New(client.Options{ResourcesDir: res, Version: version})
}
