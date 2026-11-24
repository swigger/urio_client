package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/swigger/urio-client/internal/platform"
	"golang.org/x/term"
)

func TestCityTerminalSearchAndSnippet(t *testing.T) {
	var output bytes.Buffer
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{strings.NewReader("singapore\r1\r"), &output}, "> ")
	err := chooseCityTerminal(terminal, []platform.Location{{ID: "other-id", Name: "Tokyo", Country: "Japan", Count: 3}, {ID: "chosen-id", Name: "Singapore", Country: "Singapore", Count: 5}})
	if err != nil || !strings.Contains(output.String(), `location_id: "chosen-id"`) {
		t.Fatal(err, output.String())
	}
}
