package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/swigger/urio-client/internal/platform"
	"golang.org/x/term"
)

// A small line-oriented TUI works in Windows Terminal, SSH and Unix terminals.
// Search and selection never rewrite a configuration containing credentials.
func chooseCity(cities []platform.Location) error {
	if len(cities) == 0 {
		return fmt.Errorf("no cities with online providers matched")
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)
	t := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stderr}, "> ")
	return chooseCityTerminal(t, cities)
}
func chooseCityTerminal(t *term.Terminal, cities []platform.Location) error {
	filtered := cities
	for {
		fmt.Fprint(t, "\r\nChoose an exit city (number), type a search, or q to cancel.\r\n")
		for i, l := range filtered {
			if i == 20 {
				fmt.Fprintf(t, "... %d more; type a narrower search\r\n", len(filtered)-20)
				break
			}
			fmt.Fprintf(t, "%2d  %s, %s, %s  [%d providers]\r\n", i+1, l.Name, l.Region, l.Country, l.Count)
		}
		line, err := t.ReadLine()
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "q" {
			return fmt.Errorf("city selection cancelled")
		}
		n, numberErr := strconv.Atoi(line)
		if numberErr == nil && n >= 1 && n <= len(filtered) && n <= 20 {
			l := filtered[n-1]
			fmt.Fprintf(t, "\r\nCopy into config.yaml:\r\nexit:\r\n  location_id: %q\r\n# %s, %s; %d providers currently online\r\n", l.ID, l.Name, l.Country, l.Count)
			return nil
		}
		filtered = nil
		for _, l := range cities {
			if strings.Contains(strings.ToLower(l.Name+" "+l.Region+" "+l.Country), strings.ToLower(line)) {
				filtered = append(filtered, l)
			}
		}
	}
}
