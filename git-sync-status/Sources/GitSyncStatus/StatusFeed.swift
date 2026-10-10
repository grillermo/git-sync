import Foundation
import Observation
import GitSyncStatusCore

/// Runs `git-sync status --json --follow` (the installed copy the hooks use)
/// and publishes each snapshot it prints. git-sync is the only reader of
/// ~/.gitsync; this never looks there itself. If the process exits - an
/// ./activate replaced the binary, say - it is started again after 2 s.
@MainActor
@Observable
final class StatusFeed {
    private(set) var snapshot: Snapshot?
    private(set) var updatedAt: Date?
    private(set) var feedError: String?

    /// Called after every change, for the parts of the app that are not
    /// SwiftUI (the menu bar icon).
    @ObservationIgnored var onChange: (() -> Void)?

    @ObservationIgnored private var process: Process?
    @ObservationIgnored private var input: Pipe?
    @ObservationIgnored private var stopped = false

    static let binary = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent(".gitsync/bin/git-sync")

    func start() {
        stopped = false
        launch()
    }

    func stop() {
        stopped = true
        // git-sync status exits on stdin EOF; terminate is the backstop.
        try? input?.fileHandleForWriting.close()
        process?.terminate()
    }

    private func launch() {
        guard !stopped else { return }
        let p = Process()
        p.executableURL = Self.binary
        p.arguments = ["status", "--json", "--follow"]
        let output = Pipe()
        let input = Pipe()
        p.standardOutput = output
        p.standardInput = input
        p.standardError = FileHandle.nullDevice
        p.terminationHandler = { [weak self] _ in
            Task { @MainActor in self?.exited() }
        }
        do {
            try p.run()
        } catch {
            publishError("cannot run \(Self.binary.path): \(error.localizedDescription)")
            relaunchLater()
            return
        }
        process = p
        self.input = input

        let handle = output.fileHandleForReading
        Task.detached { [weak self] in
            do {
                for try await line in handle.bytes.lines {
                    guard let snap = try? Snapshot.decode(Data(line.utf8)) else { continue }
                    await self?.publish(snap)
                }
            } catch {
                // The pipe closed; terminationHandler takes it from here.
            }
        }
    }

    private func publish(_ snap: Snapshot) {
        snapshot = snap
        updatedAt = Date()
        feedError = snap.error
        onChange?()
    }

    private func publishError(_ message: String) {
        feedError = message
        onChange?()
    }

    private func exited() {
        guard !stopped else { return }
        publishError("git-sync status exited, reconnecting…")
        relaunchLater()
    }

    private func relaunchLater() {
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: .seconds(2))
            self?.launch()
        }
    }
}
