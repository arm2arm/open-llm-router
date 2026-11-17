package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/buildinfo"
)

func TestVersionNeedsNoConfigOrDatabase(t *testing.T) {
	var output bytes.Buffer
	if err := versionCommand(nil, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{buildinfo.Version, buildinfo.Commit, buildinfo.GoVersion()} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing build identity %q", want)
		}
	}
	if err := versionCommand([]string{"unexpected"}, &output); err == nil {
		t.Fatal("extra arguments accepted")
	}
}
