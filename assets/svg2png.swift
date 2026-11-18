// Draws an SVG into a square PNG, as the app's icon and the mark the app
// shows are made from the drawings beside this file:
//
//	swift assets/svg2png.swift assets/gvx-desktop-icon.svg assets/icon.png 1024
//	swift assets/svg2png.swift assets/gvx-desktop-mark.svg internal/ui/mark.png 128
//
// The icon is drawn to the edges, as Windows shows it; ggui build insets
// the Mac's in Apple's grid, 824 pixels of 1024, as ggui.toml's
// macos.icon_inset asks. The last number, when given, is the size the
// drawing takes in the middle of the picture, clear around it.
import AppKit

let a = CommandLine.arguments
guard a.count >= 4, let size = Int(a[3]), let img = NSImage(contentsOfFile: a[1]) else {
    FileHandle.standardError.write("usage: svg2png.swift in.svg out.png size [body]\n".data(using: .utf8)!)
    exit(2)
}
let body = a.count > 4 ? Int(a[4]) ?? size : size
let rep = NSBitmapImageRep(
    bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4,
    hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
rep.size = NSSize(width: size, height: size)
NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
NSGraphicsContext.current!.imageInterpolation = .high
let inset = CGFloat(size - body) / 2
img.draw(
    in: NSRect(x: inset, y: inset, width: CGFloat(body), height: CGFloat(body)),
    from: .zero, operation: .copy, fraction: 1)
NSGraphicsContext.restoreGraphicsState()
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: a[2]))
