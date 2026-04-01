// menu_helper is a test binary used by menu_test.go to test interactive
// terminal menus via a pseudo-terminal. It calls selectMenu or confirmPrompt
// and prints the result as JSON to stdout.
package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

type menuOption struct {
	Key   string
	Label string
}

func selectMenu(prompt string, options []menuOption, defaultIdx int) string {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "FALLBACK\n")
		return options[defaultIdx].Key
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return options[defaultIdx].Key
	}
	defer term.Restore(fd, oldState)

	selected := defaultIdx

	renderMenu := func() {
		for i, opt := range options {
			if i == selected {
				fmt.Fprintf(os.Stderr, "\r\033[K  \033[1;7m ▸ %s  %s \033[0m\n", opt.Key, opt.Label)
			} else {
				fmt.Fprintf(os.Stderr, "\r\033[K    %s  %s\n", opt.Key, opt.Label)
			}
		}
	}

	if prompt != "" {
		fmt.Fprintf(os.Stderr, "\n  %s\n\n", prompt)
	}
	renderMenu()
	fmt.Fprint(os.Stderr, "\033[?25l")
	defer fmt.Fprint(os.Stderr, "\033[?25h")

	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			break
		}
		if n == 1 && buf[0] == 3 {
			return options[selected].Key
		}
		if n == 1 && (buf[0] == '\r' || buf[0] == '\n') {
			fmt.Fprintln(os.Stderr)
			return options[selected].Key
		}
		if n == 3 && buf[0] == 0x1b && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				if selected > 0 {
					selected--
				}
			case 'B':
				if selected < len(options)-1 {
					selected++
				}
			}
			fmt.Fprintf(os.Stderr, "\033[%dA", len(options))
			renderMenu()
			continue
		}
		if n == 1 {
			key := strings.ToLower(string(buf[0]))
			for _, opt := range options {
				if strings.ToLower(opt.Key) == key {
					fmt.Fprintln(os.Stderr)
					return opt.Key
				}
			}
		}
	}
	return options[selected].Key
}

func confirmPrompt(prompt string) bool {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return false
	}

	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false
	}
	defer term.Restore(fd, oldState)

	buf := make([]byte, 1)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		fmt.Fprintln(os.Stderr, "n")
		return false
	}

	ch := strings.ToLower(string(buf[0]))
	if ch == "y" {
		fmt.Fprintln(os.Stderr, "y")
		return true
	}
	fmt.Fprintln(os.Stderr, "n")
	return false
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: menu_helper <select|numeric|confirm>")
		os.Exit(1)
	}

	mode := os.Args[1]
	switch mode {
	case "select":
		choice := selectMenu("Which action?", []menuOption{
			{"c", "continue"},
			{"s", "skip"},
			{"a", "abort"},
		}, 0)
		fmt.Printf(`{"result":"%s"}`, choice)

	case "numeric":
		choice := selectMenu("UAT Review", []menuOption{
			{"1", "Approve"},
			{"2", "Reject"},
			{"3", "Chat"},
		}, 0)
		fmt.Printf(`{"result":"%s"}`, choice)

	case "confirm":
		result := confirmPrompt("Approve?")
		fmt.Printf(`{"result":"%v"}`, result)

	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", mode)
		os.Exit(1)
	}
}
