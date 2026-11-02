package termio

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// IsTerminal reports whether file is a terminal (a tty), the switch that
// decides between prompting a human and demanding an explicit flag from a
// script.
func IsTerminal(file *os.File) bool {
	var state syscall.Termios
	return ioctlTermios(file.Fd(), syscall.TCGETS, &state) == nil
}

// ReadLine reads one echoed line from file. It is the prompt-friendly sibling
// of ReadPassword: confirmations are not secrets.
func ReadLine(file *os.File) (string, error) {
	line, err := readLine(bufio.NewReader(file))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Confirm asks a y/N question on the terminal and reports the answer.
func Confirm(file *os.File, prompt string) (bool, error) {
	_, _ = fmt.Fprintf(file, "%s [y/N] ", prompt)
	answer, err := ReadLine(file)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
