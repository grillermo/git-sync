import AppKit

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let feed = StatusFeed()
    private var statusItem: StatusItemController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = StatusItemController(feed: feed)
        feed.start()
    }

    func applicationWillTerminate(_ notification: Notification) {
        feed.stop()
    }
}
