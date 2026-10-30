// swift-tools-version:5.10
// Homeport for Mac: a small app that installs Homeport and shows what's running, with start, stop and restart.
// Build the app with ./build-app.sh (it makes dist/Homeport.app).
import PackageDescription

let package = Package(
    name: "HomeportMac",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(name: "Homeport", path: "Sources/Homeport"),
    ]
)
