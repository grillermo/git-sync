import AppKit

/// Composes the menu bar icon: the whole glyph (sync arrows and git mark)
/// rotated by `angle` while something syncs, plus a red dot that never
/// rotates while a repo has a problem. A transparent ring is cleared around
/// the dot so it reads as a badge. The glyph is drawn at display time in the
/// menu bar's own text colour, so it follows light and dark menu bars.
/// Without a problem it is a template image, so the system shades it exactly
/// like its own icons.
@MainActor
struct IconRenderer {
    private let glyph = Bundle.main.image(forResource: "icon")
    private let size = NSSize(width: 22, height: 22)

    func image(angle: CGFloat, problem: Bool) -> NSImage {
        // Run outside the .app (swift run) there is no icon PNG.
        guard let glyph else {
            let fallback = NSImage(systemSymbolName: "arrow.triangle.2.circlepath", accessibilityDescription: "git-sync")
                ?? NSImage(size: size)
            fallback.isTemplate = true
            return fallback
        }
        let image = NSImage(size: size, flipped: false) { rect in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            // The badge sits where the SVG puts it: centre (84, 16), dot r 9,
            // cleared ring r 14, on the 90-unit canvas that starts at (5, 5).
            let unit = rect.width / 90
            let centre = CGPoint(x: (84 - 5) * unit, y: rect.height - (16 - 5) * unit)
            func circle(_ r: CGFloat) -> NSRect {
                NSRect(x: centre.x - r * unit, y: centre.y - r * unit, width: 2 * r * unit, height: 2 * r * unit)
            }

            ctx.saveGState()
            ctx.beginTransparencyLayer(auxiliaryInfo: nil)
            ctx.saveGState()
            ctx.translateBy(x: rect.midX, y: rect.midY)
            ctx.rotate(by: angle)
            ctx.translateBy(x: -rect.midX, y: -rect.midY)
            glyph.draw(in: rect)
            ctx.restoreGState()
            NSColor.labelColor.set()
            rect.fill(using: .sourceAtop)
            if problem {
                ctx.setBlendMode(.clear)
                NSBezierPath(ovalIn: circle(14)).fill()
                ctx.setBlendMode(.normal)
            }
            ctx.endTransparencyLayer()
            ctx.restoreGState()
            if problem {
                NSColor.systemRed.set()
                NSBezierPath(ovalIn: circle(9)).fill()
            }
            return true
        }
        image.isTemplate = !problem
        image.accessibilityDescription = "git-sync"
        return image
    }
}
