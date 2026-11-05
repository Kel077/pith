//go:build !darwin

package attribution

import (
	"errors"
	"github.com/Kel077/pith/internal/api"
)

func Snapshot(api.Protocol) ([]Socket, error) {
	return nil, errors.New("PCB attribution requires macOS")
}
func ProcessPath(int) (string, error) { return "", errors.New("process paths require macOS") }
func processIdentity(int) (identity, error) {
	return identity{}, errors.New("process identity requires macOS")
}
