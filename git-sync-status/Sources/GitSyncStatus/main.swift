import AppKit

// A menu bar-only app: no Dock icon, no app menu (LSUIElement in the
// bundle's Info.plist says the same for launches through Finder or `open`).
// Top-level code in main.swift runs on the main thread.
MainActor.assumeIsolated {
    let app = NSApplication.shared
    let delegate = AppDelegate()
    app.delegate = delegate
    app.setActivationPolicy(.accessory)
    app.run()
}
