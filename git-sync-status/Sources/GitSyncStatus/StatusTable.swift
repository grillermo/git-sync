import AppKit
import SwiftUI
import GitSyncStatusCore

private enum Col {
    static let repo: CGFloat = 170
    static let state: CGFloat = 130
    static let when: CGFloat = 90
    static let rowHeight: CGFloat = 24
}

/// The window: one row per synced repo, then the pending queue, then a
/// footer. Clicking a row copies its repo's absolute path.
struct StatusTable: View {
    let feed: StatusFeed
    @State private var copied: String?

    var body: some View {
        let split = feed.snapshot.map { rows(for: $0) } ?? (repos: [], pending: [])
        VStack(alignment: .leading, spacing: 0) {
            header
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    if split.repos.isEmpty {
                        Text(feed.snapshot == nil ? "Waiting for git-sync…" : "No repos are selected for syncing.")
                            .foregroundStyle(.secondary)
                            .padding(12)
                    }
                    ForEach(split.repos) { row in line(row) }
                    if !split.pending.isEmpty {
                        Text("Pending")
                            .font(.caption.bold())
                            .foregroundStyle(.secondary)
                            .padding(.horizontal, 12)
                            .padding(.top, 10)
                            .padding(.bottom, 4)
                        ForEach(split.pending) { row in line(row) }
                    }
                }
            }
            .frame(height: height(split.repos.count, split.pending.count))
            Divider()
            footer
        }
        .frame(width: 780)
    }

    private func height(_ repos: Int, _ pending: Int) -> CGFloat {
        let body = CGFloat(max(repos, 1)) * Col.rowHeight + (pending > 0 ? 30 + CGFloat(pending) * Col.rowHeight : 0)
        return min(460, body + 8)
    }

    private var header: some View {
        HStack(spacing: 8) {
            Text("Repo").frame(width: Col.repo, alignment: .leading)
            Text("State").frame(width: Col.state, alignment: .leading)
            Text("Last sync").frame(width: Col.when, alignment: .leading)
            Text("Detail").frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.caption.bold())
        .foregroundStyle(.secondary)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private func line(_ row: Row) -> some View {
        RowView(row: row, copied: copied == row.id) { copy(row) }
    }

    private var footer: some View {
        HStack(spacing: 12) {
            if let error = feed.feedError {
                Text(error).foregroundStyle(.red).lineLimit(1)
            } else if let at = feed.updatedAt {
                Text("Updated \(at.formatted(date: .omitted, time: .standard))").foregroundStyle(.secondary)
            }
            Spacer()
            Text("Click a row to copy its path").foregroundStyle(.tertiary)
            Button("Quit") { NSApp.terminate(nil) }
        }
        .font(.caption)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }

    private func copy(_ row: Row) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(row.path, forType: .string)
        copied = row.id
        Task {
            try? await Task.sleep(for: .seconds(1.2))
            if copied == row.id { copied = nil }
        }
    }
}

private struct RowView: View {
    let row: Row
    let copied: Bool
    let action: () -> Void
    @State private var hover = false

    var body: some View {
        HStack(spacing: 8) {
            Text(row.name)
                .lineLimit(1).truncationMode(.middle)
                .frame(width: Col.repo, alignment: .leading)
            Text("\(row.symbol) \(row.state)")
                .foregroundStyle(color)
                .lineLimit(1)
                .frame(width: Col.state, alignment: .leading)
            TimelineView(.periodic(from: .now, by: 30)) { context in
                Text(ago(row.when, now: context.date)).foregroundStyle(.secondary)
            }
            .frame(width: Col.when, alignment: .leading)
            Text(copied ? "Copied \(row.path)" : row.detail)
                .foregroundStyle(copied ? Color.accentColor : Color.primary)
                .lineLimit(1).truncationMode(.tail)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.system(size: 12))
        .frame(height: Col.rowHeight)
        .padding(.horizontal, 12)
        .background(hover ? Color.primary.opacity(0.08) : Color.clear)
        .contentShape(Rectangle())
        .onHover { hover = $0 }
        .onTapGesture(perform: action)
        .help(row.fullDetail.isEmpty ? row.path : row.fullDetail)
    }

    private var color: Color {
        switch row.tone {
        case .ok: .green
        case .error: .red
        case .warn: .orange
        case .syncing: .accentColor
        case .muted: .secondary
        }
    }
}
