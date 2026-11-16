module github.com/ironpark/gvx-desktop

go 1.27.0

require (
	github.com/ironpark/ggfx v0.0.0-20261115173452-69402fdc030e
	github.com/ironpark/ggui v0.0.0-20261114074010-026f56d67cce
	github.com/ironpark/ggui/icons/lucide v0.0.0-00010101000000-000000000000
	github.com/ironpark/ggwd v0.0.0-20261115062550-27e69288e59f
)

require (
	github.com/ebitengine/hideconsole v1.0.0 // indirect
	github.com/ebitengine/purego v0.11.0 // indirect
	github.com/gogpu/naga v0.21.10 // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace (
	github.com/ironpark/ggui => ../ggui
	github.com/ironpark/ggui/icons/lucide => ../ggui/icons/lucide
)
