package memory

import (
	"testing"
	"time"
)

func TestCache_SetAndGet(t *testing.T) {
	c := NewCache(1 * time.Second)
	data := []map[string]interface{}{{"text": "hello"}}

	c.Set("key1", data)

	got, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 1 || got[0]["text"] != "hello" {
		t.Errorf("unexpected data: %v", got)
	}
}

func TestCache_Miss(t *testing.T) {
	c := NewCache(1 * time.Second)

	_, ok := c.Get("nonexistent")
	if ok {
		t.Error("expected cache miss")
	}
}

func TestCache_Expiry(t *testing.T) {
	c := NewCache(10 * time.Millisecond)
	c.Set("key1", []map[string]interface{}{{"text": "hello"}})

	time.Sleep(20 * time.Millisecond)

	_, ok := c.Get("key1")
	if ok {
		t.Error("expected cache miss after expiry")
	}
}

func TestCache_Invalidate(t *testing.T) {
	c := NewCache(1 * time.Minute)
	c.Set("key1", []map[string]interface{}{{"text": "hello"}})

	c.Invalidate()

	_, ok := c.Get("key1")
	if ok {
		t.Error("expected cache miss after invalidate")
	}
}
