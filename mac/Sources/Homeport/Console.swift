import AppKit
import SwiftUI

// The console window: what installs, updates and uninstalls print (Activity), and Homeport's and Caddy's own logs as
// they're written.

enum ConsoleTab: String, CaseIterable, Identifiable {
    case activity = "Activity", homeport = "Homeport", caddy = "Caddy"
    var id: String { rawValue }
}

// LogFile follows a log file once a second, keeping its last lines. Caddy writes JSON lines, which are made readable.
@MainActor
final class LogFile: ObservableObject {
    @Published var lines: [String] = []
    let path: String
    private let json: Bool
    private var offset: UInt64 = 0
    private var partial = ""
    private var timer: Timer?
    private let keep = 5000

    init(_ path: String, json: Bool = false) {
        self.path = path
        self.json = json
    }

    func follow() {
        guard timer == nil else { return }
        read()
        timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.read() }
        }
    }

    func pause() {
        timer?.invalidate()
        timer = nil
    }

    private func read() {
        guard let h = FileHandle(forReadingAtPath: path) else {
            if !lines.isEmpty || offset != 0 { lines = []; offset = 0; partial = "" }
            return
        }
        defer { try? h.close() }
        let size = (try? h.seekToEnd()) ?? 0
        if size < offset { lines = []; offset = 0; partial = "" } // started again (a new install)
        var skipFirst = false
        if offset == 0 && size > 1_000_000 { // a big log: its last megabyte, from the next whole line
            offset = size - 1_000_000
            skipFirst = true
        }
        guard size > offset else { return }
        try? h.seek(toOffset: offset)
        let data = h.readData(ofLength: Int(size - offset))
        offset += UInt64(data.count)
        var parts = (partial + String(decoding: data, as: UTF8.self)).components(separatedBy: "\n")
        partial = parts.removeLast()
        if skipFirst, !parts.isEmpty { parts.removeFirst() }
        lines.append(contentsOf: json ? parts.map(Self.readable) : parts)
        if lines.count > keep { lines.removeFirst(lines.count - keep) }
    }

    // readable turns one of Caddy's JSON lines into "23:12:01  ERROR  tls.obtain: could not get certificate · error=…".
    static func readable(_ line: String) -> String {
        guard let obj = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else { return line }
        var out = ""
        if let ts = obj["ts"] as? Double {
            out = Date(timeIntervalSince1970: ts).formatted(date: .omitted, time: .standard) + "  "
        }
        out += ((obj["level"] as? String) ?? "").uppercased().padding(toLength: 5, withPad: " ", startingAt: 0) + "  "
        if let logger = obj["logger"] as? String { out += logger + ": " }
        out += (obj["msg"] as? String) ?? ""
        for key in ["error", "identifier", "domains", "server_name", "addr", "challenge_type", "remaining"] {
            if let list = obj[key] as? [Any] {
                out += " · \(key)=" + list.map { "\($0)" }.joined(separator: ", ")
            } else if let v = obj[key] {
                out += " · \(key)=\(v)"
            }
        }
        return out
    }
}

struct ConsoleView: View {
    @EnvironmentObject var m: Monitor
    @StateObject private var homeportLog = LogFile(Homeport.logs + "/homeport.log")
    @StateObject private var caddyLog = LogFile(Homeport.logs + "/caddy.log", json: true)
    @State private var filter = ""
    @State private var follow = true

    private var lines: [String] {
        let all: [String]
        switch m.consoleTab {
        case .activity: all = m.activity
        case .homeport: all = homeportLog.lines
        case .caddy: all = caddyLog.lines
        }
        return filter.isEmpty ? all : all.filter { $0.localizedCaseInsensitiveContains(filter) }
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                Picker("", selection: $m.consoleTab) {
                    ForEach(ConsoleTab.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
                TextField("Filter", text: $filter)
                    .textFieldStyle(.roundedBorder)
                    .frame(maxWidth: 220)
                Spacer()
                Toggle("Follow", isOn: $follow).toggleStyle(.checkbox)
                Button { copy() } label: { Image(systemName: "doc.on.doc") }.help("Copy these lines")
                if m.consoleTab == .activity {
                    Button { m.activity = [] } label: { Image(systemName: "trash") }
                        .help("Clear").disabled(m.job != nil)
                } else {
                    Button { m.reveal(m.consoleTab == .caddy ? caddyLog.path : homeportLog.path) } label: { Image(systemName: "folder") }
                        .help("Show the log file in Finder")
                }
            }
            .padding(10)
            if m.consoleTab == .activity, let banner = jobBanner {
                HStack(spacing: 8) {
                    banner.icon
                    Text(banner.text).font(.callout)
                    Spacer()
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 6)
                .background(.bar)
            }
            Divider()
            LogLines(lines: lines, follow: follow, empty: emptyText)
        }
        .frame(minWidth: 560, minHeight: 300)
        .onAppear {
            homeportLog.follow()
            caddyLog.follow()
        }
        .onDisappear {
            homeportLog.pause()
            caddyLog.pause()
        }
    }

    private var jobBanner: (icon: AnyView, text: String)? {
        if let job = m.job { return (AnyView(ProgressView().controlSize(.small)), job + "…") }
        if let ok = m.jobResult {
            return (AnyView(Image(systemName: ok ? "checkmark.circle.fill" : "xmark.octagon.fill").foregroundStyle(ok ? .green : .red)),
                    ok ? "Finished" : "It didn't finish: the lines below say why")
        }
        return nil
    }

    private var emptyText: String {
        if !filter.isEmpty { return "No lines match “\(filter)”." }
        switch m.consoleTab {
        case .activity: return "Installs, updates and uninstalls show what they're doing here."
        case .homeport, .caddy: return m.installed ? "Nothing logged yet." : "Homeport isn't installed, so there's no log yet."
        }
    }

    private func copy() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(lines.joined(separator: "\n"), forType: .string)
    }
}

struct LogLines: View {
    let lines: [String]
    let follow: Bool
    let empty: String

    var body: some View {
        if lines.isEmpty {
            Text(empty)
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color(nsColor: .textBackgroundColor))
        } else {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 1) {
                        ForEach(lines.indices, id: \.self) { i in
                            Text(lines[i].isEmpty ? " " : lines[i])
                                .font(.system(size: 11.5, design: .monospaced))
                                .fontWeight(lines[i].hasPrefix("==>") || lines[i].hasPrefix("────") ? .bold : .regular)
                                .foregroundStyle(color(lines[i]))
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .id(i)
                        }
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 8)
                }
                .background(Color(nsColor: .textBackgroundColor))
                .onAppear { if follow { proxy.scrollTo(lines.count - 1, anchor: .bottom) } }
                .onChange(of: lines.count) { n in
                    if follow && n > 0 { proxy.scrollTo(n - 1, anchor: .bottom) }
                }
            }
        }
    }

    private func color(_ line: String) -> Color {
        let l = line.lowercased()
        if l.contains("error") || l.hasPrefix("✗") || l.contains("couldn't") || l.contains("failed") { return .red }
        if l.contains("warn") { return .orange }
        if line.hasPrefix("✓") { return .green }
        if line.hasPrefix("────") { return .accentColor }
        return .primary
    }
}
