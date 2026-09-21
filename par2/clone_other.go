//go:build !darwin && !linux

package par2

import "errors"

// cloneFile is unavailable here; repair falls back to copying survivors.
func cloneFile(src, dst string) error {
	return errors.ErrUnsupported
}
