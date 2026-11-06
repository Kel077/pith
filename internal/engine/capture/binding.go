package capture

import "net"

// A known index is not an identity by itself: the kernel can reuse it for a
// different name. Epoch/generation checks remain the Guard's responsibility.
func checkBoundInterface(name string, index int, lookup func(int) (*net.Interface, error)) error {
	if name == "" || index <= 0 {
		return ErrEgressChanged
	}
	current, err := lookup(index)
	if err != nil || current == nil || current.Name != name || current.Index != index {
		return ErrEgressChanged
	}
	return nil
}
