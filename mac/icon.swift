// Draws Homeport's app icon (a white house on a blue rounded square) at one size: swift icon.swift SIZE OUT.png
import AppKit

let size = CGFloat(Double(CommandLine.arguments[1])!)
let out = CommandLine.arguments[2]
let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(size), pixelsHigh: Int(size), bitsPerSample: 8,
                           samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)

// macOS icons leave a margin around the rounded square.
let inset = size * 0.1
let rect = NSRect(x: inset, y: inset, width: size - 2 * inset, height: size - 2 * inset)
let shape = NSBezierPath(roundedRect: rect, xRadius: rect.width * 0.225, yRadius: rect.width * 0.225)
NSGradient(starting: NSColor(red: 0.25, green: 0.56, blue: 1.0, alpha: 1), ending: NSColor(red: 0.07, green: 0.33, blue: 0.85, alpha: 1))!
    .draw(in: shape, angle: -90)

let config = NSImage.SymbolConfiguration(pointSize: rect.width * 0.52, weight: .semibold)
    .applying(.init(paletteColors: [.white]))
if let house = NSImage(systemSymbolName: "house.fill", accessibilityDescription: nil)?.withSymbolConfiguration(config) {
    let s = house.size
    house.draw(in: NSRect(x: (size - s.width) / 2, y: (size - s.height) / 2 - size * 0.01, width: s.width, height: s.height))
}
NSGraphicsContext.restoreGraphicsState()
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
