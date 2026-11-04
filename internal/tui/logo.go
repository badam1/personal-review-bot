package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// logoSprite is the project icon (internal/web/icon.svg) as 16×16 pixel art, one letter per pixel: b the tile, l the
// lens, m the magnifier, g the check, . transparent.
const logoSprite = `
..bbbbbbbbbbbb..
.bbbbbbbbbbbbbb.
bbbbmmmmmbbbbbbb
bbbmmlllmmbbbbbb
bbmmllllgmmbbbbb
bbmllllgglmbbbbb
bbmlglggllmbbbbb
bbmlggggllmbbbbb
bbmmlggllmmbbbbb
bbbmmlllmmmbbbbb
bbbbmmmmmmmmbbbb
bbbbbbbbbbmmmbbb
bbbbbbbbbbbmmmbb
bbbbbbbbbbbbmmmb
.bbbbbbbbbbbbmb.
..bbbbbbbbbbbb..
`

var logoColours = map[byte]string{'b': "#1e1e2e", 'l': "#313244", 'm': "#cba6f7", 'g': "#a6e3a1"}

// logo draws the sprite with half blocks: each character cell holds two pixels, the upper one as the foreground of ▀
// and the lower one as its background, so the icon stays square in a terminal: 16 columns by 8 rows.
func logo() string {
	rows := strings.Fields(logoSprite)
	var b strings.Builder
	for y := 0; y+1 < len(rows); y += 2 {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < len(rows[y]); x++ {
			top, bottom := logoColours[rows[y][x]], logoColours[rows[y+1][x]]
			cell := lipgloss.NewStyle()
			switch {
			case top == "" && bottom == "":
				b.WriteString(" ")
				continue
			case bottom == "":
				b.WriteString(cell.Foreground(lipgloss.Color(top)).Render("▀"))
			case top == "":
				b.WriteString(cell.Foreground(lipgloss.Color(bottom)).Render("▄"))
			case top == bottom:
				b.WriteString(cell.Foreground(lipgloss.Color(top)).Render("█"))
			default:
				b.WriteString(cell.Foreground(lipgloss.Color(top)).Background(lipgloss.Color(bottom)).Render("▀"))
			}
		}
	}
	return b.String()
}
