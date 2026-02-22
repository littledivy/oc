package main

import "golang.org/x/sys/unix"

// patchRawTermios re-enables OPOST for output processing and disables VDISCARD
// so Ctrl+O (byte 15) passes through to the application.
func patchRawTermios(fd int) {
	if termios, err := unix.IoctlGetTermios(fd, unix.TCGETS); err == nil {
		termios.Oflag |= unix.OPOST
		termios.Cc[unix.VDISCARD] = 0
		unix.IoctlSetTermios(fd, unix.TCSETS, termios)
	}
}
