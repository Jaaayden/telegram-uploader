//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// Implemented in Go (shell_darwin.go).
extern void tvuGoMessage(char *message);
extern int tvuGoCloseRequest(void);
extern void tvuGoReady(void);
extern void tvuGoDrop(char *jsonPaths);

// Encodes file-system paths as a JSON array for Go. Returns nil when empty.
static NSString *tvuPathsJSON(NSArray<NSURL *> *urls) {
    NSMutableArray<NSString *> *paths = [NSMutableArray array];
    for (NSURL *url in urls) {
        if (url.isFileURL && url.path != nil) {
            [paths addObject:url.path];
        }
    }
    if (paths.count == 0) {
        return nil;
    }
    NSData *data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:nil];
    return data == nil ? nil : [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
}

// The page cannot see the paths of dropped files, so the view reads them from
// the drag pasteboard. The drag still continues into WebKit, which gives the
// page its usual drag events for the drop hint.
@interface TVUWebView : WKWebView
@end

@implementation TVUWebView
- (BOOL)performDragOperation:(id<NSDraggingInfo>)sender {
    NSArray<NSURL *> *urls = [[sender draggingPasteboard]
        readObjectsForClasses:@[[NSURL class]]
                      options:@{NSPasteboardURLReadingFileURLsOnlyKey : @YES}];
    NSString *json = tvuPathsJSON(urls);
    if (json != nil) {
        tvuGoDrop((char *)json.UTF8String);
    }
    return [super performDragOperation:sender];
}
@end

static NSWindow *gWindow;
static WKWebView *gWebView;
static BOOL gForceClose;
static BOOL gPageLoaded;

@interface TVUDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate, WKScriptMessageHandler, WKNavigationDelegate>
@end

@implementation TVUDelegate

- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message {
    if ([message.body isKindOfClass:[NSString class]]) {
        tvuGoMessage((char *)[(NSString *)message.body UTF8String]);
    }
}

- (BOOL)windowShouldClose:(NSWindow *)sender {
    if (gForceClose) {
        return YES;
    }
    return tvuGoCloseRequest() ? YES : NO;
}

- (void)windowWillClose:(NSNotification *)notification {
    // End [NSApp run] so Go can shut down cleanly instead of exiting inside
    // AppKit. stop: only takes effect after the next event, so post one.
    [NSApp stop:nil];
    NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                       location:NSZeroPoint
                                  modifierFlags:0
                                      timestamp:0
                                   windowNumber:0
                                        context:nil
                                        subtype:0
                                          data1:0
                                          data2:0];
    [NSApp postEvent:wake atStart:YES];
}

- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
    // Cmd+Q and the Dock's Quit take the same path as the close button, so
    // the application can ask before abandoning an upload.
    [gWindow performClose:nil];
    return NSTerminateCancel;
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
    return NO;
}

- (BOOL)applicationSupportsSecureRestorableState:(NSApplication *)app {
    return YES;
}

- (void)webView:(WKWebView *)webView
    decidePolicyForNavigationAction:(WKNavigationAction *)action
                    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    // The page is a single document loaded from a string, once. Links are
    // opened by Go in the default browser, and a dropped file must never
    // replace the interface with a preview of that file.
    if (!gPageLoaded && action.navigationType == WKNavigationTypeOther) {
        decisionHandler(WKNavigationActionPolicyAllow);
    } else {
        decisionHandler(WKNavigationActionPolicyCancel);
    }
}

- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation {
    gPageLoaded = YES;
}

@end

static TVUDelegate *gDelegate;

static void tvuEnsureApplication(void) {
    if (gDelegate != nil) {
        return;
    }
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
    gDelegate = [[TVUDelegate alloc] init];
    [NSApp setDelegate:gDelegate];
}

static NSMenuItem *tvuItem(NSMenu *menu, NSString *title, SEL action, NSString *key) {
    return [menu addItemWithTitle:title action:action keyEquivalent:key];
}

// Without a main menu the standard shortcuts (copy, paste, select all, quit)
// do not reach the web view's text fields.
static void tvuBuildMenu(NSString *appName) {
    NSMenu *bar = [[NSMenu alloc] init];

    NSMenuItem *appItem = [bar addItemWithTitle:@"" action:nil keyEquivalent:@""];
    NSMenu *appMenu = [[NSMenu alloc] init];
    tvuItem(appMenu, [@"隐藏 " stringByAppendingString:appName], @selector(hide:), @"h");
    NSMenuItem *hideOthers = tvuItem(appMenu, @"隐藏其他", @selector(hideOtherApplications:), @"h");
    hideOthers.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagOption;
    tvuItem(appMenu, @"全部显示", @selector(unhideAllApplications:), @"");
    [appMenu addItem:[NSMenuItem separatorItem]];
    tvuItem(appMenu, [@"退出 " stringByAppendingString:appName], @selector(terminate:), @"q");
    appItem.submenu = appMenu;

    NSMenuItem *editItem = [bar addItemWithTitle:@"编辑" action:nil keyEquivalent:@""];
    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"编辑"];
    tvuItem(editMenu, @"撤销", @selector(undo:), @"z");
    tvuItem(editMenu, @"重做", @selector(redo:), @"Z");
    [editMenu addItem:[NSMenuItem separatorItem]];
    tvuItem(editMenu, @"剪切", @selector(cut:), @"x");
    tvuItem(editMenu, @"拷贝", @selector(copy:), @"c");
    tvuItem(editMenu, @"粘贴", @selector(paste:), @"v");
    tvuItem(editMenu, @"全选", @selector(selectAll:), @"a");
    editItem.submenu = editMenu;

    NSMenuItem *windowItem = [bar addItemWithTitle:@"窗口" action:nil keyEquivalent:@""];
    NSMenu *windowMenu = [[NSMenu alloc] initWithTitle:@"窗口"];
    tvuItem(windowMenu, @"最小化", @selector(performMiniaturize:), @"m");
    tvuItem(windowMenu, @"缩放", @selector(performZoom:), @"");
    tvuItem(windowMenu, @"关闭", @selector(performClose:), @"w");
    windowItem.submenu = windowMenu;
    [NSApp setWindowsMenu:windowMenu];

    [NSApp setMainMenu:bar];
}

void tvuRun(const char *title, int width, int height, int minWidth, int minHeight, const char *html, int debug) {
    @autoreleasepool {
        tvuEnsureApplication();
        NSString *windowTitle = [NSString stringWithUTF8String:title];
        tvuBuildMenu(windowTitle);

        NSRect frame = NSMakeRect(0, 0, width, height);
        gWindow = [[NSWindow alloc] initWithContentRect:frame
                                              styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                                                        NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
                                                backing:NSBackingStoreBuffered
                                                  defer:NO];
        gWindow.title = windowTitle;
        gWindow.releasedWhenClosed = NO;
        gWindow.delegate = gDelegate;
        gWindow.contentMinSize = NSMakeSize(minWidth, minHeight);
        [gWindow center];
        // Remember the size and position between launches.
        [gWindow setFrameAutosaveName:@"TVUMainWindow"];

        WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
        WKUserContentController *content = [[WKUserContentController alloc] init];
        [content addScriptMessageHandler:gDelegate name:@"tvu"];
        WKUserScript *bridge = [[WKUserScript alloc]
              initWithSource:@"window.__tvuPost=function(s){window.webkit.messageHandlers.tvu.postMessage(s)};"
               injectionTime:WKUserScriptInjectionTimeAtDocumentStart
            forMainFrameOnly:YES];
        [content addUserScript:bridge];
        configuration.userContentController = content;
        if (debug) {
            [configuration.preferences setValue:@YES forKey:@"developerExtrasEnabled"];
        }

        gWebView = [[TVUWebView alloc] initWithFrame:frame configuration:configuration];
        gWebView.navigationDelegate = gDelegate;
        gWebView.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
        // Let the window colour show until the page paints: no white flash in
        // dark mode.
        [gWebView setValue:@NO forKey:@"drawsBackground"];
        gWindow.contentView = gWebView;
        [gWebView loadHTMLString:[NSString stringWithUTF8String:html] baseURL:nil];

        [gWindow makeKeyAndOrderFront:nil];
        [NSApp activateIgnoringOtherApps:YES];
        tvuGoReady();
        [NSApp run];

        gWebView = nil;
        gWindow = nil;
    }
}

void tvuEval(const char *script) {
    NSString *source = [NSString stringWithUTF8String:script];
    if (source == nil) {
        return;
    }
    dispatch_async(dispatch_get_main_queue(), ^{
        [gWebView evaluateJavaScript:source completionHandler:nil];
    });
}

void tvuClose(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        gForceClose = YES;
        [gWindow close];
    });
}

char *tvuPickFolder(const char *title) {
    NSString *message = [NSString stringWithUTF8String:title];
    __block char *result = NULL;
    void (^choose)(void) = ^{
        NSOpenPanel *panel = [NSOpenPanel openPanel];
        panel.canChooseFiles = NO;
        panel.canChooseDirectories = YES;
        panel.allowsMultipleSelection = NO;
        panel.canCreateDirectories = YES;
        panel.message = message;
        panel.prompt = @"选择";
        if ([panel runModal] == NSModalResponseOK && panel.URL.path != nil) {
            result = strdup(panel.URL.path.fileSystemRepresentation);
        }
    };
    if ([NSThread isMainThread]) {
        choose();
    } else {
        dispatch_sync(dispatch_get_main_queue(), choose);
    }
    return result;
}

char *tvuPickFiles(const char *title) {
    NSString *message = [NSString stringWithUTF8String:title];
    __block char *result = NULL;
    void (^choose)(void) = ^{
        NSOpenPanel *panel = [NSOpenPanel openPanel];
        panel.canChooseFiles = YES;
        panel.canChooseDirectories = NO;
        panel.allowsMultipleSelection = YES;
        panel.message = message;
        panel.prompt = @"添加";
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        // The replacement needs the UniformTypeIdentifiers framework; the
        // extension list is all this chooser needs.
        panel.allowedFileTypes = @[ @"mp4" ];
#pragma clang diagnostic pop
        if ([panel runModal] == NSModalResponseOK) {
            NSString *json = tvuPathsJSON(panel.URLs);
            if (json != nil) {
                result = strdup(json.UTF8String);
            }
        }
    };
    if ([NSThread isMainThread]) {
        choose();
    } else {
        dispatch_sync(dispatch_get_main_queue(), choose);
    }
    return result;
}

void tvuAlert(const char *title, const char *message) {
    @autoreleasepool {
        tvuEnsureApplication();
        NSAlert *alert = [[NSAlert alloc] init];
        alert.messageText = [NSString stringWithUTF8String:title];
        alert.informativeText = [NSString stringWithUTF8String:message];
        alert.alertStyle = NSAlertStyleCritical;
        [alert addButtonWithTitle:@"关闭"];
        [NSApp activateIgnoringOtherApps:YES];
        [alert runModal];
    }
}
