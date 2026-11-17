package main

import (
	"bytes"
	"os"
	"testing"
)

func pipeWith(t *testing.T, text string) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.WriteString(text); err != nil {
		t.Fatal(err)
	}
	// Leave the writer open, as a terminal stays open after Enter.
	t.Cleanup(func() { reader.Close(); writer.Close() })
	return reader
}

func TestReadSecretFromAPipeReadsToTheEndWithoutAPrompt(t *testing.T) {
	var prompts bytes.Buffer
	reader, writer, _ := os.Pipe()
	writer.WriteString("abc\ndef\n")
	writer.Close()
	raw, err := readSecretFromStandardInput(reader, &prompts, func(*os.File) (func(), bool) { return nil, false })
	if err != nil || string(raw) != "abc\ndef\n" || prompts.Len() != 0 {
		t.Fatalf("raw=%q prompts=%q err=%v", raw, prompts.String(), err)
	}
}

func TestReadSecretFromATerminalTurnsEchoOffReadsOneLineAndTurnsEchoOn(t *testing.T) {
	var prompts bytes.Buffer
	echoOff, echoRestored := false, false
	raw, err := readSecretFromStandardInput(pipeWith(t, "pasted-key\n"), &prompts, func(*os.File) (func(), bool) {
		echoOff = true
		return func() { echoRestored = true }, true
	})
	if err != nil || string(raw) != "pasted-key\n" {
		t.Fatalf("raw=%q err=%v", raw, err)
	}
	if !echoOff || !echoRestored {
		t.Fatalf("echo turned off=%t, restored=%t", echoOff, echoRestored)
	}
	if !bytes.Contains(prompts.Bytes(), []byte("press Enter")) {
		t.Fatalf("no prompt: %q", prompts.String())
	}
}
