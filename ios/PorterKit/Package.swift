// swift-tools-version:5.9
import PackageDescription

// PorterKit has no dependencies on Apple platforms. On Linux, where it is
// built only to test the protocol code against the router daemon,
// CryptoKit is supplied by swift-crypto.
#if os(Linux)
let dependencies: [Package.Dependency] = [
    .package(url: "https://github.com/apple/swift-crypto.git", from: "3.0.0")
]
let targetDependencies: [Target.Dependency] = [.product(name: "Crypto", package: "swift-crypto")]
#else
let dependencies: [Package.Dependency] = []
let targetDependencies: [Target.Dependency] = []
#endif

let package = Package(
    name: "PorterKit",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [.library(name: "PorterKit", targets: ["PorterKit"])],
    dependencies: dependencies,
    targets: [
        .target(name: "PorterKit", dependencies: targetDependencies),
        // The tests play the router's side of the handshake, which takes
        // the same primitives.
        .testTarget(name: "PorterKitTests", dependencies: ["PorterKit"] + targetDependencies, resources: [.copy("Resources/vectors.json")]),
    ],
    swiftLanguageVersions: [.v5]
)
