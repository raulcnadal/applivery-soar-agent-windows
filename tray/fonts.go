//go:build windows
// +build windows

// Embeds and privately loads the 3 static Outfit TTF weights the card's
// text actually uses (Regular/SemiBold/Bold — matching the BlueSky design
// system's own weight scale used elsewhere on this card, see
// ensureCardFonts's doc comment in card.go), so the card renders in the
// product's real brand typeface on every Windows machine regardless of
// whether "Outfit" happens to be installed system-wide. It almost
// certainly isn't on an end-user device — this repo shipped with zero font
// files at all until this pass — and a bare CreateFontIndirectW("Outfit",
// ...) call against an uninstalled family name fails silently: GDI's font
// mapper just substitutes the nearest installed font with no error at any
// layer, which is exactly the "font not respecting Outfit properly"
// symptom reported against the previous Segoe-UI-only version of this
// card.
//
// AddFontMemResourceEx (declared in gdi.go, not AddFontResourceEx +
// FR_PRIVATE, which needs an on-disk file) registers a font straight from
// an in-memory buffer, scoped to this process only — nothing is written to
// disk, and nothing this process does is visible to, or persists for, any
// other process on the machine.
//
// Each of the 3 TTFs is tagged with its own unique family name — "Outfit
// Regular" / "Outfit SemiBold" / "Outfit Bold" — rather than shipped as
// three weight variants of one "Outfit" family. That's deliberate: it
// means CreateFontIndirectW's family-name lookup can never accidentally
// resolve to a *different*, system- or another-app-installed "Outfit"
// build instead of the exact weight this card asked for, since each name
// here is unique to these exact 3 files.
//
// Source: static instances of Google Fonts' variable Outfit[wght].ttf
// (OFL-1.1 licensed — see fonts/OFL.txt), pinned to wght=400/600/700 via
// `fonttools varLib.instancer` and re-tagged (name table + OS/2.usWeightClass
// + head.macStyle/OS/2.fsSelection) with fonttools' own APIs to give each
// instance the distinct family/PostScript identity described above — the
// raw instancer output otherwise leaves every instance's name table
// pointing at the variable font's first named instance ("Outfit Thin"),
// which is a known fonttools quirk, not a hand-editing mistake.
package main

import "embed"

//go:embed fonts/Outfit-Regular.ttf fonts/Outfit-SemiBold.ttf fonts/Outfit-Bold.ttf
var fontFS embed.FS

const (
	outfitRegular  = "Outfit Regular"
	outfitSemiBold = "Outfit SemiBold"
	outfitBold     = "Outfit Bold"
)

// outfitLoaded is only set true once every one of the 3 weights has
// registered successfully — a partial load (e.g. Regular+SemiBold but not
// Bold) would otherwise silently mix Outfit and Segoe UI on the same card,
// which reads as more broken than a single consistent fallback. See
// fontFamilyForWeight (card.go) for how callers consult this.
var outfitLoaded bool

// loadEmbeddedFonts registers all 3 Outfit weights into this process's
// private font table. Called once from ensureCardFonts (card.go), same
// lazy-init guard (fontsReady) as the fonts themselves. Best-effort and
// side-effect-only: never returns an error, matching the tolerant,
// never-crash pattern the rest of this card's asset loading already
// follows (loadBannerBitmap, loadCardIcon in card.go).
func loadEmbeddedFonts() {
	ok := true
	for _, name := range []string{
		"fonts/Outfit-Regular.ttf",
		"fonts/Outfit-SemiBold.ttf",
		"fonts/Outfit-Bold.ttf",
	} {
		data, err := fontFS.ReadFile(name)
		if err != nil || addFontMemResource(data) == 0 {
			ok = false
		}
	}
	outfitLoaded = ok
}
