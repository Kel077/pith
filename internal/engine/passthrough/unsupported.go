//go:build !darwin

package passthrough

import (
	"errors"
	"github.com/Kel077/pith/internal/engine/capture"
)

type nativeSender struct{}

func newSender(...*capture.Guard) Sender { return nativeSender{} }
func (nativeSender) Send(string, Packet) error {
	return errors.New("native socket preservation requires macOS")
}
func (nativeSender) Close() error { return nil }
