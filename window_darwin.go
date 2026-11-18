//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework WebKit

#include <stdlib.h>
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

@interface DCAppDelegate : NSObject <NSApplicationDelegate, WKNavigationDelegate, WKScriptMessageHandler>
@property (strong) NSWindow *window;
@property (strong) WKWebView *webView;
@property (copy) NSString *url;
@end

@implementation DCAppDelegate

- (void)applicationDidFinishLaunching:(NSNotification *)note {
	NSRect frame = NSMakeRect(0, 0, 1380, 880);
	NSWindowStyleMask style = NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
		NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable;
	self.window = [[NSWindow alloc] initWithContentRect:frame styleMask:style
	                                            backing:NSBackingStoreBuffered defer:NO];
	self.window.title = @"Disk Check";
	self.window.titlebarAppearsTransparent = YES;
	self.window.titleVisibility = NSWindowTitleHidden;
	self.window.minSize = NSMakeSize(760, 520);
	self.window.releasedWhenClosed = NO;
	// Same color as the page header, so the title bar blends into it.
	self.window.backgroundColor = [NSColor colorWithName:nil dynamicProvider:^NSColor *(NSAppearance *a) {
		NSAppearanceName n = [a bestMatchFromAppearancesWithNames:@[NSAppearanceNameAqua, NSAppearanceNameDarkAqua]];
		return [n isEqualToString:NSAppearanceNameDarkAqua]
			? [NSColor colorWithSRGBRed:0x19/255.0 green:0x19/255.0 blue:0x1b/255.0 alpha:1]
			: [NSColor whiteColor];
	}];

	WKWebViewConfiguration *cfg = [WKWebViewConfiguration new];
	[cfg.userContentController addScriptMessageHandler:self name:@"pick"];
	self.webView = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
	self.webView.navigationDelegate = self;
	self.webView.allowsBackForwardNavigationGestures = YES; // two-finger swipe = back / up
	[self.webView setValue:@NO forKey:@"drawsBackground"];  // no white flash while loading
	self.window.contentView = self.webView;

	[self.webView loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:self.url]]];
	[self.window center];
	[self.window setFrameAutosaveName:@"DiskCheckMainWindow"];
	[self.window makeKeyAndOrderFront:nil];
	[NSApp activateIgnoringOtherApps:YES];
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)app {
	return YES;
}

// Only our local page may load inside the window; anything else goes to the default browser.
- (void)webView:(WKWebView *)wv decidePolicyForNavigationAction:(WKNavigationAction *)action
                                                decisionHandler:(void (^)(WKNavigationActionPolicy))handler {
	NSURL *u = action.request.URL;
	NSURL *home = [NSURL URLWithString:self.url];
	if ([u.host isEqualToString:home.host] && [u.port isEqual:home.port]) {
		handler(WKNavigationActionPolicyAllow);
		return;
	}
	if (u && ([u.scheme isEqualToString:@"http"] || [u.scheme isEqualToString:@"https"])) {
		[[NSWorkspace sharedWorkspace] openURL:u];
	}
	handler(WKNavigationActionPolicyCancel);
}

// JS: window.webkit.messageHandlers.pick.postMessage('') → native folder picker → window.dcPicked(path)
- (void)userContentController:(WKUserContentController *)ucc didReceiveScriptMessage:(WKScriptMessage *)msg {
	if (![msg.name isEqualToString:@"pick"]) return;
	NSOpenPanel *panel = [NSOpenPanel openPanel];
	panel.canChooseDirectories = YES;
	panel.canChooseFiles = NO;
	panel.allowsMultipleSelection = NO;
	panel.showsHiddenFiles = YES;
	panel.prompt = @"Scan";
	panel.message = @"Choose a folder to scan";
	[panel beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse r) {
		if (r != NSModalResponseOK || panel.URL == nil) return;
		NSData *json = [NSJSONSerialization dataWithJSONObject:@[panel.URL.path] options:0 error:nil];
		NSString *arr = [[NSString alloc] initWithData:json encoding:NSUTF8StringEncoding];
		NSString *js = [NSString stringWithFormat:@"window.dcPicked && window.dcPicked(%@[0])", arr];
		[self.webView evaluateJavaScript:js completionHandler:nil];
	}];
}

@end

static NSMenuItem *dcItem(NSString *title, SEL action, NSString *key) {
	return [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:key];
}

static void dcBuildMenu(void) {
	NSMenu *bar = [NSMenu new];

	NSMenu *app = [NSMenu new];
	[app addItem:dcItem(@"About Disk Check", @selector(orderFrontStandardAboutPanel:), @"")];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItem:dcItem(@"Hide Disk Check", @selector(hide:), @"h")];
	NSMenuItem *others = dcItem(@"Hide Others", @selector(hideOtherApplications:), @"h");
	others.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagOption;
	[app addItem:others];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItem:dcItem(@"Quit Disk Check", @selector(terminate:), @"q")];

	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
	[edit addItem:dcItem(@"Undo", @selector(undo:), @"z")];
	[edit addItem:dcItem(@"Redo", @selector(redo:), @"Z")];
	[edit addItem:[NSMenuItem separatorItem]];
	[edit addItem:dcItem(@"Cut", @selector(cut:), @"x")];
	[edit addItem:dcItem(@"Copy", @selector(copy:), @"c")];
	[edit addItem:dcItem(@"Paste", @selector(paste:), @"v")];
	[edit addItem:dcItem(@"Select All", @selector(selectAll:), @"a")];

	NSMenu *view = [[NSMenu alloc] initWithTitle:@"View"];
	[view addItem:dcItem(@"Reload", @selector(reload:), @"r")];
	[view addItem:[NSMenuItem separatorItem]];
	NSMenuItem *fs = dcItem(@"Enter Full Screen", @selector(toggleFullScreen:), @"f");
	fs.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagControl;
	[view addItem:fs];

	NSMenu *win = [[NSMenu alloc] initWithTitle:@"Window"];
	[win addItem:dcItem(@"Minimize", @selector(performMiniaturize:), @"m")];
	[win addItem:dcItem(@"Zoom", @selector(performZoom:), @"")];
	[win addItem:dcItem(@"Close", @selector(performClose:), @"w")];

	for (NSMenu *m in @[app, edit, view, win]) {
		NSMenuItem *holder = [NSMenuItem new];
		holder.submenu = m;
		[bar addItem:holder];
	}
	NSApp.mainMenu = bar;
	NSApp.windowsMenu = win;
}

static DCAppDelegate *dcDelegate; // NSApp.delegate is weak; keep it alive

static void dcRunApp(const char *url) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		dcBuildMenu();
		dcDelegate = [DCAppDelegate new];
		dcDelegate.url = [NSString stringWithUTF8String:url];
		NSApp.delegate = dcDelegate;
		[NSApp run];
	}
}
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// Cocoa must run on the main thread; locking here keeps main() on it.
func init() { runtime.LockOSThread() }

const haveWindow = true

// runWindow shows the UI in a native window and blocks until the app quits.
func runWindow(url string) {
	cs := C.CString(url)
	defer C.free(unsafe.Pointer(cs))
	C.dcRunApp(cs)
}
