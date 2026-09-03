package filesystem

import (
	"bytes"
	"errors"
	"syscall"
	"unsafe"
)

// llistxattr wraps the llistxattr(2) syscall. The standard library does not
// export the symlink ("l") xattr variants on every platform, and like
// reflink.go's FICLONE ioctl this project calls the raw kernel interface
// rather than grow a dependency for three calls. With a nil buffer it
// returns the size the attribute-name list needs.
func llistxattr(path string, buffer []byte) (int, error) {
	pointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	destination := uintptr(0)
	if len(buffer) > 0 {
		destination = uintptr(unsafe.Pointer(&buffer[0]))
	}
	written, _, errno := syscall.Syscall(syscall.SYS_LLISTXATTR,
		uintptr(unsafe.Pointer(pointer)), destination, uintptr(len(buffer)))
	if errno != 0 {
		return 0, errno
	}
	return int(written), nil
}

// lgetxattr wraps the lgetxattr(2) syscall. With a nil buffer it returns the
// size the attribute's value needs.
func lgetxattr(path, name string, buffer []byte) (int, error) {
	pathPointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	destination := uintptr(0)
	if len(buffer) > 0 {
		destination = uintptr(unsafe.Pointer(&buffer[0]))
	}
	written, _, errno := syscall.Syscall6(syscall.SYS_LGETXATTR,
		uintptr(unsafe.Pointer(pathPointer)), uintptr(unsafe.Pointer(namePointer)),
		destination, uintptr(len(buffer)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(written), nil
}

// lsetxattr wraps the lsetxattr(2) syscall.
func lsetxattr(path, name string, value []byte, flags int) error {
	pathPointer, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	source := uintptr(0)
	if len(value) > 0 {
		source = uintptr(unsafe.Pointer(&value[0]))
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LSETXATTR,
		uintptr(unsafe.Pointer(pathPointer)), uintptr(unsafe.Pointer(namePointer)),
		source, uintptr(len(value)), uintptr(flags), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// copyXattrs reproduces a file's extended attributes. Attributes the caller
// is not allowed to set — security.* namespaces a non-root process cannot
// write, or filesystems without xattr support — are skipped, because
// reproducing them is a capability question rather than a copy failure.
// Anything else that fails to copy is an error.
func copyXattrs(source, destination string) error {
	needed, err := llistxattr(source, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			// The filesystem has no xattr support at all; nothing to copy.
			return nil
		}
		return err
	}
	if needed == 0 {
		return nil
	}
	buffer := make([]byte, needed)
	written, err := llistxattr(source, buffer)
	if err != nil {
		return err
	}
	for _, name := range splitNUL(buffer[:written]) {
		size, err := lgetxattr(source, name, nil)
		if err != nil {
			if errors.Is(err, syscall.ENOTSUP) {
				continue
			}
			return err
		}
		value := make([]byte, size)
		if _, err := lgetxattr(source, name, value); err != nil {
			return err
		}
		if err := lsetxattr(destination, name, value, 0); err != nil {
			switch {
			case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.ENOTSUP):
				continue
			default:
				return err
			}
		}
	}
	return nil
}

// splitNUL splits the NUL-separated list xattr syscalls return.
func splitNUL(list []byte) []string {
	names := make([]string, 0, 8)
	for _, section := range bytes.Split(list, []byte{0}) {
		if len(section) > 0 {
			names = append(names, string(section))
		}
	}
	return names
}
