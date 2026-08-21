//go:build windows
// +build windows

// DWM Acrylic/Mica system-backdrop + legacy Accent Policy blur-behind
// bindings for the tray card window (card.go) — the second attempt at
// matching the macOS menu bar app's translucent NSVisualEffectView panel
// (see card.go's wsExLayered doc comment for what went wrong the first
// time: DwmExtendFrameIntoClientArea's classic Aero-Glass chroma-key
// transparency, where anti-aliased GDI edges blending toward the RGB(0,0,0)
// "see-through" sentinel bled the blur into opaque text/icons).
//
// This attempt sidesteps that failure mode entirely by not chroma-keying
// anything: the card window is WS_EX_LAYERED and presents a fully
// self-composited 32bpp premultiplied-alpha bitmap via UpdateLayeredWindow
// (see layeredpaint_windows.go) — every pixel's translucency is decided by
// this app, per-pixel, from real alpha values, not by which RGB value GDI
// happened to leave behind. The calls in this file are a separate,
// complementary concern: they tell DWM to blur *whatever's on the desktop
// behind* the window, so the translucent/see-through portions of our own
// bitmap reveal a blurred backdrop rather than a sharp one. Two paths,
// tried in order:
//
//  1. Native Windows 11 22H2+ system backdrop (DWMWA_SYSTEMBACKDROP_TYPE =
//     DWMSBT_TRANSIENTWINDOW, the "Acrylic" material) — the modern,
//     documented API.
//  2. Windows 10 1809+ fallback via the undocumented but widely-relied-on
//     SetWindowCompositionAttribute Accent Policy (ACCENT_ENABLE_BLURBEHIND)
//     — no tint color needed here since our own layered bitmap
//     (cardBackgroundAlpha, layeredpaint_windows.go) already supplies the
//     tint; this call's only job is the blur.
//
// Every proc is resolved defensively via LazyProc.Find() before Call() —
// unlike this repo's one existing precedent for a version-gated API
// (main.go's procSetProcessDpiAwarenessContext.Call(), which skips that
// check and would panic on an OS old enough to lack it; not a pattern to
// repeat for brand-new, less-universally-available APIs like these). On
// anything that has none of these APIs, enableBlurBackdrop is a silent
// no-op and the card still renders correctly — just against an unblurred
// desktop — via layeredpaint_windows.go's own alpha compositing, which
// doesn't depend on any of this succeeding.
package main

import (
	"syscall"
	"unsafe"
)

var (
	moddwmapi = syscall.NewLazyDLL("dwmapi.dll")

	procDwmSetWindowAttribute         = moddwmapi.NewProc("DwmSetWindowAttribute")
	procDwmExtendFrameIntoClientArea  = moddwmapi.NewProc("DwmExtendFrameIntoClientArea")
	procSetWindowCompositionAttribute = moduser32.NewProc("SetWindowCompositionAttribute")
)

const (
	dwmwaUseImmersiveDarkMode = 20
	dwmwaSystemBackdropType   = 38

	dwmsbtTransientWindow = 3 // "Acrylic" material

	wcaAccentPolicy        = 19
	accentEnableBlurBehind = 3
)

// dwmMargins mirrors the Win32 MARGINS struct (four LONGs, left/right/top/
// bottom in that order) — DwmExtendFrameIntoClientArea's only parameter
// besides the window handle.
type dwmMargins struct {
	left, right, top, bottom int32
}

// accentPolicy mirrors the undocumented ACCENT_POLICY struct
// SetWindowCompositionAttribute expects when Attribute == WCA_ACCENT_POLICY.
type accentPolicy struct {
	accentState   uint32
	accentFlags   uint32
	gradientColor uint32
	animationID   uint32
}

// windowCompositionAttributeData mirrors the undocumented
// WINDOWCOMPOSITIONATTRIBDATA struct.
type windowCompositionAttributeData struct {
	attribute  uint32
	data       unsafe.Pointer
	sizeOfData uintptr
}

// enableBlurBackdrop asks DWM to blur whatever's on the desktop behind hwnd
// — see this file's doc comment for why that's a separate, safe-by-design
// concern from this app's own per-pixel alpha content
// (layeredpaint_windows.go). Best-effort throughout: every step
// independently no-ops on an OS that doesn't support it, and none of it
// affects whether the card is visible or legible.
func enableBlurBackdrop(hwnd uintptr) {
	// DwmExtendFrameIntoClientArea with all-negative margins is the documented
	// prerequisite for DWMWA_SYSTEMBACKDROP_TYPE to actually paint into the
	// *client* area rather than just a non-client frame this borderless
	// WS_POPUP doesn't have — a step several public "Mica/Acrylic in raw
	// Win32" write-ups call out as the easy-to-miss reason
	// DWMWA_SYSTEMBACKDROP_TYPE alone silently does nothing.
	if procDwmExtendFrameIntoClientArea.Find() == nil {
		m := dwmMargins{-1, -1, -1, -1}
		procDwmExtendFrameIntoClientArea.Call(hwnd, uintptr(unsafe.Pointer(&m)))
	}

	if procDwmSetWindowAttribute.Find() == nil {
		darkMode := uint32(1)
		if cardIsLight {
			darkMode = 0
		}
		procDwmSetWindowAttribute.Call(hwnd, uintptr(dwmwaUseImmersiveDarkMode), uintptr(unsafe.Pointer(&darkMode)), unsafe.Sizeof(darkMode))

		backdrop := uint32(dwmsbtTransientWindow)
		ret, _, _ := procDwmSetWindowAttribute.Call(hwnd, uintptr(dwmwaSystemBackdropType), uintptr(unsafe.Pointer(&backdrop)), unsafe.Sizeof(backdrop))
		if ret == 0 { // S_OK — native backdrop accepted, skip the legacy fallback below
			return
		}
	}

	// Windows 10 1809–21H2 fallback: no native system backdrop type, so ask
	// for the older Accent Policy blur-behind instead. No GradientColor tint
	// here (accentFlags=0, gradientColor=0) — the card's own layered bitmap
	// already carries its background tint at the alpha this app chose
	// (cardBackgroundAlpha, layeredpaint_windows.go); all this call needs to
	// contribute is the blur itself.
	if procSetWindowCompositionAttribute.Find() == nil {
		policy := accentPolicy{accentState: accentEnableBlurBehind}
		data := windowCompositionAttributeData{
			attribute:  wcaAccentPolicy,
			data:       unsafe.Pointer(&policy),
			sizeOfData: unsafe.Sizeof(policy),
		}
		procSetWindowCompositionAttribute.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	}
}
