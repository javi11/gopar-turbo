package par2

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dst a reflink clone of src (FICLONE: btrfs, XFS with
// reflink, bcachefs). It costs no data I/O, so a repair can start from the
// damaged original and rewrite only the slices it reconstructs. It fails on
// filesystems without reflink support, and the caller falls back to a copy.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
