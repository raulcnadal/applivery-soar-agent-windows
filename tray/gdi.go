//go:build windows
// +build windows

// GDI plumbing shared by card.go: raw syscalls against gdi32.dll (drawing
// primitives, fonts) alongside the user32/shell32 bindings already declared
// in main.go. Split into its own file purely to keep main.go (tray icon +
// message loop) and card.go (the popup card's own logic) from being
// crowded out by proc-binding boilerplate.
package main

import (
	"embed"
	"log"
	"syscall"
	"unsafe"
)

//go:embed fonts/Outfit-Regular.ttf fonts/Outfit-Medium.ttf fonts/Outfit-SemiBold.ttf fonts/Outfit-Bold.ttf
var fontFS embed.FS

// Font family names baked into the embedded TTFs' own name tables (see the
// Windows agent repo's font-generation notes — the @fontsource-derived
// source files had ambiguous/incorrect family names from their variable-font
// origin, so each weight was re-tagged as its own fully distinct family
// name before embedding here, specifically so Windows' style-linking can
// never substitute the wrong weight or fall back to a system font on a
// near-miss name).
const (
	fontFamilyRegular  = "SOAR Outfit Regular"
	fontFamilyMedium   = "SOAR Outfit Medium"
	fontFamilySemiBold = "SOAR Outfit SemiBold"
	fontFamilyBold     = "SOAR Outfit Bold"
)

var (
	modgdi32 = syscall.NewLazyDLL("gdi32.dll")

	procCreateCompatibleDC   = modgdi32.NewProc("CreateCompatibleDC")
	procCreateSolidBrush     = modgdi32.NewProc("CreateSolidBrush")
	procCreatePen            = modgdi32.NewProc("CreatePen")
	procSelectObject         = modgdi32.NewProc("SelectObject")
	procDeleteObject         = modgdi32.NewProc("DeleteObject")
	procDeleteDC             = modgdi32.NewProc("DeleteDC")
	procRoundRect            = modgdi32.NewProc("RoundRect")
	procRectangleGdi         = modgdi32.NewProc("Rectangle")
	procSetTextColor         = modgdi32.NewProc("SetTextColor")
	procSetBkMode            = modgdi32.NewProc("SetBkMode")
	procCreateFontIndirectW  = modgdi32.NewProc("CreateFontIndirectW")
	procCreateRoundRectRgn   = modgdi32.NewProc("CreateRoundRectRgn")
	procAddFontMemResourceEx = modgdi32.NewProc("AddFontMemResourceEx")
	procGetStockObject       = modgdi32.NewProc("GetStockObject")

	procFillRect  = moduser32.NewProc("FillRect")
	procDrawTextW = moduser32.NewProc("DrawTextW")
	procSetWindowRgn = moduser32.NewProc("SetWindowRgn")
)

const (
	transparentBkMode = 1
	nullBrush         = 5 // GetStockObject index
	psSolid           = 0
	dtLeft            = 0x00000000
	dtCenter          = 0x00000001
	dtRight           = 0x00000002
	dtVCenter         = 0x00000004
	dtSingleLine      = 0x00000020
	dtEndEllipsis     = 0x00008000
	dtWordBreak       = 0x00000010
	fwRegular         = 400
	fwMedium          = 500
	fwSemiBold        = 600
	fwBold            = 700
	defaultCharset    = 1
	outTTPrecis       = 4
	clipDefaultPrecis = 0
	clearTypeQuality  = 5
	defaultPitch      = 0
	ffDontCare        = 0
)

// logFontW mirrors LOGFONTW exactly (32-char face name buffer, standard
// Win32 GDI struct — same field order/types used by every CreateFontIndirectW
// caller).
type logFontW struct {
	lfHeight         int32
	lfWidth          int32
	lfEscapement     int32
	lfOrientation    int32
	lfWeight         int32
	lfItalic         byte
	lfUnderline      byte
	lfStrikeOut      byte
	lfCharSet        byte
	lfOutPrecision   byte
	lfClipPrecision  byte
	lfQuality        byte
	lfPitchAndFamily byte
	lfFaceName       [32]uint16
}

// colorref packs an (r,g,b) triple into the 0x00BBGGRR layout every GDI
// color parameter (COLORREF) expects — the one detail that's easy to get
// backwards (RGB, not BGR) when hand-rolling these calls.
func colorref(r, g, b byte) uintptr {
	return uintptr(uint32(r) | uint32(g)<<8 | uint32(b)<<16)
}

// loadEmbeddedFonts registers all four embedded Outfit weights as
// process-private fonts (AddFontMemResourceEx — never touches the system
// font table, and Windows automatically releases them when this process
// exits). Best-effort: a failure just means createFont() below falls back
// to whatever GDI substitutes for an unresolved face name (typically the
// system default UI font) — a worse-looking card, not a crash.
func loadEmbeddedFonts() {
	files := []string{"fonts/Outfit-Regular.ttf", "fonts/Outfit-Medium.ttf", "fonts/Outfit-SemiBold.ttf", "fonts/Outfit-Bold.ttf"}
	for _, f := range files {
		data, err := fontFS.ReadFile(f)
		if err != nil {
			log.Printf("Could not read embedded font %s: %v", f, err)
			continue
		}
		var numFonts uint32
		ret, _, callErr := procAddFontMemResourceEx.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0, uintptr(unsafe.Pointer(&numFonts)))
		if ret == 0 {
			log.Printf("AddFontMemResourceEx failed for %s: %v", f, callErr)
		}
	}
}

// createFont builds an HFONT for one of the four embedded families at a
// given pixel height (negative lfHeight = exact character pixel height,
// avoiding any point-size/DPI conversion — the caller is expected to have
// already scaled pxHeight by the card window's own DPI factor).
func createFont(family string, pxHeight int32) uintptr {
	var lf logFontW
	lf.lfHeight = -pxHeight
	lf.lfWeight = fwRegular
	lf.lfCharSet = defaultCharset
	lf.lfOutPrecision = outTTPrecis
	lf.lfClipPrecision = clipDefaultPrecis
	lf.lfQuality = clearTypeQuality
	lf.lfPitchAndFamily = defaultPitch | ffDontCare
	u, err := syscall.UTF16FromString(family)
	if err == nil {
		n := len(u)
		if n > len(lf.lfFaceName) {
			n = len(lf.lfFaceName)
		}
		copy(lf.lfFaceName[:n], u[:n])
	}
	h, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	return h
}

// fillRect fills `r` (a rect already positioned in window-client
// coordinates) with a solid color — used for both KV-row backgrounds/pills
// (via roundRect, see card.go) and 1px divider lines.
func fillRectColor(hdc uintptr, r *winRect, color uintptr) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	if brush == 0 {
		return
	}
	defer procDeleteObject.Call(brush)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(r)), brush)
}

// roundRectFill draws a filled rounded rectangle (pen-less — NULL_BRUSH's
// sibling stock object for pens keeps the outline from being drawn at all,
// so only the fill shows) with the given corner radius, used for pill
// badges and the card's own rounded background.
func roundRectFill(hdc uintptr, r *winRect, radius int32, color uintptr) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	if brush == 0 {
		return
	}
	defer procDeleteObject.Call(brush)
	nullPen, _, _ := procGetStockObject.Call(uintptr(5)) // NULL_PEN = 5
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, nullPen)
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
}

// drawText draws `s` inside `r` with the given DT_* flags, using whatever
// font/text color is currently selected into hdc (callers select those via
// SelectObject/SetTextColor before calling this).
func drawText(hdc uintptr, s string, r *winRect, flags uintptr) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(r)), flags)
}
