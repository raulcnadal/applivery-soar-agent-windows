//go:build windows
// +build windows

// The status card shown when a user clicks (or right-clicks — Explorer
// treats both as "activate" for a notification-area icon) the tray icon.
// Replaces what used to be a native Win32 popup menu with a small,
// custom-painted, borderless window laid out to match the SOAR web app's
// own Workspace Profile modal styling (bold name, muted slug below, a pair
// of pills, bordered brand-blue outline, boxed sections) so the native tray
// experience reads as the same product as the dashboard — banner logo top
// left, device name/workspace slug stacked underneath, rather than a
// centered text title.
//
// Mostly informational/troubleshooting — there is still no "open dashboard"
// link (see main.go's doc comment for why: this agent runs on end-user
// devices, not admin machines) — but not purely read-only any more: the
// "Force report"/"Force evaluate compliance" buttons let the person sitting
// at this device kick off an out-of-cycle report/evaluation without waiting
// for the next scheduled tick. Neither button calls the backend from this
// process directly (see triggerForceReport/triggerForceEvaluate, main.go) —
// they just signal the main service, which is the one that's actually
// authenticated.
//
// Built as a second borderless top-level window (WS_POPUP) rather than a
// dialog resource or any GUI toolkit, painted with the same raw GDI calls
// declared in gdi.go — kept content as a flat []drawItem list built once in
// buildCardContent() and replayed on every WM_PAINT, so the paint handler
// itself stays trivial and layout logic lives in one place. All pill/label
// widths below are fixed rather than text-measured: the card has no HDC
// available at layout time (that only exists during WM_PAINT), and every
// string that actually varies in length (subtitle, policy names) is drawn
// with DT_END_ELLIPSIS so Windows does the real, pixel-accurate truncation
// itself rather than a hand-rolled character-count guess.
package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

const (
	wmPaint       = 0x000F
	wmClose       = 0x0010
	wmLButtonDown = 0x0201
	wmKeyDown     = 0x0100
	wmActivate    = 0x0006
	wmNcHitTest   = 0x0084

	vkEscape = 0x1B

	// WM_NCHITTEST return codes relevant here: HTCLIENT (the default, "this
	// is ordinary client area") and HTCAPTION ("treat this like a title bar
	// for dragging purposes"). Returning HTCAPTION from a spot that isn't a
	// real caption is the standard, well-known technique borderless/custom-
	// chrome apps use to make an arbitrary region drag-movable — DefWindowProc
	// starts its normal window-move loop off the back of it exactly as if the
	// window had a real WS_CAPTION, with zero extra plumbing needed on our
	// side (no manual mouse-capture/WM_MOUSEMOVE tracking, no owner-drawn
	// caption button hit-testing beyond the exclusion below).
	htClient  = 1
	htCaption = 2

	wsPopup        = 0x80000000
	wsExToolWindow = 0x00000080
	wsExTopMost    = 0x00000008

	smCxScreen = 0
	smCyScreen = 1

	swShow = 5

	waInactive = 0

	diNormal = 0x0003 // DrawIconEx: draw both mask and image

	// imageBitmap/lrCreateDibSection back loadBannerBitmap's LoadImageW call
	// — LR_CREATEDIBSECTION asks Windows to hand back a DIB section (rather
	// than a device-dependent bitmap) so the loaded handle is safe to select
	// into any memory DC regardless of the current display's color depth.
	imageBitmap        = 0
	lrCreateDibSection = 0x00002000
	srcCopy            = 0x00CC0020 // BitBlt/StretchBlt ROP: straight copy, no masking
	halftoneStretchMode = 4         // STRETCH_HALFTONE — best downscale quality of the four GDI stretch modes

	// bannerRasterW/bannerRasterH are the exact pixel dimensions the two
	// embedded banner_*.bmp assets were rasterized at (see tray/icons —
	// generated from applivery-bp-login.svg via cairosvg at 1.5x its native
	// 673x64 viewBox, ~4x supersampled relative to the card's actual display
	// size so StretchBlt's HALFTONE downscale stays crisp at every DPI this
	// card already scales for). Hardcoded rather than queried via
	// GetObjectW/BITMAP at load time — this repo controls both assets
	// exactly, and skipping that call is one fewer new GDI struct to get
	// wrong with no local Windows build to verify it against.
	bannerRasterW = 1010
	bannerRasterH = 96

	cardClassName     = "ApplivierySOARCardWndClass"
	cardCornerRadius  = 16
	cardPadX          = 24
	cardPadY          = 20
	cardRowH          = 28
	cardMaxPolicyRows = 6
	cardMinWidth      = 440
)

var (
	procShowWindow      = moduser32.NewProc("ShowWindow")
	procBeginPaint      = moduser32.NewProc("BeginPaint")
	procEndPaint        = moduser32.NewProc("EndPaint")
	procGetDpiForSystem = moduser32.NewProc("GetDpiForSystem")
	procDrawIconEx      = moduser32.NewProc("DrawIconEx")
	procPostMessageW    = moduser32.NewProc("PostMessageW")
	procScreenToClient  = moduser32.NewProc("ScreenToClient")

	// Banner bitmap plumbing (loadBannerBitmap/paintCard's drawKindBitmap
	// case) — gdi.go doesn't declare these since nothing else in this repo
	// draws a bitmap; CreateCompatibleDC/StretchBlt/SetStretchBltMode/DeleteDC
	// are the standard "select a bitmap into a memory DC, blit from it"
	// sequence every Win32 image-drawing example uses.
	procCreateCompatibleDC = modgdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = modgdi32.NewProc("DeleteDC")
	procStretchBlt         = modgdi32.NewProc("StretchBlt")
	procSetStretchBltMode  = modgdi32.NewProc("SetStretchBltMode")
)

// Color palette — matches frontend/src/assets/styles/bluesky-tokens.css
// (--color-brand-600) and the app-wide severity/status colors used
// throughout the SOAR web app (grepped from the frontend's Tailwind
// classes/components rather than guessed, so the card reads as the same
// product as the dashboard).
var (
	colBrand   = colorref(2, 65, 227)   // #0241E3 — brand-600
	colSuccess = colorref(34, 197, 94)  // #22C55E — compliant/OK
	colDanger  = colorref(239, 68, 68)  // #EF4444 — violation/high severity
	colWarning = colorref(245, 158, 11) // #F59E0B — medium severity
	colCrit    = colorref(185, 28, 28)  // #B91C1C — critical severity
	colLow     = colorref(100, 116, 139)
	colWhite   = colorref(255, 255, 255)
	colGray900 = colorref(17, 24, 39) // dark-theme surface
	colGray200 = colorref(229, 231, 235)
	colGray700 = colorref(55, 65, 81)
	colGray400 = colorref(156, 163, 175) // muted text, both themes
)

func cardSurfaceColor(light bool) uintptr {
	if light {
		return colWhite
	}
	return colGray900
}

func cardBorderColor(light bool) uintptr {
	if light {
		return colGray200
	}
	return colGray700
}

func cardPrimaryTextColor(light bool) uintptr {
	if light {
		return colGray900
	}
	return colWhite
}

func cardMutedTextColor() uintptr {
	return colGray400
}

func tierColor(tier string) uintptr {
	switch strings.ToLower(tier) {
	case "critical":
		return colCrit
	case "high":
		return colDanger
	case "medium":
		return colWarning
	case "low":
		return colLow
	default:
		return colGray400
	}
}

type drawKind int

const (
	drawKindText drawKind = iota
	drawKindPill
	drawKindFill // solid rect fill — dividers, section dots, the risk bar
	drawKindIcon
	drawKindBitmap // the header banner logo — see loadBannerBitmap
)

type drawItem struct {
	kind         drawKind
	rect         winRect
	text         string
	font         uintptr
	color        uintptr
	bgColor      uintptr
	align        uintptr
	radius       int32
	outline      bool
	iconHandle   uintptr
	bitmapHandle uintptr
}

var (
	cardHwnd            uintptr
	cardClassRegistered bool
	cardIsLight         bool
	cardScale           float64 = 1.0
	cardWidthPx         int32
	cardHeight          int32
	cardCursorY         int32
	cardItems           []drawItem
	cardCloseRect       winRect
	cardIconHandle      uintptr
	// cardBannerHandle is the current theme's loaded header banner bitmap —
	// reloaded (and the previous handle freed) at the top of every showCard()
	// call, same lifetime as cardIsLight itself, since the light/dark variant
	// depends on the theme read at that same moment.
	cardBannerHandle uintptr
	// cardForceReportRect/cardForceEvaluateRect mirror cardCloseRect's exact
	// hit-testing pattern (wmNcHitTest excludes them from the drag/caption
	// region, wmLButtonDown fires the action) — see cardCloseRect's usage
	// there for why a borderless WS_POPUP card needs this for every
	// clickable region rather than relying on real button controls.
	cardForceReportRect   winRect
	cardForceEvaluateRect winRect

	fontTitle   uintptr
	fontSection uintptr
	fontBody    uintptr
	fontBodyMed uintptr
	fontSmall   uintptr
	fontPill    uintptr
	fontsReady  bool
)

const fontFamilyUI = "Segoe UI"

// s DPI-scales a base pixel value against cardScale (see showCard).
func s(v int32) int32 {
	return int32(float64(v) * cardScale)
}

func ensureCardFonts() {
	if fontsReady {
		return
	}
	fontTitle = createFont(fontFamilyUI, s(19), fwBold)
	fontSection = createFont(fontFamilyUI, s(12), fwSemiBold)
	fontBody = createFont(fontFamilyUI, s(14), fwRegular)
	fontBodyMed = createFont(fontFamilyUI, s(14), fwSemiBold)
	fontSmall = createFont(fontFamilyUI, s(12), fwRegular)
	fontPill = createFont(fontFamilyUI, s(12), fwSemiBold)
	fontsReady = true
}

// loadCardIcon loads the same embedded shield-check .ico (already extracted
// to a temp file by main.go at startup — see lightIconPath/darkIconPath) at
// a size suited to the card's header mark, independent of whatever size the
// notification area itself is using.
func loadCardIcon(light bool, size int32) uintptr {
	path := darkIconPath
	if light {
		path = lightIconPath
	}
	if path == "" {
		return 0
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(pathPtr)), uintptr(imageIcon), uintptr(size), uintptr(size), uintptr(lrLoadFromFile))
	return h
}

// loadBannerBitmap loads the embedded header banner (extracted to a temp
// .bmp file by main.go's extractIcon — same mechanism as the tray/card
// icons, just a different file extension) at its native bannerRasterW x
// bannerRasterH resolution. LR_CREATEDIBSECTION (see its const doc comment)
// is what makes the returned handle safe to SelectObject into the memory DC
// paintCard's drawKindBitmap case creates for the StretchBlt. Returns 0 on
// any failure — paintCard's bitmap case tolerates a zero handle the same
// way DrawIconEx already tolerates one elsewhere on this card (nothing
// visible is drawn, nothing crashes).
func loadBannerBitmap(light bool) uintptr {
	path := darkBannerPath
	if light {
		path = lightBannerPath
	}
	if path == "" {
		return 0
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(pathPtr)), uintptr(imageBitmap), 0, 0, uintptr(lrLoadFromFile|lrCreateDibSection))
	return h
}

// ptInRect is the shared hit-test used by wmNcHitTest (to exclude a
// clickable rect from the drag/caption region) and wmLButtonDown (to fire
// its action) for every button on this card — cardCloseRect,
// cardForceReportRect, cardForceEvaluateRect.
func ptInRect(pt point, r winRect) bool {
	return pt.x >= r.left && pt.x <= r.right && pt.y >= r.top && pt.y <= r.bottom
}

// addActionButtons lays out the "Force report" / "Force evaluate
// compliance" pair side by side, full-width, right under the header. Drawn
// as drawKindPill rows like the rest of the card's badges, but with a
// smaller corner radius (s(8) vs the s(11)/s(13) used by status pills) so
// they read as rectangular buttons rather than another status indicator —
// the primary action (evaluate) solid brand-blue, the secondary (report)
// an outline in the same style as addHeroPills' left-hand outline pill.
func addActionButtons() {
	h := s(32)
	gap := s(10)
	btnW := (cardWidthPx - s(cardPadX)*2 - gap) / 2
	top := cardCursorY
	cardForceReportRect = winRect{left: s(cardPadX), top: top, right: s(cardPadX) + btnW, bottom: top + h}
	cardForceEvaluateRect = winRect{left: s(cardPadX) + btnW + gap, top: top, right: s(cardPadX) + btnW + gap + btnW, bottom: top + h}
	textAlign := dtCenter | dtVCenter | dtSingleLine | dtEndEllipsis
	cardItems = append(cardItems,
		drawItem{kind: drawKindPill, rect: cardForceReportRect, text: "Force report", font: fontPill, color: cardPrimaryTextColor(cardIsLight), bgColor: cardBorderColor(cardIsLight), align: textAlign, radius: s(8), outline: true},
		drawItem{kind: drawKindPill, rect: cardForceEvaluateRect, text: "Force evaluate compliance", font: fontPill, color: colWhite, bgColor: colBrand, align: textAlign, radius: s(8)},
	)
	cardCursorY = top + h + s(16)
}

func addDivider() {
	y := cardCursorY + s(8)
	cardItems = append(cardItems, drawItem{
		kind:    drawKindFill,
		rect:    winRect{left: s(cardPadX), top: y, right: cardWidthPx - s(cardPadX), bottom: y + 1},
		bgColor: cardBorderColor(cardIsLight),
	})
	cardCursorY = y + s(9)
}

func addSectionHeader(title string) {
	h := s(20)
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + h},
		text:  strings.ToUpper(title),
		font:  fontSection,
		color: colBrand,
		align: dtLeft | dtVCenter | dtSingleLine,
	})
	cardCursorY += h + s(4)
}

func addKVRow(label, value string, valueColor uintptr) {
	h := s(cardRowH)
	half := (cardWidthPx - s(cardPadX)*2) / 2
	labelRect := winRect{left: s(cardPadX), top: cardCursorY, right: s(cardPadX) + half, bottom: cardCursorY + h}
	valueRect := winRect{left: s(cardPadX) + half, top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + h}
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: labelRect, text: label, font: fontBody, color: cardMutedTextColor(), align: dtLeft | dtVCenter | dtSingleLine},
		drawItem{kind: drawKindText, rect: valueRect, text: value, font: fontBodyMed, color: valueColor, align: dtRight | dtVCenter | dtSingleLine | dtEndEllipsis},
	)
	cardCursorY += h
}

// addPillRow lays out a label (left, ellipsized against whatever room is
// actually left — not pre-truncated by character count) and a fixed-width
// filled pill (right). pillW should comfortably fit the longest text this
// call site ever passes (all of this card's pill texts are short, known
// words: "OK", "Failed", "Compliant", "N issues", "Violation").
func addPillRow(label, pillText string, pillBg, pillFg uintptr, pillW int32) {
	h := s(cardRowH)
	pw := s(pillW)
	ph := s(22)
	labelRect := winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX) - pw - s(10), bottom: cardCursorY + h}
	pillTop := cardCursorY + (h-ph)/2
	pillRect := winRect{left: cardWidthPx - s(cardPadX) - pw, top: pillTop, right: cardWidthPx - s(cardPadX), bottom: pillTop + ph}
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: labelRect, text: label, font: fontBody, color: cardMutedTextColor(), align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis},
		drawItem{kind: drawKindPill, rect: pillRect, text: pillText, font: fontPill, color: pillFg, bgColor: pillBg, align: dtCenter | dtVCenter | dtSingleLine, radius: s(11)},
	)
	cardCursorY += h
}

// cardPolicyPillW/cardPolicyGap are the fixed pill width and name-to-pill gap
// used both by addPolicyRow's actual layout and by buildCardContent's width
// measurement pass — kept as named constants specifically so the two can
// never drift out of sync with each other.
const (
	cardPolicyPillW = 78
	cardPolicyGap   = 10
)

// addPolicyRow lays out the policy name and its OK/Violation pill on a single
// row, name on the left (ellipsized only as a last-resort safety net; in
// practice buildCardContent has already measured every policy name and sized
// the whole card wide enough that DT_END_ELLIPSIS never actually needs to
// cut anything) and the pill right-aligned, matching the web app's own
// policy-list row style rather than stacking the pill onto its own line.
func addPolicyRow(name, pillText string, pillBg, pillFg uintptr) {
	h := s(cardRowH)
	pw, ph := s(cardPolicyPillW), s(20)
	nameRect := winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX) - pw - s(cardPolicyGap), bottom: cardCursorY + h}
	pillTop := cardCursorY + (h-ph)/2
	pillRect := winRect{left: cardWidthPx - s(cardPadX) - pw, top: pillTop, right: cardWidthPx - s(cardPadX), bottom: pillTop + ph}
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: nameRect, text: name, font: fontBody, color: cardPrimaryTextColor(cardIsLight), align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis},
		drawItem{kind: drawKindPill, rect: pillRect, text: pillText, font: fontPill, color: pillFg, bgColor: pillBg, align: dtCenter | dtVCenter | dtSingleLine, radius: s(10)},
	)
	cardCursorY += h + s(4)
}

func addBodyLine(text string, muted bool) {
	h := s(cardRowH)
	color := cardPrimaryTextColor(cardIsLight)
	if muted {
		color = cardMutedTextColor()
	}
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + h},
		text:  text,
		font:  fontBody,
		color: color,
		align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis,
	})
	cardCursorY += h
}

// addRiskBar draws a thin horizontal track plus a proportional colored fill
// (0-100), the same visual language as the Workspace Profile modal's usage
// bars, colored by compliance risk tier.
func addRiskBar(score int, barColor uintptr) {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	trackH := s(6)
	track := winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + trackH}
	cardItems = append(cardItems, drawItem{kind: drawKindFill, rect: track, bgColor: cardBorderColor(cardIsLight)})
	fillW := int32(float64(track.right-track.left) * float64(score) / 100.0)
	fill := winRect{left: track.left, top: track.top, right: track.left + fillW, bottom: track.bottom}
	cardItems = append(cardItems, drawItem{kind: drawKindFill, rect: fill, bgColor: barColor})
	cardCursorY += trackH + s(12)
}

// addHeroPills draws the centered pair of pills below the title/subtitle —
// left is an outline pill (border only, no fill, mirroring the web app's
// "Company" badge), right is a solid-filled status pill.
func addHeroPills(leftText string, leftW int32, rightText string, rightW int32, rightBg, rightFg uintptr) {
	h := s(26)
	gap := s(8)
	lw, rw := s(leftW), s(rightW)
	total := lw + gap + rw
	startX := (cardWidthPx - total) / 2
	leftRect := winRect{left: startX, top: cardCursorY, right: startX + lw, bottom: cardCursorY + h}
	rightRect := winRect{left: startX + lw + gap, top: cardCursorY, right: startX + lw + gap + rw, bottom: cardCursorY + h}
	borderCol := cardBorderColor(cardIsLight)
	cardItems = append(cardItems,
		drawItem{kind: drawKindPill, rect: leftRect, text: leftText, font: fontPill, color: cardMutedTextColor(), bgColor: borderCol, align: dtCenter | dtVCenter | dtSingleLine, radius: s(13), outline: true},
		drawItem{kind: drawKindPill, rect: rightRect, text: rightText, font: fontPill, color: rightFg, bgColor: rightBg, align: dtCenter | dtVCenter | dtSingleLine, radius: s(13)},
	)
	cardCursorY += h
}

// buildCardContent re-reads status.json fresh (see readStatusCache in
// main.go) and lays out the full row list every time the card is opened —
// cheap (a handful of small structs), and guarantees the card never shows
// stale data from a previous open.
func buildCardContent() {
	cardItems = cardItems[:0]
	cardCursorY = s(cardPadY)
	light := cardIsLight

	cache, err := readStatusCache()

	// Card width: starts at the existing fixed default (cardMinWidth) and
	// grows to fit the widest dynamic string this particular open actually
	// needs to show (workspace/device subtitle, policy names) — capped at
	// 90% of the screen width so it can never render partly off-screen on a
	// small/low-res display. Measured up front against a screen-reference
	// HDC via GetTextExtentPoint32W (the same glyph-metrics math DrawText
	// itself consults internally), so every drawItem appended below already
	// uses the final width and DT_END_ELLIPSIS never actually has anything
	// left to truncate — it stays on the text items purely as a safety net
	// for whatever's left over after the 90%-of-screen cap.
	cardWidthPx = s(cardMinWidth)
	deviceNameText := "This device"
	var slugText string
	if err == nil && cache != nil {
		if cache.DeviceName != "" {
			deviceNameText = cache.DeviceName
		}
		slugText = cache.WorkspaceSlug
		if screenDC, _, _ := procGetDC.Call(0); screenDC != 0 {
			needed := cardWidthPx
			if w := measureTextWidthPx(screenDC, fontTitle, deviceNameText) + s(cardPadX)*2; w > needed {
				needed = w
			}
			if w := measureTextWidthPx(screenDC, fontSmall, slugText) + s(cardPadX)*2; w > needed {
				needed = w
			}
			if cache.Compliance.Available {
				for _, p := range cache.Compliance.Policies {
					w := measureTextWidthPx(screenDC, fontBody, p.Name) + s(cardPadX)*2 + s(cardPolicyPillW) + s(cardPolicyGap)
					if w > needed {
						needed = w
					}
				}
			}
			procReleaseDC.Call(0, screenDC)

			if screenW, _, _ := procGetSystemMetrics.Call(uintptr(smCxScreen)); screenW > 0 {
				if maxW := int32(float64(screenW) * 0.9); needed > maxW {
					needed = maxW
				}
			}
			cardWidthPx = needed
		}
	}

	// Header: banner logo top-left (replacing the old centered "Applivery
	// SOAR" text title), close button top-right on the same row, then the
	// device name (bold, "same font type" as the old title per the brand
	// reference this layout follows) and workspace slug stacked left-aligned
	// underneath — mirrors the SOAR web app's own Workspace Profile modal
	// styling (bold name, muted slug below) without literally centering
	// everything the way that modal does, since the banner is explicitly
	// top-left here rather than centered.
	bannerH := s(22)
	bannerW := int32(float64(bannerH) * float64(bannerRasterW) / float64(bannerRasterH))
	cardItems = append(cardItems, drawItem{
		kind:         drawKindBitmap,
		rect:         winRect{left: s(cardPadX), top: cardCursorY, right: s(cardPadX) + bannerW, bottom: cardCursorY + bannerH},
		bitmapHandle: cardBannerHandle,
	})

	cardCloseRect = winRect{left: cardWidthPx - s(36), top: s(12), right: cardWidthPx - s(12), bottom: s(12) + s(24)}
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  cardCloseRect,
		text:  "×",
		font:  fontTitle,
		color: cardMutedTextColor(),
		align: dtCenter | dtVCenter | dtSingleLine,
	})
	cardCursorY += bannerH + s(14)

	deviceNameH := s(24)
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + deviceNameH},
		text:  deviceNameText,
		font:  fontTitle,
		color: cardPrimaryTextColor(light),
		align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis,
	})
	cardCursorY += deviceNameH + s(2)

	if slugText != "" {
		slugH := s(18)
		cardItems = append(cardItems, drawItem{
			kind:  drawKindText,
			rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + slugH},
			text:  slugText,
			font:  fontSmall,
			color: cardMutedTextColor(),
			align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis,
		})
		cardCursorY += slugH + s(2)
	}
	cardCursorY += s(12)

	addActionButtons()

	if err != nil || cache == nil {
		subtitleH := s(18)
		cardItems = append(cardItems, drawItem{
			kind:  drawKindText,
			rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + subtitleH},
			text:  "Waiting for the first report from this device",
			font:  fontSmall,
			color: cardMutedTextColor(),
			align: dtCenter | dtVCenter | dtSingleLine | dtEndEllipsis,
		})
		cardCursorY += subtitleH + s(16)
		cardHeight = cardCursorY + s(cardPadY)
		return
	}

	comp := cache.Compliance
	statusBg, statusFg, statusText := colSuccess, colWhite, "Compliant"
	if !comp.Available {
		statusBg, statusText = colGray400, "Unavailable"
	} else if !comp.Compliant {
		n := len(comp.Violations)
		plural := "s"
		if n == 1 {
			plural = ""
		}
		statusText = fmt.Sprintf("%d issue%s", n, plural)
		statusBg = colDanger
	}
	addHeroPills("Windows Device", 118, statusText, 118, statusBg, statusFg)
	cardCursorY += s(16)
	addDivider()

	addSectionHeader("Reporting")
	if cache.ReportedBitLocker {
		text, color := "Unknown", cardMutedTextColor()
		if cache.BitLockerStatus != nil {
			text = onOff(*cache.BitLockerStatus)
			color = colDanger
			if *cache.BitLockerStatus {
				color = colSuccess
			}
		}
		addKVRow("BitLocker", text, color)
	}
	if cache.ReportedFirewall {
		text, color := "Unknown", cardMutedTextColor()
		if cache.FirewallEnabled != nil {
			text = onOff(*cache.FirewallEnabled)
			color = colDanger
			if *cache.FirewallEnabled {
				color = colSuccess
			}
		}
		addKVRow("Firewall", text, color)
	}
	if cache.ReportedApps {
		addKVRow("App inventory", "Reported", cardMutedTextColor())
	}
	lastReportBg, lastReportFg, lastReportText := colSuccess, colWhite, "OK"
	if !cache.LastReportOK {
		lastReportBg, lastReportText = colDanger, "Failed"
	}
	addPillRow("Last report ("+formatRelativeTime(cache.LastReportAt)+")", lastReportText, lastReportBg, lastReportFg, 72)

	addDivider()
	addSectionHeader("Compliance")

	if !comp.Available {
		reason := comp.Reason
		if reason == "" {
			reason = "Compliance policies are not available for this device."
		}
		addBodyLine(reason, true)
	} else {
		if comp.RiskScore != nil {
			tier := ""
			if comp.RiskTier != nil {
				tier = *comp.RiskTier
			}
			riskColor := tierColor(tier)
			riskText := fmt.Sprintf("%d", *comp.RiskScore)
			if tier != "" {
				riskText += " · " + strings.Title(strings.ToLower(tier))
			}
			addKVRow("Risk score", riskText, riskColor)
			addRiskBar(*comp.RiskScore, riskColor)
		}

		if len(comp.Policies) > 0 {
			addBodyLine(fmt.Sprintf("Policies applied (%d)", len(comp.Policies)), true)
			violated := make(map[string]bool, len(comp.Violations))
			for _, v := range comp.Violations {
				violated[v.PolicyID] = true
			}
			shown := 0
			for _, p := range comp.Policies {
				if shown >= cardMaxPolicyRows {
					break
				}
				pillText, pillBg := "OK", colSuccess
				if violated[p.ID] {
					pillText, pillBg = "Violation", colDanger
				}
				addPolicyRow(p.Name, pillText, pillBg, colWhite)
				shown++
			}
			if len(comp.Policies) > cardMaxPolicyRows {
				addBodyLine(fmt.Sprintf("+%d more", len(comp.Policies)-cardMaxPolicyRows), true)
			}
		}
	}

	addDivider()
	footerH := s(16)
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + footerH}, text: "MANAGED BY YOUR ORGANIZATION", font: fontSmall, color: cardMutedTextColor(), align: dtCenter | dtVCenter | dtSingleLine},
	)
	cardCursorY += footerH + s(2)
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + footerH}, text: "Updated " + formatRelativeTime(cache.UpdatedAt), font: fontSmall, color: cardMutedTextColor(), align: dtCenter | dtVCenter | dtSingleLine},
	)
	cardCursorY += footerH

	cardHeight = cardCursorY + s(cardPadY)
}

func registerCardClassOnce() {
	if cardClassRegistered {
		return
	}
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, err := syscall.UTF16PtrFromString(cardClassName)
	if err != nil {
		return
	}
	wc := wndClassExW{
		lpfnWndProc:   syscall.NewCallback(cardWndProc),
		hInstance:     hInst,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	atom, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom != 0 {
		cardClassRegistered = true
	}
}

// showCard opens (or, if already open, just re-focuses) the status card,
// centered on the primary display. Re-entrancy-guarded: a second click
// while the card is already open just brings the existing window forward
// rather than stacking a duplicate.
func showCard() {
	if cardHwnd != 0 {
		procSetForegroundWin.Call(cardHwnd)
		return
	}

	cardIsLight = isLightTheme()

	// Reload the banner bitmap for whatever theme was just detected —
	// buildCardContent (called below) reads cardBannerHandle directly, so
	// this has to happen before that call. The previous handle (if any,
	// e.g. from a prior open under the other theme) is freed first to avoid
	// leaking a GDI bitmap handle on every card open.
	if cardBannerHandle != 0 {
		procDeleteObject.Call(cardBannerHandle)
	}
	cardBannerHandle = loadBannerBitmap(cardIsLight)

	cardScale = 1.0
	// GetDpiForSystem (Windows 10 1607+, no args) reports the system DPI —
	// combined with the Per-Monitor-v2 process awareness set in main(),
	// this keeps the card's fixed-pixel layout correctly sized (not just
	// unblurred) on the common 125%/150% laptop scaling factors. Falls back
	// to 1.0 (100%) if the call fails.
	if dpi, _, _ := procGetDpiForSystem.Call(); dpi > 0 {
		cardScale = float64(dpi) / 96.0
	}

	// cardWidthPx itself is computed inside buildCardContent (it needs fonts
	// and the status cache to measure against, neither ready yet here) —
	// nothing to set on this line anymore now that the card's width is
	// dynamic rather than a fixed 440px.
	ensureCardFonts()
	buildCardContent()
	registerCardClassOnce()

	screenW, _, _ := procGetSystemMetrics.Call(uintptr(smCxScreen))
	screenH, _, _ := procGetSystemMetrics.Call(uintptr(smCyScreen))
	x := (int32(screenW) - cardWidthPx) / 2
	y := (int32(screenH) - cardHeight) / 2

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, err := syscall.UTF16PtrFromString(cardClassName)
	if err != nil {
		return
	}
	title, err := syscall.UTF16PtrFromString("Applivery SOAR")
	if err != nil {
		return
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(wsExToolWindow|wsExTopMost),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(wsPopup),
		uintptr(x), uintptr(y), uintptr(cardWidthPx), uintptr(cardHeight),
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return
	}
	cardHwnd = hwnd

	radius := s(cardCornerRadius)
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(cardWidthPx), uintptr(cardHeight), uintptr(radius), uintptr(radius))
	if rgn != 0 {
		procSetWindowRgn.Call(hwnd, rgn, 1)
	}

	procShowWindow.Call(hwnd, uintptr(swShow))
	procSetForegroundWin.Call(hwnd)
}

func paintCard(hdc uintptr) {
	bg := cardSurfaceColor(cardIsLight)
	full := winRect{left: 0, top: 0, right: cardWidthPx, bottom: cardHeight}
	roundRectFill(hdc, &full, s(cardCornerRadius), bg)
	procSetBkMode.Call(hdc, uintptr(transparentBkMode))

	for i := range cardItems {
		item := &cardItems[i]
		r := item.rect
		switch item.kind {
		case drawKindFill:
			fillRectColor(hdc, &r, item.bgColor)
		case drawKindIcon:
			w := r.right - r.left
			h := r.bottom - r.top
			procDrawIconEx.Call(hdc, uintptr(r.left), uintptr(r.top), item.iconHandle, uintptr(w), uintptr(h), 0, 0, uintptr(diNormal))
		case drawKindBitmap:
			if item.bitmapHandle != 0 {
				if memDC, _, _ := procCreateCompatibleDC.Call(hdc); memDC != 0 {
					oldBmp, _, _ := procSelectObject.Call(memDC, item.bitmapHandle)
					procSetStretchBltMode.Call(hdc, uintptr(halftoneStretchMode))
					w := r.right - r.left
					h := r.bottom - r.top
					procStretchBlt.Call(hdc, uintptr(r.left), uintptr(r.top), uintptr(w), uintptr(h), memDC, 0, 0, uintptr(bannerRasterW), uintptr(bannerRasterH), uintptr(srcCopy))
					procSelectObject.Call(memDC, oldBmp)
					procDeleteDC.Call(memDC)
				}
			}
		case drawKindPill:
			if item.outline {
				roundRectStroke(hdc, &r, item.radius, item.bgColor, s(1))
			} else {
				roundRectFill(hdc, &r, item.radius, item.bgColor)
			}
			procSelectObject.Call(hdc, item.font)
			procSetTextColor.Call(hdc, item.color)
			drawText(hdc, item.text, &r, item.align)
		case drawKindText:
			procSelectObject.Call(hdc, item.font)
			procSetTextColor.Call(hdc, item.color)
			drawText(hdc, item.text, &r, item.align)
		}
	}

	inset := s(1)
	border := winRect{left: inset, top: inset, right: cardWidthPx - inset, bottom: cardHeight - inset}
	roundRectStroke(hdc, &border, s(cardCornerRadius)-inset, colBrand, s(2))
}

// cardWndProc backs the card's own window class — a small, self-contained
// message handler: paint, drag-to-move, close-button hit-test, Escape to
// dismiss, and auto-dismiss when the card loses foreground focus (mirrors
// how the old native popup menu dismissed on click-away, so the interaction
// still feels familiar even though the underlying implementation changed
// completely).
//
// Every dismiss path posts WM_CLOSE rather than calling DestroyWindow
// directly. DestroyWindow tears the window down synchronously, including
// dispatching WM_DESTROY to this same wndproc before returning -- calling
// it from inside WM_ACTIVATE in particular is a well-known Win32 pitfall:
// WM_ACTIVATE fires while the window manager's own activation handling is
// still on the call stack (showCard's ShowWindow/SetForegroundWindow are
// what triggers it), and re-entering DestroyWindow from there was hanging
// the whole process ("not responding") rather than merely misbehaving.
// Posting WM_CLOSE instead defers the actual teardown to its own, later,
// non-reentrant iteration of the message loop.
func cardWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 {
			paintCard(hdc)
		}
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmNcHitTest:
		// The card is a plain WS_POPUP with no title bar, so nothing about it
		// is draggable by default. Reporting HTCAPTION for a click anywhere
		// on it other than the close button hands the actual move-tracking
		// off to Windows' own built-in window-move loop (the same one a real
		// title bar drag would trigger) -- simpler and more correct than
		// hand-tracking WM_LBUTTONDOWN/WM_MOUSEMOVE/WM_LBUTTONUP ourselves
		// (which would also have to fight the existing close-button and
		// deactivate-to-dismiss handling below for the same mouse events).
		ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		if uint32(ret) == htClient {
			pt := point{x: int32(int16(uint32(lParam) & 0xFFFF)), y: int32(int16(uint32(lParam) >> 16))}
			procScreenToClient.Call(hwnd, uintptr(unsafe.Pointer(&pt)))
			onButton := ptInRect(pt, cardCloseRect) || ptInRect(pt, cardForceReportRect) || ptInRect(pt, cardForceEvaluateRect)
			if !onButton {
				return htCaption
			}
		}
		return ret
	case wmLButtonDown:
		pt := point{x: int32(int16(uint32(lParam) & 0xFFFF)), y: int32(int16(uint32(lParam) >> 16))}
		switch {
		case ptInRect(pt, cardCloseRect):
			procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
		case ptInRect(pt, cardForceReportRect):
			triggerForceReport()
		case ptInRect(pt, cardForceEvaluateRect):
			triggerForceEvaluate()
		}
		return 0
	case wmKeyDown:
		if wParam == vkEscape {
			procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
		}
		return 0
	case wmActivate:
		if uint32(wParam)&0xFFFF == waInactive {
			procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if cardIconHandle != 0 {
			procDestroyIcon.Call(cardIconHandle)
			cardIconHandle = 0
		}
		if cardBannerHandle != 0 {
			procDeleteObject.Call(cardBannerHandle)
			cardBannerHandle = 0
		}
		cardHwnd = 0
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return ret
}

// paintStruct mirrors PAINTSTRUCT — passed to BeginPaint/EndPaint, which
// need the full, correctly-sized struct even though we only read the hdc
// field ourselves.
type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     winRect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}
