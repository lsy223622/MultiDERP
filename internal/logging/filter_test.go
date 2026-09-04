package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestFilterHonorsConfiguredLevel(t *testing.T) {
	var output bytes.Buffer
	filter := New(log.New(&output, "", 0), "warn")
	filter.Printf("INFO hidden")
	filter.Printf("WARN visible")
	filter.Printf("ERROR also visible")
	if got := output.String(); got != "WARN visible\nERROR also visible\n" {
		t.Fatalf("filtered output = %q", got)
	}

	output.Reset()
	filter.SetLevel("debug")
	filter.Printf("DEBUG visible")
	if !strings.Contains(output.String(), "DEBUG visible") {
		t.Fatalf("debug message was filtered: %q", output.String())
	}
}
