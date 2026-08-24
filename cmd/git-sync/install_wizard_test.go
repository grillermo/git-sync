package main

import (
	"io"
	"strings"
	"testing"
)

func TestConfirmQuitsOnQ(t *testing.T) {
	if confirm(io.Discard, strings.NewReader("q\n"), "?") {
		t.Error("q should quit the wizard")
	}
}

func TestConfirmContinuesOnEnter(t *testing.T) {
	if !confirm(io.Discard, strings.NewReader("\n"), "?") {
		t.Error("a bare enter should continue")
	}
}

func TestConfirmQuitsOnEOF(t *testing.T) {
	// Ctrl-D is a quit, not a blank continue.
	if confirm(io.Discard, strings.NewReader(""), "?") {
		t.Error("EOF should quit")
	}
}
