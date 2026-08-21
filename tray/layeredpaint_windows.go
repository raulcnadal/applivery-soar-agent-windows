//go:build windows
// +build windows

// Per-pixel-alpha compositing for the tray card window — the piece that
// makes card.go's plain GDI drawing (FillRect/RoundRect/DrawTextW/DrawIconEx/
// StretchBlt, none of which touch an alpha channel) safe to present through
// a WS_EX_LAYERED window's UpdateLayeredWindow, which requires one.
//
// card.go's own drawing code (paintCardBackground/paintCardForeground) is
// untouched by this file — every draw call still lands exactly where it
// always has (FillRect still fills, DrawTextW still draws ClearType
// anti-aliased glyphs, and so on). What changes is *where* those calls draw
// (an offscreen 32bpp DIB instead of the window's own paint DC) and what
// happens to that DIB afterward, all orchestrated by paintAndPresentCard:
//
//  1. Start the buffer fully transparent (every byte, including alpha, zero).
//  2. Call paintCardBackground(memDC) — just the one roundRectFill that
//     used to open the old combined paintCard — then snapshot the buffer's
//     raw bytes (`afterBackground`). No alpha/premultiplication happens
//     yet — deliberately deferred to step 4, below.
//  3. Call paintCardForeground(memDC) — everything else the card has always
//     drawn: icons, the banner bitmap, pill fills/borders/text, plain text,
//     the outer brand-blue border stroke. Unchanged code, drawn straight on
//     top of the *unmodified* background from step 2 in the same memory DC
//     — critically, GDI's own ClearType anti-aliasing blends every text
//     edge against that same bright, undarkened background color exactly
//     as it always has, byte-for-byte identical to today's fully-opaque,
//     unblurred card. (An earlier draft of this file premultiplied the
//     background's RGB *before* this step, which meant text anti-aliasing
//     ended up blending against an already-darkened color instead — mostly
//     invisible in dark mode, where colGray900 barely changes, but a
//     visible gray fringe risk in light mode, where colWhite's premultiplied
//     value is a noticeably darker gray. Deferring all premultiplication to
//     step 4 avoids that class of bug entirely, rather than tuning around it.)
//  4. finalizeAlpha does one single pass comparing the now-fully-painted
//     buffer against the step-2 snapshot, per pixel: identical RGB and
//     still-zero means genuinely untouched (stays alpha 0 — the four true
//     rounded corners RoundRect's fill never reaches, which is also what
//     makes SetWindowRgn's separate rounded-corner clip in card.go
//     redundant-but-harmless belt-and-suspenders rather than load-bearing);
//     identical RGB but non-zero means background-only, so it gets
//     premultiplied by cardBackgroundAlpha *now*, for the first time, and
//     tagged with that alpha; and any difference at all from the
//     snapshot — including a faint ClearType fringe a single shade off the
//     background color, not just a solid fill — means step 3 put real ink
//     there, so that pixel is forced fully opaque (alpha 255;
//     premultiplying by 255/255 is a no-op, so its RGB is left exactly as
//     GDI wrote it). This is what keeps text crisp and fully opaque right
//     down to its anti-aliased edges instead of the first attempt's washout
//     (see card.go's wsExLayered doc comment) — nothing here depends on
//     matching one specific "transparent" RGB value the way that reverted
//     RGB(0,0,0) chroma-key approach did, so there's no color
//     anti-aliasing can accidentally blend toward and partially erase.
//  5. presentLayeredCard hands the finished buffer to UpdateLayeredWindow.
//
// No local Windows/DWM build available in this repo's own tooling to run
// this against live — see ARCHITECTURE.md for what to check visually before
// this ships (light/dark mode, text edges, the banner bitmap's edges, and
// the pre-Windows-11 SetWindowCompositionAttribute fallback specifically,
// since that path can only be exercised on an actual Windows 10 machine).
package main

import "unsafe"

// cardBackgroundAlpha is the tint opacity applied to the card's own
// translucent background fill (0-255) — the RGB itself still comes from
// cardSurfaceColor (colWhite/colGray900), just not fully opaque, so the DWM
// blur set up in acrylic_windows.go shows through it. 0xCC (~80%) matches
// the opacity Windows' own Acrylic material recipe (Fluent Design's
// AcrylicBackgroundFillColorDefault) uses by default — a starting point,
// not a value measured against this specific card; adjust after a real
// visual pass.
const cardBackgroundAlpha = 0xCC

var (
	procCreateDIBSection    = modgdi32.NewProc("CreateDIBSection")
	procUpdateLayeredWindow = moduser32.NewProc("UpdateLayeredWindow")
)

const (
	dibRgbColors = 0
	biRGB        = 0

	ulwAlpha   = 0x00000002
	acSrcOver  = 0x00
	acSrcAlpha = 0x01
)

// bitmapInfoHeader mirrors BITMAPINFOHEADER — CreateDIBSection's format
// descriptor. biHeight negative selects a top-down DIB (row 0 = the DIB's
// first scanline in memory), matching every other coordinate this card
// already computes in top-left-origin, y-increases-downward client space —
// avoids a manual row-flip that a bottom-up (positive biHeight) DIB would
// otherwise need. No color-table fields (bmiColors) — unused and unread by
// CreateDIBSection for a 32bpp BI_RGB request.
type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// blendFunction mirrors BLENDFUNCTION — UpdateLayeredWindow's per-window
// blend-mode parameter. sourceConstantAlpha stays 255 (no additional
// whole-window fade layered on top of this app's own per-pixel alpha) with
// alphaFormat=AC_SRC_ALPHA so UpdateLayeredWindow actually reads and uses
// each pixel's own alpha byte instead of treating the source as fully
// opaque.
type blendFunction struct {
	blendOp             byte
	blendFlags          byte
	sourceConstantAlpha byte
	alphaFormat         byte
}

// createAlphaDIB allocates a top-down 32bpp BI_RGB DIB section sized w x h,
// selects it into a fresh memory DC, and returns a direct []byte view of its
// pixel buffer (4 bytes/pixel, B-G-R-A per Win32's documented 32bpp DIB
// memory layout — the same order GDI itself writes when it draws into this
// kind of buffer, so no channel-swapping is needed anywhere else in this
// file). The buffer starts fully zeroed (transparent black). Caller owns
// cleanup via deleteAlphaDIB.
func createAlphaDIB(w, h int32) (hdcMem uintptr, hBitmap uintptr, oldBitmap uintptr, pixels []byte, ok bool) {
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return 0, 0, 0, nil, false
	}
	defer procReleaseDC.Call(0, screenDC)

	hdcMem, _, _ = procCreateCompatibleDC.Call(screenDC)
	if hdcMem == 0 {
		return 0, 0, 0, nil, false
	}

	var bi bitmapInfoHeader
	bi.biSize = uint32(unsafe.Sizeof(bi))
	bi.biWidth = w
	bi.biHeight = -h // top-down
	bi.biPlanes = 1
	bi.biBitCount = 32
	bi.biCompression = biRGB

	var bits uintptr
	hBitmap, _, _ = procCreateDIBSection.Call(hdcMem, uintptr(unsafe.Pointer(&bi)), uintptr(dibRgbColors), uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hBitmap == 0 || bits == 0 {
		procDeleteDC.Call(hdcMem)
		return 0, 0, 0, nil, false
	}
	// Save the memory DC's original default bitmap (a 1x1 monochrome
	// placeholder CreateCompatibleDC always starts with) so it can be
	// selected back in before DeleteDC — the same select-old/delete-DC order
	// card.go's own drawKindBitmap case already uses for its own memory DC,
	// avoiding a "GDI object still selected into a DC being deleted" leak.
	oldBitmap, _, _ = procSelectObject.Call(hdcMem, hBitmap)

	n := int(w) * int(h) * 4
	pixels = unsafe.Slice((*byte)(unsafe.Pointer(bits)), n)
	for i := range pixels {
		pixels[i] = 0
	}
	return hdcMem, hBitmap, oldBitmap, pixels, true
}

func deleteAlphaDIB(hdcMem, hBitmap, oldBitmap uintptr) {
	if hdcMem != 0 && oldBitmap != 0 {
		procSelectObject.Call(hdcMem, oldBitmap)
	}
	if hBitmap != 0 {
		procDeleteObject.Call(hBitmap)
	}
	if hdcMem != 0 {
		procDeleteDC.Call(hdcMem)
	}
}

// premultiply scales one BGR channel byte by alpha/255, as
// UpdateLayeredWindow(ULW_ALPHA) requires for every non-fully-opaque pixel
// (its documented "premultiplied alpha" source format).
func premultiply(c, alpha byte) byte {
	return byte(uint32(c) * uint32(alpha) / 255)
}

// finalizeAlpha is step 4 of this file's doc comment — the single pass that
// computes every pixel's alpha, run once after both paintCardBackground and
// paintCardForeground have drawn. `afterBackground` is the raw, still
// non-premultiplied snapshot taken right after paintCardBackground (step 2);
// `pixels` is the same buffer now that paintCardForeground (step 3) has
// drawn on top of it. See this file's top doc comment for why deferring
// premultiplication to this single final pass — rather than premultiplying
// the background immediately after step 2, before step 3's text
// anti-aliasing ever sees it — matters.
func finalizeAlpha(pixels, afterBackground []byte) {
	n := len(pixels)
	if len(afterBackground) < n {
		n = len(afterBackground)
	}
	for i := 0; i+3 < n; i += 4 {
		b, g, r := pixels[i], pixels[i+1], pixels[i+2]
		if b == afterBackground[i] && g == afterBackground[i+1] && r == afterBackground[i+2] {
			if b == 0 && g == 0 && r == 0 {
				continue // never touched by either pass — fully transparent
			}
			// Background-only: tint to cardBackgroundAlpha now, for the
			// first time (see doc comment above for why not sooner).
			pixels[i] = premultiply(b, cardBackgroundAlpha)
			pixels[i+1] = premultiply(g, cardBackgroundAlpha)
			pixels[i+2] = premultiply(r, cardBackgroundAlpha)
			pixels[i+3] = cardBackgroundAlpha
			continue
		}
		// paintCardForeground put real ink here — however slightly it
		// differs from the background snapshot, including a faint
		// anti-aliasing fringe. Fully opaque; premultiplying by 255/255 is
		// a no-op, so BGR is left exactly as GDI wrote it.
		pixels[i+3] = 255
	}
}

// presentLayeredCard blits the finished, alpha-corrected buffer to hwnd via
// UpdateLayeredWindow — the only way a WS_EX_LAYERED top-level window's
// content is ever actually shown; a plain BeginPaint/EndPaint blit (what
// every other window in this repo relies on) is not sufficient once
// WS_EX_LAYERED is set.
func presentLayeredCard(hwnd, hdcMem uintptr, w, h int32) {
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC != 0 {
		defer procReleaseDC.Call(0, screenDC)
	}
	size := sizeT{cx: w, cy: h}
	srcPt := point{x: 0, y: 0}
	blend := blendFunction{blendOp: acSrcOver, sourceConstantAlpha: 255, alphaFormat: acSrcAlpha}
	procUpdateLayeredWindow.Call(
		hwnd, screenDC, 0, uintptr(unsafe.Pointer(&size)),
		hdcMem, uintptr(unsafe.Pointer(&srcPt)), 0,
		uintptr(unsafe.Pointer(&blend)), uintptr(ulwAlpha),
	)
}

// paintAndPresentCard runs the full sequence documented at the top of this
// file. Called explicitly, synchronously, from showCard (card.go) right
// after the window is created — a WS_EX_LAYERED window isn't guaranteed to
// receive an ordinary WM_PAINT the way the rest of this repo's windows do,
// so this can't be left to arrive asynchronously through cardWndProc's
// wmPaint case the way painting normally works here; that case still calls
// this too, purely as a defensive fallback for the rare case Windows does
// send one (e.g. after a DPI change), which is harmless to run twice since
// this card's content never changes after buildCardContent(). Falls back to
// leaving the window blank (rather than crashing) if the offscreen DIB
// can't be allocated at all, which would only happen under real GDI
// resource exhaustion.
func paintAndPresentCard(hwnd uintptr) {
	hdcMem, hBitmap, oldBitmap, pixels, ok := createAlphaDIB(cardWidthPx, cardHeight)
	if !ok {
		return
	}
	defer deleteAlphaDIB(hdcMem, hBitmap, oldBitmap)

	paintCardBackground(hdcMem)
	afterBackground := make([]byte, len(pixels))
	copy(afterBackground, pixels)

	paintCardForeground(hdcMem)
	finalizeAlpha(pixels, afterBackground)

	presentLayeredCard(hwnd, hdcMem, cardWidthPx, cardHeight)
}
