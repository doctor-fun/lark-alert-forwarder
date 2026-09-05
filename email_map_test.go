package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEmailMapHappyPath(t *testing.T) {
	input := `
# oncall team
ou_aaa = alice@example.com
ou_bbb=bob@example.com

# managers
ou_ccc = carol@example.com
`
	m, err := parseEmailMap(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(m), m)
	}
	if m["ou_aaa"] != "alice@example.com" {
		t.Errorf("ou_aaa=%q", m["ou_aaa"])
	}
	if m["ou_bbb"] != "bob@example.com" {
		t.Errorf("ou_bbb=%q", m["ou_bbb"])
	}
}

func TestParseEmailMapRejectsDuplicate(t *testing.T) {
	input := "ou_aaa=a@x.com\nou_aaa=b@x.com\n"
	if _, err := parseEmailMap(strings.NewReader(input)); err == nil {
		t.Fatal("expected duplicate key error")
	}
}

func TestParseEmailMapRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"no_equals_sign",
		"=value_no_key",
		"key_no_value=",
	} {
		if _, err := parseEmailMap(strings.NewReader(bad)); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestParseEmailMapSkipsBlankAndComments(t *testing.T) {
	input := "\n\n# comment\n  # indented comment\n  \n"
	m, err := parseEmailMap(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestFileEmailMapLookupCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.txt")
	if err := os.WriteFile(path, []byte("ou_AAA=alice@example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fm, err := NewFileEmailMap(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// lookup with different case
	if got := fm.Lookup("OU_aaa"); got != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %q", got)
	}
	if got := fm.Lookup("ou_aaa"); got != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %q", got)
	}
	if got := fm.Lookup("ou_bbb"); got != "" {
		t.Errorf("expected empty for unknown key, got %q", got)
	}
}

func TestFileEmailMapEmptyPathReturnsNoopMap(t *testing.T) {
	fm, err := NewFileEmailMap("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := fm.Lookup("ou_anything"); got != "" {
		t.Errorf("expected empty from noop map, got %q", got)
	}
	if fm.Size() != 0 {
		t.Errorf("expected size=0, got %d", fm.Size())
	}
}

func TestFileEmailMapRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.txt")
	if err := os.WriteFile(path, []byte("ou_aaa=old@example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fm, err := NewFileEmailMap(path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := fm.Lookup("ou_aaa"); got != "old@example.com" {
		t.Fatalf("initial lookup: %q", got)
	}
	// overwrite the file
	if err := os.WriteFile(path, []byte("ou_aaa=new@example.com\nou_bbb=bob@example.com\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// wait for refresh
	time.Sleep(300 * time.Millisecond)
	if got := fm.Lookup("ou_aaa"); got != "new@example.com" {
		t.Errorf("after refresh: ou_aaa=%q", got)
	}
	if got := fm.Lookup("ou_bbb"); got != "bob@example.com" {
		t.Errorf("after refresh: ou_bbb=%q", got)
	}
	if fm.Size() != 2 {
		t.Errorf("expected size=2, got %d", fm.Size())
	}
}

func TestStaticEmailMapLookup(t *testing.T) {
	m := staticEmailMap{"ou_aaa": "a@x.com", "ou_bbb": "b@x.com"}
	if got := m.Lookup("ou_aaa"); got != "a@x.com" {
		t.Errorf("got %q", got)
	}
	if got := m.Lookup("ou_ccc"); got != "" {
		t.Errorf("got %q for missing key", got)
	}
	var nilMap staticEmailMap
	if got := nilMap.Lookup("ou_aaa"); got != "" {
		t.Errorf("nil map should return empty, got %q", got)
	}
}
