package memory

import (
	"testing"
)

func TestPointID_Deterministic(t *testing.T) {
	id1 := pointID("hello world")
	id2 := pointID("hello world")
	if id1 != id2 {
		t.Errorf("expected same ID for same text, got %s and %s", id1, id2)
	}
}

func TestPointID_Different(t *testing.T) {
	id1 := pointID("hello")
	id2 := pointID("world")
	if id1 == id2 {
		t.Error("expected different IDs for different text")
	}
}

func TestPointID_Format(t *testing.T) {
	id := pointID("test")
	// Should be in UUID-like format: 8-4-4-4-12
	if len(id) != 36 {
		t.Errorf("expected 36 char UUID-like format, got %d: %s", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("unexpected format: %s", id)
	}
}

func TestIsExpired_NoField(t *testing.T) {
	payload := map[string]interface{}{"text": "hello"}
	if isExpired(payload) {
		t.Error("expected not expired when valid_until is missing")
	}
}

func TestIsExpired_FutureDate(t *testing.T) {
	payload := map[string]interface{}{"valid_until": "2099-12-31"}
	if isExpired(payload) {
		t.Error("expected not expired for future date")
	}
}

func TestIsExpired_PastDate(t *testing.T) {
	payload := map[string]interface{}{"valid_until": "2020-01-01"}
	if !isExpired(payload) {
		t.Error("expected expired for past date")
	}
}

func TestFormatTagsList(t *testing.T) {
	tests := []struct {
		input    interface{}
		expected string
	}{
		{nil, "[]"},
		{[]interface{}{"a", "b"}, "['a', 'b']"},
		{[]string{"x"}, "['x']"},
	}
	for _, tt := range tests {
		got := formatTagsList(tt.input)
		if got != tt.expected {
			t.Errorf("formatTagsList(%v) = %s, want %s", tt.input, got, tt.expected)
		}
	}
}
