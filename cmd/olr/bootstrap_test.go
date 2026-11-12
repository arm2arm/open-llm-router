package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPasswordFilesArePrivateAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	for _, ending := range []string{"", "\n", "\r\n"} {
		if err := os.WriteFile(path, []byte("  preserve password spaces  "+ending), 0600); err != nil {
			t.Fatal(err)
		}
		password, err := readPasswordFile(path)
		if err != nil || string(password) != "  preserve password spaces  " {
			t.Fatal("private password file or newline handling", err)
		}
	}
	if err := os.WriteFile(path, []byte("preserve final carriage return\r"), 0600); err != nil {
		t.Fatal(err)
	}
	if password, err := readPasswordFile(path); err != nil || string(password) != "preserve final carriage return\r" {
		t.Fatal("a standalone carriage return was trimmed", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if password, err := readPasswordFile(path); err == nil || password != nil {
		t.Fatal("public password file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 1027)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, filepath.Dir(path), path + "-private-path"} {
		if password, err := readPasswordFile(file); err == nil || password != nil || strings.Contains(err.Error(), file) {
			t.Fatal("invalid password file was accepted or leaked its path")
		}
	}
	fifo := path + "-fifo"
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if password, err := readPasswordFile(fifo); err == nil || password != nil {
		t.Fatal("FIFO password source accepted")
	}
}
