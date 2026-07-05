package quran

import "testing"

// TestByteBudgetEviction checks the LRU evicts oldest entries once the byte
// budget is exceeded, and curBytes stays within budget.
func TestByteBudgetEviction(t *testing.T) {
	c := &Client{cache: make(map[string][]byte), maxBytes: 1000}

	put := func(key string, n int) { c.putLocked(key, make([]byte, n)) }

	put("a", 400)
	put("b", 400)
	if c.curBytes != 800 {
		t.Fatalf("curBytes = %d, want 800", c.curBytes)
	}
	// "c" (400) pushes total to 1200 > 1000 → evict oldest ("a").
	put("c", 400)
	if _, ok := c.cache["a"]; ok {
		t.Errorf("oldest entry 'a' should have been evicted")
	}
	if _, ok := c.cache["b"]; !ok {
		t.Errorf("'b' should still be cached")
	}
	if _, ok := c.cache["c"]; !ok {
		t.Errorf("'c' should be cached")
	}
	if c.curBytes > c.maxBytes {
		t.Errorf("curBytes %d exceeds budget %d", c.curBytes, c.maxBytes)
	}

	// Duplicate insert is a no-op (no double counting).
	before := c.curBytes
	put("c", 400)
	if c.curBytes != before {
		t.Errorf("duplicate insert changed curBytes: %d -> %d", before, c.curBytes)
	}
}
