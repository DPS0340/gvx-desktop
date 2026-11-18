// Package term runs a program on a terminal and keeps its screen: the
// program's output is parsed by gostty, Ghostty's terminal core, and the
// user's keys, pastes and mouse are encoded for it under the modes it set.
//
// It is mako's session and terminal connection made one: mako parses a
// program's output in its daemon, which answers the program's queries,
// and again in its window, which draws it. Here the program runs in the
// app, so one terminal does both.
package term

import (
	"io"
	"strings"
	"time"
)

// A Program is what a terminal runs: a process on a pseudo-terminal, or a
// stream to one elsewhere, as a container's exec is. Reading it reads what
// the program writes to its terminal, and writing it types.
type Program interface {
	io.Reader
	io.Writer
	// Resize sizes the program's terminal cols by rows of cells cellW by
	// cellH pixels, which zero leaves unsaid.
	Resize(cols, rows, cellW, cellH uint16) error
	// Wait waits for the program to exit and is its exit code, -1 when it
	// says none.
	Wait() int
	// EndOutput ends reading the program's output after grace, the
	// program gone: a process it left holding the terminal may otherwise
	// keep the output from ever ending.
	EndOutput(grace time.Duration)
	// Kill ends the program, and what it started.
	Kill()
	// Close releases the terminal.
	Close() error
}

// Start starts argv with env on a pseudo-terminal cols by rows: a Unix
// pty, or a Windows pseudoconsole. env is the process's environment
// whole; Env makes one.
func Start(argv []string, env []string, cols, rows uint16) (Program, error) {
	return startProc(argv, env, max(cols, 2), max(rows, 2))
}

// Env is environ as a program on a terminal sees it: with the terminal
// described as the one it runs in, not whichever one the app was started
// from. That terminal is gostty, Ghostty's core, which speaks xterm's
// 256 colours and 24-bit colour.
func Env(environ []string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "TERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "COLORTERM",
			"LC_TERMINAL", "LC_TERMINAL_VERSION", "ITERM_SESSION_ID", "ITERM_PROFILE",
			"GHOSTTY_RESOURCES_DIR", "GHOSTTY_BIN_DIR", "GHOSTTY_SHELL_FEATURES",
			"KITTY_WINDOW_ID", "WT_SESSION", "VSCODE_INJECTION":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}
