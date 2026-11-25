package main

import (
	"image/color"
	"testing"
)

func TestTrayIconConfigForOS(t *testing.T) {
	winImg, winTemplate := trayIconConfigForOS("windows")
	if winTemplate {
		t.Fatal("Windows tray icon must have Template=false for full color")
	}
	if winImg == nil {
		t.Fatal("Windows tray icon image is nil")
	}
	b := winImg.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("Windows tray icon bounds = %v, want non-empty", b)
	}
	colored := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(winImg.At(x, y)).(color.NRGBA)
			if c.A > 0 && (c.R != c.G || c.G != c.B) {
				colored++
			}
		}
	}
	if colored == 0 {
		t.Fatal("Windows tray icon has no chromatic pixels; expected full-color mark")
	}

	macImg, macTemplate := trayIconConfigForOS("darwin")
	if !macTemplate {
		t.Fatal("macOS tray icon must have Template=true")
	}
	if macImg == nil {
		t.Fatal("macOS tray icon image is nil")
	}
	mb := macImg.Bounds()
	if mb.Dx() != 36 || mb.Dy() != 36 {
		t.Fatalf("macOS tray icon bounds = %v, want 36x36", mb)
	}
	opaque := 0
	for y := mb.Min.Y; y < mb.Max.Y; y++ {
		for x := mb.Min.X; x < mb.Max.X; x++ {
			c := color.NRGBAModel.Convert(macImg.At(x, y)).(color.NRGBA)
			if c.R != 0 || c.G != 0 || c.B != 0 {
				t.Fatalf("macOS template icon pixel (%d,%d) has RGB (%d,%d,%d), want alpha-only", x, y, c.R, c.G, c.B)
			}
			if c.A > 0 {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Fatal("macOS template icon is completely transparent")
	}
}
