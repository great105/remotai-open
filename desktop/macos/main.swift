import AppKit
import WebKit

private var nativeLanguage: String = {
    let explicit = ProcessInfo.processInfo.environment["REMOTAI_LANGUAGE"]
    if explicit == "ru" || explicit == "en" { return explicit! }
    let saved = UserDefaults.standard.string(forKey: "RemotaiLanguage")
    if saved == "ru" || saved == "en" { return saved! }
    return Locale.preferredLanguages.first?.lowercased().hasPrefix("ru") == true ? "ru" : "en"
}()
private func localized(_ russian: String, _ english: String) -> String {
    return nativeLanguage == "ru" ? russian : english
}

// A window for the shared /miniapp client. The agent owns its own lifetime:
// closing this window or quitting the UI never terminates it or its PTYs.
final class RemotaiApp: NSObject, NSApplicationDelegate, NSWindowDelegate,
    WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate, WKScriptMessageHandler {
    private var window: NSWindow!
    private var web: WKWebView!
    private var localURL: URL?
    private var bootstrap: Process?
    private var downloads = Set<WKDownload>()
    private var notice: String?
    private var starting = false
    private var loaded = false
    private var reopenCount = 0
    private var checkingAgent = false
    private var recoveringAgent = false
    private var bootstrapCount = 0

    func applicationDidFinishLaunching(_ notification: Notification) {
        makeMenu()
        let size = NSSize(width: 1100, height: 780)
        window = NSWindow(contentRect: NSRect(origin: .zero, size: size),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Remotai"
        window.minSize = NSSize(width: 640, height: 480)
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.setFrameAutosaveName("RemotaiMainWindow")
        if let screen = NSScreen.main, window.frame.width > screen.visibleFrame.width || window.frame.height > screen.visibleFrame.height {
            window.setFrame(screen.visibleFrame.insetBy(dx: 12, dy: 12), display: false)
        }
        window.center()
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        config.userContentController.add(self, name: "remotaiLanguage")
        let languageScript = """
        (() => { const report = () => window.webkit.messageHandlers.remotaiLanguage.postMessage(document.documentElement.lang);
          new MutationObserver(report).observe(document.documentElement, { attributes: true, attributeFilter: ['lang'] }); report(); })();
        """
        config.userContentController.addUserScript(WKUserScript(source: languageScript, injectionTime: .atDocumentEnd, forMainFrameOnly: true))
        web = WKWebView(frame: NSRect(origin: .zero, size: size), configuration: config)
        web.autoresizingMask = [.width, .height]
        web.navigationDelegate = self
        web.uiDelegate = self
        web.allowsBackForwardNavigationGestures = true
        showWindow()
        startAgent()
    }

    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard message.name == "remotaiLanguage", let language = message.body as? String,
              language == "ru" || language == "en", language != nativeLanguage else { return }
        nativeLanguage = language
        UserDefaults.standard.set(language, forKey: "RemotaiLanguage")
        makeMenu()
    }

    private func makeMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: localized("О Remotai", "About Remotai"), action: #selector(about), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: localized("Скрыть Remotai", "Hide Remotai"), action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: localized("Закрыть Remotai", "Quit Remotai"), action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        menu.addItem(appItem)
        let editItem = NSMenuItem()
        editItem.title = localized("Правка", "Edit")
        let edit = NSMenu(title: localized("Правка", "Edit"))
        for (title, action, key) in [(localized("Отменить", "Undo"), "undo:", "z"), (localized("Вырезать", "Cut"), "cut:", "x"),
                                     (localized("Копировать", "Copy"), "copy:", "c"), (localized("Вставить", "Paste"), "paste:", "v"),
                                     (localized("Выделить всё", "Select All"), "selectAll:", "a")] {
            edit.addItem(withTitle: title, action: Selector(action), keyEquivalent: key)
        }
        editItem.submenu = edit
        menu.addItem(editItem)
        let viewItem = NSMenuItem()
        viewItem.title = localized("Окно", "Window")
        let view = NSMenu(title: localized("Окно", "Window"))
        view.addItem(withTitle: localized("Показать Remotai", "Show Remotai"), action: #selector(showWindow), keyEquivalent: "0")
        view.addItem(withTitle: localized("Назад", "Back"), action: #selector(goBack), keyEquivalent: "[")
        view.addItem(withTitle: localized("Обновить страницу", "Reload Page"), action: #selector(reloadPage), keyEquivalent: "r")
        view.addItem(withTitle: localized("Закрыть окно", "Close Window"), action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        viewItem.submenu = view
        menu.addItem(viewItem)
        NSApp.mainMenu = menu
    }

    @objc private func about() {
        let alert = NSAlert()
        alert.messageText = "Remotai"
        alert.informativeText = localized("Терминалы продолжают работать при закрытии окна. Чтобы остановить Remotai на этом Mac, откройте «Панель ПК» и выберите «Завершить Remotai на этом компьютере…».", "Terminals keep running when you close the window. To stop Remotai on this Mac, open the computer panel and choose “Quit Remotai on this computer…”.")
        alert.beginSheetModal(for: window)
    }

    @objc private func showWindow() {
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showWindow() // Preserve navigation, drafts and terminal scroll position.
        reopenCount += 1
        writeSmokeReport(extra: ["reopened": true])
        ensureAgentRunning()
        return true
    }

    private func ensureAgentRunning() {
        guard !starting, !checkingAgent else { return }
        guard let local = localURL, var parts = URLComponents(url: local, resolvingAgainstBaseURL: false) else {
            startAgent()
            return
        }
        parts.path = "/api/setup/status"
        parts.query = nil
        parts.fragment = nil
        guard let url = parts.url else { return }
        checkingAgent = true
        let request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 5)
        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            let state = data.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
            let alive = error == nil && (response as? HTTPURLResponse)?.statusCode == 200 &&
                !(state?["device_id"] as? String ?? "").isEmpty && !(state?["version"] as? String ?? "").isEmpty
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.checkingAgent = false
                if !alive {
                    self.recoveringAgent = true
                    self.startAgent()
                }
            }
        }.resume()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    @objc private func goBack() { if web.canGoBack { web.goBack() } }
    @objc private func reloadPage() {
        if loaded { web.reload() } else { startAgent() }
    }

    private func showStatus(_ text: String, retry: Bool) {
        let label = NSTextField(wrappingLabelWithString: text)
        label.font = .systemFont(ofSize: 16)
        label.alignment = .center
        let stack = NSStackView(views: [label])
        stack.orientation = .vertical
        stack.spacing = 20
        if retry {
            let button = NSButton(title: localized("Повторить", "Retry"), target: self, action: #selector(startAgent))
            button.bezelStyle = .rounded
            stack.addArrangedSubview(button)
        } else {
            let progress = NSProgressIndicator()
            progress.style = .spinning
            progress.startAnimation(nil)
            stack.addArrangedSubview(progress)
        }
        let content = NSView(frame: window.contentView!.bounds)
        content.addSubview(stack)
        stack.translatesAutoresizingMaskIntoConstraints = false
        NSLayoutConstraint.activate([
            stack.centerXAnchor.constraint(equalTo: content.centerXAnchor),
            stack.centerYAnchor.constraint(equalTo: content.centerYAnchor),
            stack.widthAnchor.constraint(lessThanOrEqualTo: content.widthAnchor, constant: -80),
            stack.widthAnchor.constraint(lessThanOrEqualToConstant: 540),
        ])
        window.contentView = content
    }

    @objc private func startAgent() {
        guard !starting else { return }
        starting = true
        loaded = false
        bootstrapCount += 1
        showStatus(localized("Открываем Remotai…", "Opening Remotai…"), retry: false)
        let task = Process()
        task.executableURL = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/Remotai")
        task.arguments = ["--desktop-window"]
        var taskEnvironment = ProcessInfo.processInfo.environment
        taskEnvironment["REMOTAI_LANGUAGE"] = nativeLanguage
        task.environment = taskEnvironment
        let output = Pipe()
        task.standardOutput = output
        task.standardError = FileHandle.nullDevice
        bootstrap = task
        task.terminationHandler = { [weak self] process in
            let data = output.fileHandleForReading.readDataToEndOfFile()
            let result = (try? JSONSerialization.jsonObject(with: data)) as? [String: String]
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.starting = false
                self.bootstrap = nil
                guard process.terminationStatus == 0, let raw = result?["url"],
                      let url = URL(string: raw), url.scheme == "http",
                      ["localhost", "127.0.0.1"].contains(url.host ?? ""),
                      url.port != nil, url.user == nil, url.password == nil else {
                    self.showStatus(result?["error"] ?? localized("Не удалось запустить Remotai. Повторите открытие приложения.", "Could not start Remotai. Open the application again."), retry: true)
                    return
                }
                self.localURL = url
                self.notice = result?["notice"]
                self.web.load(URLRequest(url: url))
            }
        }
        do { try task.run() } catch {
            starting = false
            bootstrap = nil
            showStatus(localized("В приложении не найден Remotai. Заново перенесите его из установочного диска в «Программы».", "Remotai is missing from the application. Copy it from the installation disk to Applications again."), retry: true)
        }
    }

    private func isLocal(_ url: URL) -> Bool {
        url.scheme == "http" && ["localhost", "127.0.0.1"].contains(url.host ?? "") &&
        url.port == localURL?.port && url.user == nil && url.password == nil
    }

    private func openExternal(_ url: URL) {
        if ["https", "http", "tg", "mailto"].contains(url.scheme?.lowercased() ?? "") {
            NSWorkspace.shared.open(url)
        }
    }

    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else { decisionHandler(.cancel); return }
        if action.shouldPerformDownload && (isLocal(url) || url.scheme == "blob") {
            decisionHandler(.download)
        } else if isLocal(url) || action.targetFrame?.isMainFrame == false {
            decisionHandler(.allow)
        } else {
            decisionHandler(.cancel)
            if action.navigationType == .linkActivated || action.targetFrame == nil { openExternal(url) }
        }
    }

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = action.request.url {
            if isLocal(url) { webView.load(action.request) } else { openExternal(url) }
        }
        return nil
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        loaded = true
        web.frame = window.contentView!.bounds
        window.contentView = web
        if let message = notice {
            notice = nil
            let alert = NSAlert()
            alert.messageText = localized("Обновление Remotai", "Remotai Update")
            alert.informativeText = message
            alert.beginSheetModal(for: window)
        }
        runSmokeCheck()
        if recoveringAgent {
            recoveringAgent = false
            writeSmokeReport(extra: ["reopened": true, "agent_recovered": true])
        }
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        if (error as NSError).code == NSURLErrorCancelled { return }
        loaded = false
        showStatus(localized("Не удалось открыть Remotai. Если приложение обновляется, подождите немного и нажмите «Повторить».", "Could not open Remotai. If an update is in progress, wait a moment and click Retry."), retry: true)
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: localized("Продолжить", "Continue"))
        alert.addButton(withTitle: localized("Отмена", "Cancel"))
        alert.beginSheetModal(for: window) { response in completionHandler(response == .alertFirstButtonReturn) }
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String,
                 defaultText: String?, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (String?) -> Void) {
        let alert = NSAlert()
        alert.messageText = prompt
        let input = NSTextField(string: defaultText ?? "")
        input.frame = NSRect(x: 0, y: 0, width: 320, height: 24)
        alert.accessoryView = input
        alert.addButton(withTitle: localized("Сохранить", "Save"))
        alert.addButton(withTitle: localized("Отмена", "Cancel"))
        alert.beginSheetModal(for: window) { response in completionHandler(response == .alertFirstButtonReturn ? input.stringValue : nil) }
    }

    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.canChooseFiles = true
        panel.beginSheetModal(for: window) { response in completionHandler(response == .OK ? panel.urls : nil) }
    }

    func webView(_ webView: WKWebView, decidePolicyFor response: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(response.canShowMIMEType ? .allow : .download)
    }
    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        downloads.insert(download); download.delegate = self
    }
    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        downloads.insert(download); download.delegate = self
    }
    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = (suggestedFilename as NSString).lastPathComponent
        panel.beginSheetModal(for: window) { result in completionHandler(result == .OK ? panel.url : nil) }
    }
    func downloadDidFinish(_ download: WKDownload) { downloads.remove(download) }
    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        downloads.remove(download)
    }

    // Hosted-runner acceptance exercises this actual window through LaunchServices.
    // No URLs, tokens or page contents are written to the proof file.
    private var smokePath: URL? {
        let args = CommandLine.arguments
        guard args.count == 3, args[1] == "--smoke-test", args[2].hasPrefix("/") else { return nil }
        return URL(fileURLWithPath: args[2])
    }
    private var smokeChecked = false
    private var smokeDOM = false
    private func writeSmokeReport(extra: [String: Any] = [:]) {
        guard let path = smokePath else { return }
        var report: [String: Any] = ["native_window": window.isVisible, "webview_loaded": loaded,
            "dom_ready": smokeDOM, "shared_client": web.url?.path.hasPrefix("/miniapp") == true,
            "reopen_count": reopenCount, "bootstrap_count": bootstrapCount,
            "dock_app": NSApp.activationPolicy() == .regular]
        report["ui_language"] = nativeLanguage
        report["menu_localized"] = NSApp.mainMenu?.items.dropFirst().first?.title == localized("Правка", "Edit")
        extra.forEach { report[$0] = $1 }
        if let data = try? JSONSerialization.data(withJSONObject: report, options: .prettyPrinted) {
            try? data.write(to: path, options: .atomic)
        }
    }
    private func runSmokeCheck() {
        guard let path = smokePath, !smokeChecked else { return }
        smokeChecked = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            self.web.evaluateJavaScript("document.readyState === 'complete' && (location.pathname.startsWith('/miniapp') ? !!document.getElementById('root')?.children.length : document.body.innerText.includes('Remotai'))") { result, error in
                self.smokeDOM = (result as? Bool) == true && error == nil
                self.web.takeSnapshot(with: nil) { picture, _ in
                    if let data = picture?.tiffRepresentation, let bitmap = NSBitmapImageRep(data: data),
                       let png = bitmap.representation(using: .png, properties: [:]) {
                        try? png.write(to: path.deletingPathExtension().appendingPathExtension("png"))
                    }
                    self.window.performClose(nil)
                    self.writeSmokeReport(extra: ["closed_after_load": !self.window.isVisible])
                }
            }
        }
    }
}

let app = NSApplication.shared
let delegate = RemotaiApp()
app.setActivationPolicy(.regular)
app.delegate = delegate
app.run()
