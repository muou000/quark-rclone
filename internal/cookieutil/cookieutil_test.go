package cookieutil

import (
	"strings"
	"testing"
)

func TestSetReplacesExisting(t *testing.T) {
	got := Set("a=1; __puus=old; b=2", "__puus", "new")
	if got != "a=1; __puus=new; b=2" {
		t.Fatalf("Set() = %q", got)
	}
}

func TestSetAppendsWhenAbsent(t *testing.T) {
	got := Set("a=1", "__puus", "new")
	if got != "a=1; __puus=new" {
		t.Fatalf("Set() = %q", got)
	}
}

func TestSetOnEmpty(t *testing.T) {
	if got := Set("", "__puus", "v"); got != "__puus=v" {
		t.Fatalf("Set() = %q", got)
	}
}

func TestSetIsCaseInsensitiveOnName(t *testing.T) {
	got := Set("__PUUS=old", "__puus", "new")
	if got != "__puus=new" {
		t.Fatalf("Set() = %q, want the canonical name", got)
	}
}

func TestGet(t *testing.T) {
	header := "a=1; __puus=abc; b=2"
	if got := Get(header, "__puus"); got != "abc" {
		t.Fatalf("Get() = %q, want %q", got, "abc")
	}
	if got := Get(header, "missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
	if got := Get(header, "A"); got != "1" {
		t.Fatalf("Get(A) = %q, want %q (case-insensitive)", got, "1")
	}
}

func TestDel(t *testing.T) {
	got := Del("a=1; __puus=abc; b=2", "__puus")
	if got != "a=1; b=2" {
		t.Fatalf("Del() = %q", got)
	}
}

func TestDelAbsentIsNoop(t *testing.T) {
	if got := Del("a=1", "__puus"); got != "a=1" {
		t.Fatalf("Del() = %q", got)
	}
}

func TestValidateTrimsAndAcceptsUnknownCookies(t *testing.T) {
	in := "  __puus=abc; mystery=x=y; b=2  "
	got, err := Validate(in)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != "__puus=abc; mystery=x=y; b=2" {
		t.Fatalf("Validate() = %q", got)
	}
}

func TestValidateRejectsEmpty(t *testing.T) {
	if _, err := Validate("   "); err == nil {
		t.Fatal("Validate should reject an empty cookie")
	}
}

func TestValidateRejectsSegmentWithoutEquals(t *testing.T) {
	_, err := Validate("a=1; notacookie")
	if err == nil {
		t.Fatal("Validate should reject a segment without =")
	}
	if !strings.Contains(err.Error(), "notacookie") {
		t.Fatalf("error should name the bad segment: %v", err)
	}
}

func TestValidateRejectsEmptyName(t *testing.T) {
	if _, err := Validate("a=1; =value"); err == nil {
		t.Fatal("Validate should reject an empty cookie name")
	}
}
