package hsmkeys

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/bide-ai/bide/signer/pkcs11"
)

// ErrNoTerminal means pin-source=prompt was asked for and the process has no controlling terminal
// to ask on. The PIN is never read from standard input or any pipe: a prompt that quietly read a
// pipe would take whatever a script or another program wrote to it.
var ErrNoTerminal = errors.New("pin-source=prompt needs a terminal: this process has no controlling terminal, and the PIN is never read from a pipe (use pin-source=file: or env:)")

// promptPIN returns a PIN source that asks on the controlling terminal with echo off each time a
// session opens. The terminal is opened by name (/dev/tty, or CONIN$ on Windows), not taken from
// standard input, so redirecting stdin does not change where the PIN comes from.
func promptPIN(label string) pkcs11.PINSource {
	return func() (string, error) { return readPIN(fmt.Sprintf("PIN for token %q: ", label)) }
}

// pinMemo remembers a prompted PIN for the attempts of one Open (detection may open the token two or
// three times), so the approver is asked once. Open forgets it when it returns: a session the signer
// reopens later asks again, and the PIN is not held in memory between uses.
type pinMemo struct {
	mu  sync.Mutex
	pin string
	set bool
}

func (m *pinMemo) source(label string) pkcs11.PINSource {
	ask := promptPIN(label)
	return func() (string, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.set {
			return m.pin, nil
		}
		pin, err := ask()
		if err != nil {
			return "", err
		}
		m.pin, m.set = pin, true
		return pin, nil
	}
}

func (m *pinMemo) forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pin, m.set = "", false
}

// readPIN asks for a PIN with prompt. A test replaces it.
var readPIN = readPINFromTerminal

// ttyNames are the names of the controlling terminal for reading and for writing. A test replaces
// them to point at a file that is not a terminal.
var ttyNames = func() (in, out string) {
	if runtime.GOOS == "windows" {
		return "CONIN$", "CONOUT$"
	}
	return "/dev/tty", "/dev/tty"
}

func readPINFromTerminal(prompt string) (string, error) {
	in, out := ttyNames()
	f, err := os.OpenFile(in, os.O_RDWR, 0)
	if err != nil {
		return "", ErrNoTerminal
	}
	defer f.Close()
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return "", ErrNoTerminal
	}
	w := f
	if out != in {
		if o, err := os.OpenFile(out, os.O_WRONLY, 0); err == nil {
			defer o.Close()
			w = o
		}
	}
	fmt.Fprint(w, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(w)
	if err != nil {
		return "", errors.New("could not read the PIN from the terminal")
	}
	pin := strings.TrimRight(string(b), "\r\n")
	if pin == "" {
		return "", errors.New("no PIN was entered")
	}
	return pin, nil
}
