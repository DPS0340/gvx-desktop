package ui

import (
	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/widget"
)

// labsPage holds what is still experimental, apart from the settings that
// are not: running containers without a Docker Engine, and amd64 builds in
// a Rosetta VM, which needs the former.
func labsPage(m *Model) ggui.Widget {
	warning := widget.Row(
		glyph(lucide.FlaskConical, 15).Color(colorAmber),
		widget.Expanded(ui.Caption(tr("These features are still being built. They may change or go away in an update, and turning one off undoes what it set up."))),
	).Gap(8).Align(widget.AlignStart)
	return page(
		headed(pageHeader(tr("Labs"), ui.Caption(tr("Experimental features to try before they are done.")))),
		widget.Box(warning).Pad(10, 12).Radius(8).Fill(rgbA(0xf59e0b, 0x14)).Border(1, rgbA(0xf59e0b, 0x55)),
		settingsGroup(lucide.Container, tr("Sandbox engine"),
			sandboxEngineSettings(m),
			amd64BuilderSettings(m),
		),
	)
}
