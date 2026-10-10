// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "git-sync-status",
    platforms: [.macOS(.v15)],
    products: [
        .executable(name: "git-sync-status", targets: ["GitSyncStatus"]),
    ],
    targets: [
        .target(name: "GitSyncStatusCore", path: "Sources/GitSyncStatusCore"),
        .executableTarget(
            name: "GitSyncStatus",
            dependencies: ["GitSyncStatusCore"],
            path: "Sources/GitSyncStatus"
        ),
        .testTarget(
            name: "GitSyncStatusCoreTests",
            dependencies: ["GitSyncStatusCore"],
            path: "Tests/GitSyncStatusCoreTests"
        ),
    ],
    swiftLanguageModes: [.v5]
)
