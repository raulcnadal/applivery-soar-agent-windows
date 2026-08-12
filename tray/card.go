//go:build windows
// +build windows

// The status card shown when a user clicks (or right-clicks — Explorer
// treats both as "activate" for a notification-area icon) the tray icon.
// Replaces what used to be a native Win32 popup menu with a small,
// custom-painted, borderless window: a centered "card" styled after the
// BlueSky design system used across the rest of SOAR (brand blue #0241E3,
// Outfit font, rounded surfaces) rather than the grey, native-menu look a
// plain TrackPopupMenuEx produces. Read-only and purely informational/
// troubleshooting — there is nothing to click through to (see main.go's
// doc comment for why: this agent runs on end-user devices, not admin
// machines, so there's no dashboard link here).
//
// Built as a second borderless top-level window (WS_POPUP) rather than a
// dialog resource or any GUI toolkit, painted with the same raw GDI calls
// declared in gdi.go — kept content as a flat []drawItem list built once in
// buildCardContent() and replayed on every WM_PAINT, so the paint handler
// itself stays trivial and layout logic lives in one place.
package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

const (
	wmPaint       = 0x000F
	wmLButtonDown = 0x0201
	wmKeyDown     = 0x0100
	wmActivate    = 0x0006

	vkEscape = 0x1B

	wsPopup        = 0x80000000
	wsExToolWindow = 0x00000080
	wsExTopMost    = 0x00000008

	smCxScreen = 0
	smCyScreen = 1

	swShow = 5

	waInactive = 0

	cardClassName     = "ApplivierySOARCardWndClass"
	cardCornerRadius  = 14
	cardPadX          = 20
	cardPadY          = 18
	cardRowH          = 26
	cardMaxPolicyRows = 6
)

var (
	procShowWindow      = moduser32.NewProc("ShowWindow")
	procBeginPaint      = moduser32.NewProc("BeginPaint")
	procEndPaint        = moduser32.NewProc("EndPaint")
	procGetDpiForSystem = moduser32.NewProc("GetDpiForSystem")
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
	drawKindDivider
)

type drawItem struct {
	kind    drawKind
	rect    winRect
	text    string
	font    uintptr
	color   uintptr
	bgColor uintptr
	align   uintptr
	radius  int32
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

	fontTitle   uintptr
	fontSection uintptr
	fontBody    uintptr
	fontBodyMed uintptr
	fontSmall   uintptr
	fontPill    uintptr
	fontsReady  bool
)

// s DPI-scales a base pixel value against cardScale (see showCard).
func s(v int32) int32 {
	return int32(float64(v) * cardScale)
}

func ensureCardFonts() {
	if fontsReady {
		return
	}
	fontTitle = createFont(fontFamilySemiBold, s(17))
	fontSection = createFont(fontFamilySemiBold, s(12))
	fontBody = createFont(fontFamilyRegular, s(13))
	fontBodyMed = createFont(fontFamilyMedium, s(13))
	fontSmall = createFont(fontFamilyRegular, s(12))
	fontPill = createFont(fontFamilyMedium, s(12))
	fontsReady = true
}

func addDivider() {
	y := cardCursorY + s(8)
	cardItems = append(cardItems, drawItem{
		kind:    drawKindDivider,
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

func addPillRow(label, pillText string, pillBg, pillFg uintptr) {
	h := s(cardRowH)
	pillW := s(96)
	pillH := s(20)
	labelRect := winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX) - pillW - s(8), bottom: cardCursorY + h}
	pillTop := cardCursorY + (h-pillH)/2
	pillRect := winRect{left: cardWidthPx - s(cardPadX) - pillW, top: pillTop, right: cardWidthPx - s(cardPadX), bottom: pillTop + pillH}
	cardItems = append(cardItems,
		drawItem{kind: drawKindText, rect: labelRect, text: label, font: fontBody, color: cardMutedTextColor(), align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis},
		drawItem{kind: drawKindPill, rect: pillRect, text: pillText, font: fontPill, color: pillFg, bgColor: pillBg, align: dtCenter | dtVCenter | dtSingleLine, radius: s(10)},
	)
	cardCursorY += h
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

// buildCardContent re-reads status.json fresh (see readStatusCache in
// main.go) and lays out the full row list every time the card is opened —
// cheap (a handful of small structs), and guarantees the card never shows
// stale data from a previous open.
func buildCardContent() {
	cardItems = cardItems[:0]
	cardCursorY = 0
	light := cardIsLight

	headerH := s(44)
	closeSize := s(28)
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  winRect{left: s(cardPadX), top: 0, right: cardWidthPx - s(cardPadX) - closeSize, bottom: headerH},
		text:  "Applivery SOAR",
		font:  fontTitle,
		color: cardPrimaryTextColor(light),
		align: dtLeft | dtVCenter | dtSingleLine,
	})
	cardCloseRect = winRect{left: cardWidthPx - closeSize - s(8), top: s(8), right: cardWidthPx - s(8), bottom: s(8) + closeSize}
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  cardCloseRect,
		text:  "×",
		font:  fontTitle,
		color: cardMutedTextColor(),
		align: dtCenter | dtVCenter | dtSingleLine,
	})
	cardCursorY = headerH

	cache, err := readStatusCache()
	if err != nil || cache == nil {
		addBodyLine("Waiting for the first report from this device…", true)
		cardCursorY += s(12)
		cardHeight = cardCursorY + s(cardPadY)
		return
	}

	subtitle := cache.WorkspaceSlug
	if cache.DeviceName != "" {
		subtitle = cache.DeviceName + " · " + subtitle
	}
	cardItems = append(cardItems, drawItem{
		kind:  drawKindText,
		rect:  winRect{left: s(cardPadX), top: cardCursorY, right: cardWidthPx - s(cardPadX), bottom: cardCursorY + s(18)},
		text:  truncate(subtitle, 60),
		font:  fontSmall,
		color: cardMutedTextColor(),
		align: dtLeft | dtVCenter | dtSingleLine | dtEndEllipsis,
	})
	cardCursorY += s(18) + s(6)
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
	addPillRow("Last report ("+formatRelativeTime(cache.LastReportAt)+")", lastReportText, lastReportBg, lastReportFg)

	addDivider()
	addSectionHeader("Compliance")

	comp := cache.Compliance
	if !comp.Available {
		reason := comp.Reason
		if reason == "" {
			reason = "Compliance policies are not available for this device."
		}
		addBodyLine(reason, true)
	} else {
		statusBg, statusFg, statusText := colSuccess, colWhite, "Compliant"
		if !comp.Compliant {
			n := len(comp.Violations)
			plural := "s"
			if n == 1 {
				plural = ""
			}
			statusText = fmt.Sprintf("%d issue%s", n, plural)
			statusBg = colDanger
		}
		addPillRow("Status", statusText, statusBg, statusFg)

		if comp.RiskScore != nil {
			tier := ""
			if comp.RiskTier != nil {
				tier = *comp.RiskTier
			}
			riskText := fmt.Sprintf("%d", *comp.RiskScore)
			if tier != "" {
				riskText += " · " + strings.Title(strings.ToLower(tier))
			}
			addKVRow("Risk score", riskText, tierColor(tier))
		}

		if len(comp.Policies) > 0 {
			addDivider()
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
				addPillRow(truncate(p.Name, 26), pillText, pillBg, colWhite)
				shown++
			}
			if len(comp.Policies) > cardMaxPolicyRows {
				addBodyLine(fmt.Sprintf("+%d more", len(comp.Policies)-cardMaxPolicyRows), true)
			}
		}
	}

	addDivider()
	addBodyLine("Managed by your organization", true)
	addBodyLine("Updated "+formatRelativeTime(cache.UpdatedAt), true)

	cardCursorY += s(6)
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
	cardScale = 1.0
	// GetDpiForSystem (Windows 10 1607+, no args) reports the system DPI —
	// combined with the Per-Monitor-V2 process awareness set in main(),
	// this keeps the card's fixed-pixel layout correctly sized (not just
	// unblurred) on the common 125%/150% laptop scaling factors most likely
	// behind the "icon looks bad" complaint in the first place. Falls back
	// to 1.0 (100%) if the call fails, matching this repo's general
	// best-effort/no-crash style for optional Win32 feature calls.
	if dpi, _, _ := procGetDpiForSystem.Call(); dpi > 0 {
		cardScale = float64(dpi) / 96.0
	}
	cardWidthPx = s(380)

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
		case drawKindDivider:
			fillRectColor(hdc, &r, item.bgColor)
		case drawKindPill:
			roundRectFill(hdc, &r, item.radius, item.bgColor)
			procSelectObject.Call(hdc, item.font)
			procSetTextColor.Call(hdc, item.color)
			drawText(hdc, item.text, &r, item.align)
		case drawKindText:
			procSelectObject.Call(hdc, item.font)
			procSetTextColor.Call(hdc, item.color)
			drawText(hdc, item.text, &r, item.align)
		}
	}
}

// cardWndProc backs the card's own window class — a small, self-contained
// message handler: paint, close-button hit-test, Escape to dismiss, and
// auto-dismiss when the card loses foreground focus (mirrors how the old
// native popup menu dismissed on click-away, so the interaction still feels
// familiar even though the underlying implementation changed completely).
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
	case wmLButtonDown:
		x := int32(int16(uint32(lParam) & 0xFFFF))
		y := int32(int16(uint32(lParam) >> 16))
		if x >= cardCloseRect.left && x <= cardCloseRect.right && y >= cardCloseRect.top && y <= cardCloseRect.bottom {
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmKeyDown:
		if wParam == vkEscape {
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmActivate:
		if uint32(wParam)&0xFFFF == waInactive {
			procDestroyWindow.Call(hwnd)
		}
		return 0
	case wmDestroy:
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
