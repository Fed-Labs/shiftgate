// Package termio reads secrets from the terminal without echoing them. Echo
// suppression is the only honest way to take a password interactively, and it
// is done with termios ioctls — no dependency beyond the standard library.
// When stdin is not a terminal (a script piping the secret in) the read
// degrades to a plain line read, which is exactly what the script wanted.
package termio

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// ReadPassword reads one line from file. When file is a terminal the line is
// not echoed and a newline is emitted afterwards so the next prompt starts on
// a fresh line; when it is not a terminal the line is read as-is.
func ReadPassword(file *os.File) (string, error) {
	var state syscall.Termios
	if err := ioctlTermios(file.Fd(), syscall.TCGETS, &state); err != nil {
		// ENOTTY and friends: a pipe or a redirection. Read plainly — the
		// caller chose non-interactive input.
		return readLine(file)
	}
	saved := state
	state.Lflag &^= syscall.ECHO
	if err := ioctlTermios(file.Fd(), syscall.TCSETS, &state); err != nil {
		return "", err
	}
	line, readErr := readLine(file)
	// Restore the terminal whatever the read did; a broken prompt on exit is
	// worse than a lost error here.
	_ = ioctlTermios(file.Fd(), syscall.TCSETS, &saved)
	_, _ = fmt.Fprintln(file)
	return line, readErr
}

func readLine(file *os.File) (string, error) {
	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func ioctlTermios(fd, request uintptr, termios *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(termios)))
	if errno != 0 {
		return errno
	}
	return nil
}
