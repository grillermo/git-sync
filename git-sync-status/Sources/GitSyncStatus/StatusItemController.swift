import AppKit
import SwiftUI
import GitSyncStatusCore

/// The menu bar item: draws the icon from the feed, turns the glyph while
/// something syncs (one turn every 2 s, and no timer at all otherwise), and
/// toggles the window. The window is a transient popover: clicking anywhere
/// else closes it, and so does Esc.
@MainActor
final class StatusItemController: NSObject {
    private let feed: StatusFeed
    private let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    private let popover = NSPopover()
    private let icon = IconRenderer()
    private var state = IconState(nil)
    private var angle: CGFloat = 0
    private var timer: Timer?
    private var keyMonitor: Any?

    private static let fps = 20.0
    private static let secondsPerTurn = 2.0

    init(feed: StatusFeed) {
        self.feed = feed
        super.init()

        let host = NSHostingController(rootView: StatusTable(feed: feed))
        host.sizingOptions = .preferredContentSize
        popover.contentViewController = host
        popover.behavior = .transient
        popover.animates = false

        if let button = item.button {
            button.target = self
            button.action = #selector(toggle(_:))
        }
        feed.onChange = { [weak self] in self?.refresh() }
        refresh()

        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            guard let self, event.keyCode == 53, self.popover.isShown else { return event } // 53: Esc
            self.popover.performClose(nil)
            return nil
        }
    }

    @objc private func toggle(_ sender: Any?) {
        if popover.isShown {
            popover.performClose(nil)
            return
        }
        guard let button = item.button else { return }
        // An accessory app must activate for the popover to take key events
        // (Esc) and to close on a click elsewhere.
        NSApp.activate()
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        popover.contentViewController?.view.window?.makeKey()
    }

    private func refresh() {
        state = IconState(feed.snapshot)
        if state.syncing {
            if timer == nil {
                timer = Timer.scheduledTimer(withTimeInterval: 1 / Self.fps, repeats: true) { [weak self] _ in
                    MainActor.assumeIsolated { self?.tick() }
                }
            }
        } else {
            timer?.invalidate()
            timer = nil
            angle = 0
        }
        draw()
    }

    private func tick() {
        let step = 2 * CGFloat.pi / CGFloat(Self.fps * Self.secondsPerTurn)
        // The arrows point counter-clockwise, so the turn goes that way too.
        angle = (angle + step).truncatingRemainder(dividingBy: 2 * .pi)
        draw()
    }

    private func draw() {
        item.button?.image = icon.image(angle: angle, problem: state.problem)
        item.button?.toolTip = state.problem ? "git-sync: problems" : (state.syncing ? "git-sync: syncing" : "git-sync")
    }
}
