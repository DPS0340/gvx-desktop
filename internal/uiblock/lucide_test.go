package uiblock_test

import (
	"slices"
	"testing"

	"github.com/ironpark/ggui/icons/lucide/all"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

func TestLucideIconNamesMatchAll(t *testing.T) {
	got := uiblock.LucideIconNames()
	want := all.Names()
	if !slices.Equal(got, want) {
		t.Fatalf("LucideIconNames() (len=%d) does not match ggui/icons/lucide/all.Names() (len=%d)", len(got), len(want))
	}
}
