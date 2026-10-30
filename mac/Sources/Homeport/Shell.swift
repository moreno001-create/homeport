import Foundation

// Shell runs commands for the app. Apps opened from Finder get a bare PATH, so Homebrew's and OrbStack's folders are
// added (where homeport, caddy, docker and brew are).
enum Shell {
    static let home = FileManager.default.homeDirectoryForCurrentUser.path
    static let path = ["/opt/homebrew/bin", "/usr/local/bin", home + "/.orbstack/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"]
        .joined(separator: ":")

    struct Result {
        let status: Int32
        let out: String
        var ok: Bool { status == 0 }
    }

    // run runs a command to the end (stopping it after timeout seconds) and returns its output.
    static func run(_ args: [String], env: [String: String] = [:], timeout: TimeInterval = 20) async -> Result {
        await withCheckedContinuation { cont in
            DispatchQueue.global().async {
                let p = process(args, env)
                let pipe = Pipe()
                p.standardOutput = pipe
                p.standardError = pipe
                p.standardInput = FileHandle.nullDevice
                do { try p.run() } catch {
                    cont.resume(returning: Result(status: -1, out: error.localizedDescription))
                    return
                }
                let stop = DispatchWorkItem { if p.isRunning { p.terminate() } }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: stop)
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                stop.cancel()
                cont.resume(returning: Result(status: p.terminationStatus, out: String(decoding: data, as: UTF8.self)))
            }
        }
    }

    // stream runs a long command (the installer), handing each line of its output to onLine on the main thread.
    static func stream(_ args: [String], env: [String: String] = [:], onLine: @escaping (String) -> Void) async -> Int32 {
        await withCheckedContinuation { cont in
            DispatchQueue.global().async {
                let p = process(args, env)
                let pipe = Pipe()
                p.standardOutput = pipe
                p.standardError = pipe
                p.standardInput = FileHandle.nullDevice
                do { try p.run() } catch {
                    let msg = error.localizedDescription
                    DispatchQueue.main.async { onLine(msg) }
                    cont.resume(returning: -1)
                    return
                }
                let h = pipe.fileHandleForReading
                var buf = Data()
                func send(_ d: Data) {
                    let line = clean(String(decoding: d, as: UTF8.self))
                    DispatchQueue.main.async { onLine(line) }
                }
                while true {
                    let chunk = h.availableData
                    if chunk.isEmpty { break }
                    buf.append(chunk)
                    while let nl = buf.firstIndex(of: 0x0A) {
                        send(buf.subdata(in: buf.startIndex..<nl))
                        buf = buf.subdata(in: buf.index(after: nl)..<buf.endIndex)
                    }
                }
                if !buf.isEmpty { send(buf) }
                p.waitUntilExit()
                cont.resume(returning: p.terminationStatus)
            }
        }
    }

    private static func process(_ args: [String], _ env: [String: String]) -> Process {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = args
        var e = ProcessInfo.processInfo.environment
        e["PATH"] = path
        e.merge(env) { $1 }
        p.environment = e
        return p
    }

    // clean drops the installer's color codes.
    static func clean(_ s: String) -> String {
        s.replacingOccurrences(of: "\u{1B}\\[[0-9;]*m", with: "", options: .regularExpression)
    }
}
