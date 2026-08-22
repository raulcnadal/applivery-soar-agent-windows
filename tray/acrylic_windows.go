//go:build windows
// +build windows

// Legacy Accent Policy blur-behind binding for the tray card window
// (card.go) — the second attempt at
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
// happened to leave behind. The one call left in this file asks DWM to
// blur *whatever's on the desktop behind* the window, so the translucent/
// see-through portions of our own bitmap reveal a blurred backdrop rather
// than a sharp one.
//
// THIRD-ATTEMPT FIX (real-device test on both AMD64 and ARM64 showed the
// card fully opaque and its background visibly greyer at cardBackgroundAlpha
// = 0x66 than at 0xCC — i.e. lowering alpha made the *flat, unblended*
// premultiplied color darker with zero corresponding increase in see-through
// desktop, which only makes sense if DWM was never actually alpha-blending
// this window against the desktop at all): this file previously ALSO called
// DwmExtendFrameIntoClientArea (all-negative margins, the "sheet of glass"
// trick) followed by DwmSetWindowAttribute(DWMWA_SYSTEMBACKDROP_TYPE) to
// request the native Windows 11 22H2+ Acrylic material. Both of those are
// designed for an ordinary DWM-composited window where DWM itself owns
// painting a backdrop behind content the app draws via normal
// WM_PAINT/BitBlt — they hand DWM a second, competing compositing pipeline
// for the exact same client area a WS_EX_LAYERED window's UpdateLayeredWindow
// call already owns outright. On the tested hardware DWM's frame-extension
// path won that fight: it composited our bitmap as an *opaque* surface
// (ignoring the alpha channel UpdateLayeredWindow supplied), which explains
// both symptoms — no visible transparency, and a background that gets
// visibly grayer/darker as cardBackgroundAlpha drops, since a lower alpha
// only changes the premultiplied RGB (darker) while contributing nothing to
// blending against the desktop.
//
// Removed that native-backdrop path entirely. What remains,
// SetWindowCompositionAttribute's Accent Policy, is the older Windows 10
// 1809+ mechanism, and — unlike the native backdrop API — it was purpose-
// built for exactly this combination (WS_EX_LAYERED + UpdateLayeredWindow):
// it doesn't ask DWM to own compositing the client area, only to blur the
// desktop pixels sitting behind the window, which the app's own per-pixel
// alpha then blends against untouched. This is the same technique long used
// by pre-Windows-11 raw-Win32 blur-behind implementations for exactly this
// reason.
//
// FOURTH-ATTEMPT REFINEMENT (pending real-device test): switched the accent
// state from ACCENT_ENABLE_BLURBEHIND (3, a plain Gaussian-ish blur) to
// ACCENT_ENABLE_ACRYLICBLURBEHIND (4) — the actual "Acrylic" material
// Windows 10/11 used natively in things like the Start Menu and Action
// Center for years: blur plus a subtle noise texture, closer in spirit to
// macOS's NSVisualEffectView vibrancy than plain blur-behind. Undocumented
// by Microsoft (unlike accent state 3, which at least has a stable-enough
// reputation from a decade of use), but well-established across several
// actively-maintained real-world tools (TranslucentTB and similar) that
// rely on the exact same reverse-engineered accentFlags/gradientColor
// contract used below — reliable in the "widely depended upon" sense, not
// in the "Microsoft-guaranteed-stable" sense, which is the honest trade-off
// of using it at all.
//
// gradientColor's byte layout is community-documented as 0xAABBGGRR — the
// same 0x00BBGGRR this repo's own colorref() already produces, just with an
// alpha byte prepended — so acrylicGradientColor below reuses colorref
// values directly rather than repacking RGB by hand. Alpha is set to 1
// (near-zero, not literally 0) deliberately: several reverse-engineering
// write-ups of this same undocumented API note that GradientColor with
// alpha=0 causes some Windows builds to skip rendering the Acrylic effect
// entirely (likely an internal "fully transparent overlay, skip it"
// shortcut) — 1 is the standard workaround, visually negligible but enough
// to signal "render it." accentFlags=2 is the other commonly-cited minimum
// needed for the tint/material to actually render reliably across builds;
// no border bits are set since the card already draws its own border stroke
// (paintCardForeground's outer roundRectStroke).
//
// This call intentionally does NOT try to make the OS-side gradientColor
// carry this card's real visible tint — that stays exactly as before,
// coming from this app's own per-pixel alpha compositing
// (cardBackgroundAlphaLight/Dark, layeredpaint_windows.go). Stacking two
// independent tints (the OS accent's and this app's own) would be much
// harder to reason about and tune than swapping only the one variable this
// change is actually testing: whether the Acrylic material's blur+noise
// texture alone looks meaningfully closer to "native" than plain
// blur-behind did.
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
	"unsafe"
)

var procSetWindowCompositionAttribute = moduser32.NewProc("SetWindowCompositionAttribute")

const (
	wcaAccentPolicy = 19

	accentEnableAcrylicBlurBehind = 4

	// acrylicAccentFlags — see this file's doc comment for why 2 (no border
	// bits set) rather than 0.
	acrylicAccentFlags = 2
	// acrylicGradientAlpha — see this file's doc comment for why 1, not 0.
	acrylicGradientAlpha = 1
)

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

// acrylicGradientColor packs an RGB value already in this repo's colorref()
// 0x00BBGGRR layout, plus an alpha byte, into the 0xAABBGGRR uint32
// SetWindowCompositionAttribute's GradientColor field expects — see this
// file's doc comment for the byte-layout source and why alpha stays
// negligible-but-nonzero here rather than carrying the card's real tint.
func acrylicGradientColor(rgb uintptr, alpha byte) uint32 {
	return uint32(alpha)<<24 | uint32(rgb&0x00FFFFFF)
}

// enableBlurBackdrop asks DWM to blur whatever's on the desktop behind hwnd
// — see this file's doc comment for why that's a separate, safe-by-design
// concern from this app's own per-pixel alpha content
// (layeredpaint_windows.go). Best-effort throughout: every step
// independently no-ops on an OS that doesn't support it, and none of it
// affects whether the card is visible or legible.
func enableBlurBackdrop(hwnd uintptr) {
	// Accent Policy Acrylic blur-behind — see this file's doc comment for
	// why ACCENT_ENABLE_ACRYLICBLURBEHIND over plain ACCENT_ENABLE_BLURBEHIND,
	// and why GradientColor's alpha is 1 rather than 0 or a real tint value.
	// The card's own layered bitmap still carries the actual visible
	// background tint at the alpha this app chose (cardBackgroundAlphaLight/
	// Dark, layeredpaint_windows.go); this call's job is only the
	// blur+noise material underneath it.
	if procSetWindowCompositionAttribute.Find() == nil {
		policy := accentPolicy{
			accentState:   accentEnableAcrylicBlurBehind,
			accentFlags:   acrylicAccentFlags,
			gradientColor: acrylicGradientColor(cardSurfaceColor(cardIsLight), acrylicGradientAlpha),
		}
		data := windowCompositionAttributeData{
			attribute:  wcaAccentPolicy,
			data:       unsafe.Pointer(&policy),
			sizeOfData: unsafe.Sizeof(policy),
		}
		procSetWindowCompositionAttribute.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	}
}
