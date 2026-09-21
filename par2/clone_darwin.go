package par2

import "golang.org/x/sys/unix"

// cloneFile makes dst a copy-on-write clone of src (APFS clonefile). It
// costs no data I/O, so a repair can start from the damaged original and
// rewrite only the slices it reconstructs.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, 0)
}
