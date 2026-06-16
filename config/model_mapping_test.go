package config

import (
	"os"
	"strings"
	"testing"
)

func TestParseModelMappingsTrimsAndSkipsEmptyPairs(t *testing.T) {
	got, err := ParseModelMappings(strings.NewReader(`{
		" gpt-5.5 ": " glm-51 ",
		"": "ignored",
		"empty-target": " "
	}`))
	if err != nil {
		t.Fatalf("ParseModelMappings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %#v", len(got), got)
	}
	if got["gpt-5.5"] != "glm-51" {
		t.Fatalf("mapping = %#v, want gpt-5.5 -> glm-51", got)
	}
}

func TestLoadModelMappingsFromFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "model-mapping-*.json")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(`{"gpt-5.5":"glm-51"}`); err != nil {
		t.Fatalf("write mapping file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close mapping file: %v", err)
	}

	got, err := LoadModelMappingsFromFile(f.Name())
	if err != nil {
		t.Fatalf("LoadModelMappingsFromFile: %v", err)
	}
	if got["gpt-5.5"] != "glm-51" {
		t.Fatalf("mapping = %#v, want gpt-5.5 -> glm-51", got)
	}
}
