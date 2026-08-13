//go:build windows
// +build windows

// GDI plumbing shared by card.go: raw syscalls against gdi32.dll (drawing
// primitives, fonts) alongside the user32/shell32 bindings already declared
// in main.go. Split into its own file purely to keep main.go (tray icon +
// message loop) and card.go (the popup card's own logic) from being
// crowded out by proc-binding boilerplate.
//
// Font choice: the card renders with the system "Segoe UI" family rather
// than an embedded webfont. An earlier revision embedded a converted Outfit
// TTF via AddFontMemResourceEx + a custom per-weight family name, but with
// no local Windows/GDI available to this repo's build environment to
// actually verify it rendered (see the root README's build notes), that
// path shipped a real, silently-wrong result: GDI's font mapper fell back
// to a default UI font with no error at any layer. Segoe UI is guaranteed
// present on every supported Windows version and is exactly what a native
// Win32 popup should look like anyway — trading a small brand-consistency
// gap for a card that reliably renders as designed.
package main

import (
	"syscall"
	"unsafe"
)

var (
	modgdi32 = syscall.NewLazyDLL("gdi32.dll")

	procCreateSolidBrush    = modgdi32.NewProc("CreateSolidBrush")
	procCreatePen           = modgdi32.NewProc("CreatePen")
	procSelectObject        = modgdi32.NewProc("SelectObject")
	procDeleteObject        = modgdi32.NewProc("DeleteObject")
	procRoundRect           = modgdi32.NewProc("RoundRect")
	procSetTextColor        = modgdi32.NewProc("SetTextColor")
	procSetBkMode           = modgdi32.NewProc("SetBkMode")
	procCreateFontIndirectW    = modgdi32.NewProc("CreateFontIndirectW")
	procCreateRoundRectRgn     = modgdi32.NewProc("CreateRoundRectRgn")
	procGetStockObject         = modgdi32.NewProc("GetStockObject")
	procGetTextExtentPoint32W  = modgdi32.NewProc("GetTextExtentPoint32W")

	procFillRect     = moduser32.NewProc("FillRect")
	procDrawTextW    = moduser32.NewProc("DrawTextW")
	procSetWindowRgn = moduser32.NewProc("SetWindowRgn")
	procGetDC        = moduser32.NewProc("GetDC")
	procReleaseDC    = moduser32.NewProc("ReleaseDC")
)

// GetStockObject indices (wingdi.h) — NULL_BRUSH and NULL_PEN are easy to
// mix up (5 vs 8) and GDI happily accepts either handle in either slot
// without complaint, so a swap here fails silently as a hollow shape with
// no error anywhere: SelectObject identifies an object by its own internal
// type tag, not by which "slot" the caller thinks they're filling, so
// passing a brush handle where a pen was intended just re-selects the
// current brush again and leaves the default BLACK_PEN outline in place.
const (
	stockNullBrush = 5
	stockNullPen   = 8
)

const (
	transparentBkMode = 1
	psSolid           = 0
	dtLeft            = 0x00000000
	dtCenter          = 0x00000001
	dtRight           = 0x00000002
	dtVCenter         = 0x00000004
	dtSingleLine      = 0x00000020
	dtEndEllipsis     = 0x00008000
	dtWordBreak       = 0x00000010
	fwLight           = 300 // matches the BlueSky design system's StatusPill (font-light)
	fwRegular         = 400
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

// createFont builds an HFONT for the given family/pixel-height/weight
// (negative lfHeight = exact character pixel height, avoiding any
// point-size/DPI conversion — the caller is expected to have already scaled
// pxHeight by the card window's own DPI factor). weight is a standard
// FW_* value (400/600/700/...); Segoe UI ships all of these as proper
// weight variants of one family, so this is what actually produces bold/
// semibold text rather than a synthesized-or-substituted font.
func createFont(family string, pxHeight int32, weight int32) uintptr {
	var lf logFontW
	lf.lfHeight = -pxHeight
	lf.lfWeight = weight
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

// fillRectColor fills `r` (already positioned in window-client coordinates)
// with a solid color — used for divider lines and the risk-score bar.
func fillRectColor(hdc uintptr, r *winRect, color uintptr) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	if brush == 0 {
		return
	}
	defer procDeleteObject.Call(brush)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(r)), brush)
}

// roundRectFill draws a filled rounded rectangle with no visible outline
// (a NULL_PEN selected as the current pen, so RoundRect's stroke is
// invisible and only the brush fill shows) — used for pill badges and the
// card's own rounded background.
func roundRectFill(hdc uintptr, r *winRect, radius int32, color uintptr) {
	brush, _, _ := procCreateSolidBrush.Call(color)
	if brush == 0 {
		return
	}
	defer procDeleteObject.Call(brush)
	nullPen, _, _ := procGetStockObject.Call(uintptr(stockNullPen))
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, nullPen)
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
}

// roundRectStroke draws a rounded-rectangle outline only (a NULL_BRUSH
// selected as the current brush, so RoundRect's fill is invisible and only
// the colored pen stroke shows) — used for outline-style pills and the
// card's brand-colored outer border.
func roundRectStroke(hdc uintptr, r *winRect, radius int32, penColor uintptr, width int32) {
	pen, _, _ := procCreatePen.Call(uintptr(psSolid), uintptr(width), penColor)
	if pen == 0 {
		return
	}
	defer procDeleteObject.Call(pen)
	nullBrush, _, _ := procGetStockObject.Call(uintptr(stockNullBrush))
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	oldBrush, _, _ := procSelectObject.Call(hdc, nullBrush)
	procRoundRect.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(r.right), uintptr(r.bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldPen)
	procSelectObject.Call(hdc, oldBrush)
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

// sizeT mirrors the Win32 SIZE struct — GetTextExtentPoint32W's output
// parameter.
type sizeT struct{ cx, cy int32 }

// measureTextWidthPx returns the pixel width `text` would occupy if drawn
// with `font`, using GetTextExtentPoint32W against `hdc` (the caller's
// responsibility to obtain and release — see measureCardScreenDC in
// card.go). This is the same glyph-metrics math DrawText itself uses
// internally, so a width computed here and then used to size the window
// beforehand guarantees DrawText/DT_END_ELLIPSIS never actually needs to
// truncate anything: the card was already sized to fit. Restores hdc's
// previously-selected font before returning, since callers may reuse the
// same DC to measure several strings in different fonts back to back.
func measureTextWidthPx(hdc uintptr, font uintptr, text string) int32 {
	old, _, _ := procSelectObject.Call(hdc, font)
	defer procSelectObject.Call(hdc, old)
	u, err := syscall.UTF16FromString(text)
	if err != nil {
		return 0
	}
	var size sizeT
	procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&size)))
	return size.cx
}
