//go:build !darwin

package passthrough

import "errors"

type nativeSender struct{}

func newSender() Sender { return nativeSender{} }
func (nativeSender) Send(string, Packet) error {
	return errors.New("native socket preservation requires macOS")
}
func (nativeSender) Close() error { return nil }
