//go:build darwin
// +build darwin

package darwin

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework Cocoa -framework WebKit
#import <Foundation/Foundation.h>
#import "Application.h"
#import "CustomProtocol.h"
#import "WailsContext.h"

#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/url"
	"os"
	"unsafe"

	"github.com/wailsapp/wails/v2/internal/conv"
	"github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/assetserver/webview"

	"github.com/wailsapp/wails/v2/internal/binding"
	"github.com/wailsapp/wails/v2/internal/frontend"
	"github.com/wailsapp/wails/v2/internal/frontend/runtime"
	"github.com/wailsapp/wails/v2/internal/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
)

const startURL = "wails://wails/"

var (
	messageBuffer        = make(chan string, 100)
	requestBuffer        = make(chan webview.Request, 100)
	callbackBuffer       = make(chan uint, 10)
	openFilepathBuffer   = make(chan string, 100)
	openUrlBuffer        = make(chan string, 100)
	secondInstanceBuffer = make(chan options.SecondInstanceData, 1)
)

type Frontend struct {
	// Context
	ctx context.Context

	frontendOptions *options.App
	logger          *logger.Logger
	debug           bool
	devtoolsEnabled bool

	// Keep single instance lock file, so that it will not be GC and lock will exist while app is running
	singleInstanceLockFile *os.File

	// Assets
	assets   *assetserver.AssetServer
	startURL *url.URL

	// main window handle
	mainWindow *Window
	bindings   *binding.Bindings
	dispatcher frontend.Dispatcher
}

func (f *Frontend) RunMainLoop() {
	C.RunMainLoop()
}

func (f *Frontend) WindowClose() {
	C.ReleaseContext(f.mainWindow.context)
}

func NewFrontend(ctx context.Context, appoptions *options.App, myLogger *logger.Logger, appBindings *binding.Bindings, dispatcher frontend.Dispatcher) *Frontend {
	result := &Frontend{
		frontendOptions: appoptions,
		logger:          myLogger,
		bindings:        appBindings,
		dispatcher:      dispatcher,
		ctx:             ctx,
	}
	result.startURL, _ = url.Parse(startURL)

	// this should be initialized as early as possible to handle first instance launch
	C.StartCustomProtocolHandler()

	if _starturl, _ := ctx.Value("starturl").(*url.URL); _starturl != nil {
		result.startURL = _starturl
	} else {
		if port, _ := ctx.Value("assetserverport").(string); port != "" {
			result.startURL.Host = net.JoinHostPort(result.startURL.Host+".localhost", port)
		}

		var bindings string
		var err error
		if _obfuscated, _ := ctx.Value("obfuscated").(bool); !_obfuscated {
			bindings, err = appBindings.ToJSON()
			if err != nil {
				log.Fatal(err)
			}
		} else {
			appBindings.DB().UpdateObfuscatedCallMap()
		}

		assets, err := assetserver.NewAssetServerMainPage(bindings, appoptions, ctx.Value("assetdir") != nil, myLogger, runtime.RuntimeAssetsBundle)
		if err != nil {
			log.Fatal(err)
		}
		assets.ExpectedWebViewHost = result.startURL.Host
		result.assets = assets

		go result.startRequestProcessor()
	}

	go result.startMessageProcessor()
	go result.startCallbackProcessor()
	go result.startFileOpenProcessor()
	go result.startUrlOpenProcessor()
	go result.startSecondInstanceProcessor()

	return result
}

func (f *Frontend) startFileOpenProcessor() {
	for filePath := range openFilepathBuffer {
		f.ProcessOpenFileEvent(filePath)
	}
}

func (f *Frontend) startUrlOpenProcessor() {
	for url := range openUrlBuffer {
		f.ProcessOpenUrlEvent(url)
	}
}

func (f *Frontend) startSecondInstanceProcessor() {
	for secondInstanceData := range secondInstanceBuffer {
		if f.frontendOptions.SingleInstanceLock != nil &&
			f.frontendOptions.SingleInstanceLock.OnSecondInstanceLaunch != nil {
			f.frontendOptions.SingleInstanceLock.OnSecondInstanceLaunch(secondInstanceData)
		}
	}
}

func (f *Frontend) startMessageProcessor() {
	for message := range messageBuffer {
		f.processMessage(message)
	}
}

func (f *Frontend) startRequestProcessor() {
	for request := range requestBuffer {
		f.assets.ServeWebViewRequest(request)
	}
}

func (f *Frontend) startCallbackProcessor() {
	for callback := range callbackBuffer {
		err := f.handleCallback(callback)
		if err != nil {
			println(err.Error())
		}
	}
}

func (f *Frontend) WindowReload() {
	f.ExecJS("runtime.WindowReload();")
}

func (f *Frontend) WindowReloadApp() {
	f.ExecJS(fmt.Sprintf("window.location.href = '%s';", f.startURL))
}

func (f *Frontend) WindowSetSystemDefaultTheme() {
}

func (f *Frontend) WindowSetLightTheme() {
}

func (f *Frontend) WindowSetDarkTheme() {
}

func (f *Frontend) Run(ctx context.Context) error {
	f.ctx = ctx

	if f.frontendOptions.SingleInstanceLock != nil {
		f.singleInstanceLockFile = SetupSingleInstance(f.frontendOptions.SingleInstanceLock.UniqueId)
	}

	_debug := ctx.Value("debug")
	_devtoolsEnabled := ctx.Value("devtoolsEnabled")

	if _debug != nil {
		f.debug = _debug.(bool)
	}
	if _devtoolsEnabled != nil {
		f.devtoolsEnabled = _devtoolsEnabled.(bool)
	}

	mainWindow := NewWindow(f.frontendOptions, f.debug, f.devtoolsEnabled)
	f.mainWindow = mainWindow
	f.mainWindow.Center()

	go func() {
		if f.frontendOptions.OnStartup != nil {
			f.frontendOptions.OnStartup(f.ctx)
		}
	}()
	mainWindow.Run(f.startURL.String())
	return nil
}

func (f *Frontend) WindowCenter() {
	f.mainWindow.Center()
}

func (f *Frontend) WindowSetAlwaysOnTop(onTop bool) {
	f.mainWindow.SetAlwaysOnTop(onTop)
}

func (f *Frontend) WindowSetPosition(x, y int) {
	f.mainWindow.SetPosition(x, y)
}

func (f *Frontend) WindowGetPosition() (int, int) {
	return f.mainWindow.GetPosition()
}

func (f *Frontend) WindowSetSize(width, height int) {
	f.mainWindow.SetSize(width, height)
}

func (f *Frontend) WindowGetSize() (int, int) {
	return f.mainWindow.Size()
}

func (f *Frontend) WindowSetTitle(title string) {
	f.mainWindow.SetTitle(title)
}

func (f *Frontend) WindowFullscreen() {
	f.mainWindow.Fullscreen()
}

func (f *Frontend) WindowUnfullscreen() {
	f.mainWindow.UnFullscreen()
}

func (f *Frontend) WindowShow() {
	f.mainWindow.Show()
}

func (f *Frontend) WindowHide() {
	f.mainWindow.Hide()
}

func (f *Frontend) Show() {
	f.mainWindow.ShowApplication()
}

func (f *Frontend) Hide() {
	f.mainWindow.HideApplication()
}

func (f *Frontend) WindowMaximise() {
	f.mainWindow.Maximise()
}

func (f *Frontend) WindowToggleMaximise() {
	f.mainWindow.ToggleMaximise()
}

func (f *Frontend) WindowUnmaximise() {
	f.mainWindow.UnMaximise()
}

func (f *Frontend) WindowMinimise() {
	f.mainWindow.Minimise()
}

func (f *Frontend) WindowUnminimise() {
	f.mainWindow.UnMinimise()
}

func (f *Frontend) WindowSetMinSize(width int, height int) {
	f.mainWindow.SetMinSize(width, height)
}

func (f *Frontend) WindowSetMaxSize(width int, height int) {
	f.mainWindow.SetMaxSize(width, height)
}

func (f *Frontend) WindowSetBackgroundColour(col *options.RGBA) {
	if col == nil {
		return
	}
	f.mainWindow.SetBackgroundColour(col.R, col.G, col.B, col.A)
}

func (f *Frontend) ScreenGetAll() ([]frontend.Screen, error) {
	return GetAllScreens(f.mainWindow.context)
}

func (f *Frontend) WindowIsMaximised() bool {
	return f.mainWindow.IsMaximised()
}

func (f *Frontend) WindowIsMinimised() bool {
	return f.mainWindow.IsMinimised()
}

func (f *Frontend) WindowIsNormal() bool {
	return f.mainWindow.IsNormal()
}

func (f *Frontend) WindowIsFullscreen() bool {
	return f.mainWindow.IsFullScreen()
}

func (f *Frontend) Quit() {
	if f.frontendOptions.OnBeforeClose != nil {
		go func() {
			if !f.frontendOptions.OnBeforeClose(f.ctx) {
				f.mainWindow.Quit()
			}
		}()
		return
	}
	f.mainWindow.Quit()
}

func (f *Frontend) WindowPrint() {
	f.mainWindow.Print()
}

type EventNotify struct {
	Name string        `json:"name"`
	Data []interface{} `json:"data"`
}

func (f *Frontend) Notify(name string, data ...interface{}) {
	notification := EventNotify{
		Name: name,
		Data: data,
	}
	payload, err := json.Marshal(notification)
	if err != nil {
		f.logger.Error(err.Error())
		return
	}
	f.ExecJS(`window.wails.EventsNotify('` + template.JSEscapeString(string(payload)) + `');`)
}

func (f *Frontend) processMessage(message string) {
	if message == "DomReady" {
		if f.frontendOptions.OnDomReady != nil {
			f.frontendOptions.OnDomReady(f.ctx)
		}
		return
	}

	if message == "runtime:ready" {
		cmd := fmt.Sprintf("window.wails.setCSSDragProperties('%s', '%s');", f.frontendOptions.CSSDragProperty, f.frontendOptions.CSSDragValue)
		f.ExecJS(cmd)

		if f.frontendOptions.DragAndDrop != nil && f.frontendOptions.DragAndDrop.EnableFileDrop {
			f.ExecJS("window.wails.flags.enableWailsDragAndDrop = true;")
		}

		return
	}

	if message == "wails:openInspector" {
		showInspector(f.mainWindow.context)
		return
	}

	//if strings.HasPrefix(message, "systemevent:") {
	//	f.processSystemEvent(message)
	//	return
	//}

	go func() {
		result, err := f.dispatcher.ProcessMessage(message, f)
		if err != nil {
			f.logger.Error(err.Error())
			f.Callback(result)
			return
		}
		if result == "" {
			return
		}

		switch result[0] {
		case 'c':
			// Callback from a method call
			f.Callback(result[1:])
		default:
			f.logger.Info("Unknown message returned from dispatcher: %+v", result)
		}
	}()
}

func (f *Frontend) ProcessOpenFileEvent(filePath string) {
	if f.frontendOptions.Mac != nil && f.frontendOptions.Mac.OnFileOpen != nil {
		f.frontendOptions.Mac.OnFileOpen(filePath)
	}
}

func (f *Frontend) ProcessOpenUrlEvent(url string) {
	if f.frontendOptions.Mac != nil && f.frontendOptions.Mac.OnUrlOpen != nil {
		f.frontendOptions.Mac.OnUrlOpen(url)
	}
}

func (f *Frontend) Callback(message string) {
	escaped, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	f.ExecJS(`window.wails.Callback(` + conv.BytesToString(escaped) + `);`)
}

func (f *Frontend) ExecJS(js string) {
	f.mainWindow.ExecJS(js)
}

//func (f *Frontend) processSystemEvent(message string) {
//	sl := strings.Split(message, ":")
//	if len(sl) != 2 {
//		f.logger.Error("Invalid system message: %s", message)
//		return
//	}
//	switch sl[1] {
//	case "fullscreen":
//		f.mainWindow.DisableSizeConstraints()
//	case "unfullscreen":
//		f.mainWindow.EnableSizeConstraints()
//	default:
//		f.logger.Error("Unknown system message: %s", message)
//	}
//}

//export processMessage
func processMessage(message *C.char) {
	goMessage := C.GoString(message)
	messageBuffer <- goMessage
}

//export processCallback
func processCallback(callbackID uint) {
	callbackBuffer <- callbackID
}

//export processURLRequest
func processURLRequest(_ unsafe.Pointer, wkURLSchemeTask unsafe.Pointer) {
	requestBuffer <- webview.NewRequest(wkURLSchemeTask)
}

//export HandleOpenFile
func HandleOpenFile(filePath *C.char) {
	goFilepath := C.GoString(filePath)
	openFilepathBuffer <- goFilepath
}

//export HandleCustomProtocol
func HandleCustomProtocol(url *C.char) {
	goUrl := C.GoString(url)
	openUrlBuffer <- goUrl
}
