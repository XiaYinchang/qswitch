package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// readSecret prompts on stderr and reads one line from stdin with echo off.
// Bytes are read one at a time so a second prompt gets the second line.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if restore, err := disableEcho(os.Stdin); err == nil {
		defer func() {
			restore()
			fmt.Fprintln(os.Stderr)
		}()
	}
	return readLine(os.Stdin)
}

func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				break
			}
			return "", err
		}
	}
	return strings.TrimSuffix(b.String(), "\r"), nil
}
