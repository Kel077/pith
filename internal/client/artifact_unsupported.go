//go:build !darwin

package client

import (
	"errors"
	"os"
)

func openArtifact(string, bool) (*os.File, artifactStamp, error) {
	return nil, artifactStamp{}, errors.New("helper identity requires macOS")
}

func artifactMetadata(*os.File, bool) (artifactStamp, error) {
	return artifactStamp{}, errors.New("helper identity requires macOS")
}
