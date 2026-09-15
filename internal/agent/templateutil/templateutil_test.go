package templateutil

import (
	"strings"
	"testing"
)

func TestRender_Simple(t *testing.T) {
	got, err := Render("test", "Hello, {{.Name}}!", struct{ Name string }{"World"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "Hello, World!"
	if got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestRender_NoPlaceholders(t *testing.T) {
	got, err := Render("test", "static text", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "static text" {
		t.Errorf("Render() = %q, want %q", got, "static text")
	}
}

func TestRender_NilData(t *testing.T) {
	got, err := Render("test", "no data needed", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "no data needed" {
		t.Errorf("Render() = %q, want %q", got, "no data needed")
	}
}

func TestRender_MapData(t *testing.T) {
	data := map[string]string{"Key": "value"}
	got, err := Render("test", "{{.Key}}", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "value" {
		t.Errorf("Render() = %q, want %q", got, "value")
	}
}

func TestRender_ParseError(t *testing.T) {
	_, err := Render("bad", "{{.Unclosed", nil)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "template parse") {
		t.Errorf("error should mention 'template parse', got: %v", err)
	}
}

func TestRender_ExecuteError(t *testing.T) {
	// Accessing a field that doesn't exist on a struct
	_, err := Render("exec", "{{.Missing}}", struct{ Name string }{"test"})
	if err == nil {
		t.Fatal("expected execute error, got nil")
	}
	if !strings.Contains(err.Error(), "template execute") {
		t.Errorf("error should mention 'template execute', got: %v", err)
	}
}

func TestRender_EmptyTemplate(t *testing.T) {
	got, err := Render("empty", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("Render() = %q, want empty string", got)
	}
}

func TestMustRender_Simple(t *testing.T) {
	got := MustRender("test", "Hello, {{.Name}}!", struct{ Name string }{"World"})
	want := "Hello, World!"
	if got != want {
		t.Errorf("MustRender() = %q, want %q", got, want)
	}
}

func TestMustRender_ParseError_ReturnsPlaceholder(t *testing.T) {
	got := MustRender("bad", "{{.Unclosed", nil)
	if !strings.Contains(got, "[template render error:") {
		t.Errorf("MustRender with bad template should return placeholder, got %q", got)
	}
	if !strings.Contains(got, "bad") {
		t.Errorf("placeholder should contain template name, got %q", got)
	}
}

func TestMustRender_ExecuteError_ReturnsPlaceholder(t *testing.T) {
	got := MustRender("exec", "{{.Missing}}", struct{ Name string }{"test"})
	if !strings.Contains(got, "[template render error:") {
		t.Errorf("MustRender with exec error should return placeholder, got %q", got)
	}
}
