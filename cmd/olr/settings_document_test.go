package main

import (
	"errors"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsDocumentLimitMatchesManagedService(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routing.json")
	for _, size := range []int{80 << 10, settings.MaxDocumentBytes, settings.MaxDocumentBytes + 1} {
		if err := os.WriteFile(p, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
		data, err := readDocument(p)
		if size <= settings.MaxDocumentBytes {
			if err != nil || len(data) != size {
				t.Fatal("valid document refused", size, err)
			}
		} else if !errors.Is(err, settings.ErrDocumentSize) {
			t.Fatal("different document limit", err)
		}
	}
}
