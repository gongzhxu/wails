package dev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/samber/lo"
	"github.com/wailsapp/wails/v2/cmd/wails/flags"
	"github.com/wailsapp/wails/v2/cmd/wails/internal/gomod"
	"github.com/wailsapp/wails/v2/cmd/wails/internal/logutils"
	"github.com/wailsapp/wails/v2/internal/conv"
	"golang.org/x/mod/semver"

	"github.com/wailsapp/wails/v2/pkg/commands/buildtags"

	"github.com/google/shlex"

	"github.com/pkg/browser"

	"github.com/fsnotify/fsnotify"
	"github.com/wailsapp/wails/v2/internal/fs"
	"github.com/wailsapp/wails/v2/internal/process"
	"github.com/wailsapp/wails/v2/pkg/clilogger"
	"github.com/wailsapp/wails/v2/pkg/commands/build"
)

const (
	viteMinVersion = "v3.0.0"
)

func sliceToMap(input []string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, value := range input {
		result[value] = struct{}{}
	}
	return result
}

// Application runs the application in dev mode
func Application(f *flags.Dev, logger *clilogger.CLILogger) error {
	cwd := lo.Must(os.Getwd())

	// Update go.mod to use current wails version
	err := gomod.SyncGoMod(logger, !f.NoSyncGoMod)
	if err != nil {
		return err
	}

	if !f.SkipModTidy {
		// Run go mod tidy to ensure we're up-to-date
		err = runCommand(cwd, false, f.Compiler, "mod", "tidy")
		if err != nil {
			return err
		}
	}

	buildOptions := f.GenerateBuildOptions()
	buildOptions.Logger = logger

	userTags, err := buildtags.Parse(f.Tags)
	if err != nil {
		return err
	}

	buildOptions.UserTags = userTags

	projectConfig := f.ProjectConfig()

	// Setup signal handler
	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, os.Interrupt, syscall.SIGTERM)
	exitCodeChannel := make(chan int, 1)

	// Build the frontend if requested, but ignore building the application itself.
	ignoreFrontend := buildOptions.IgnoreFrontend
	if !ignoreFrontend {
		buildOptions.IgnoreApplication = true
		if _, err := build.Build(buildOptions); err != nil {
			return err
		}
		buildOptions.IgnoreApplication = false
	}

	legacyUseDevServerInsteadofCustomScheme := false
	// frontend:dev:watcher command.
	frontendDevAutoDiscovery := projectConfig.IsFrontendDevServerURLAutoDiscovery()
	if command := projectConfig.DevWatcherCommand; command != "" {
		closer, devServerURL, devServerViteVersion, err := runFrontendDevWatcherCommand(projectConfig.GetFrontendDir(), command, frontendDevAutoDiscovery)
		if err != nil {
			return err
		}
		if devServerURL != "" {
			projectConfig.FrontendDevServerURL = devServerURL
			f.FrontendDevServerURL = devServerURL
		}
		defer closer()

		if devServerViteVersion != "" && semver.Compare(devServerViteVersion, viteMinVersion) < 0 {
			logutils.LogRed("Please upgrade your Vite Server to at least '%s' future Wails versions will require at least Vite '%s'", viteMinVersion, viteMinVersion)
			time.Sleep(3 * time.Second)
			legacyUseDevServerInsteadofCustomScheme = true
		}
	} else if frontendDevAutoDiscovery {
		return fmt.Errorf("unable to auto discover frontend:dev:serverUrl without a frontend:dev:watcher command, please either set frontend:dev:watcher or remove the auto discovery from frontend:dev:serverUrl")
	}

	// Do initial build but only for the application.
	logger.Println("Building application for development...")
	buildOptions.IgnoreFrontend = true
	debugBinaryProcess, appBinary, err := restartApp(buildOptions, nil, f, exitCodeChannel, legacyUseDevServerInsteadofCustomScheme)
	buildOptions.IgnoreFrontend = ignoreFrontend || f.FrontendDevServerURL != ""
	if err != nil {
		return err
	}
	defer func() {
		if err := killProcessAndCleanupBinary(debugBinaryProcess, appBinary); err != nil {
			logutils.LogDarkYellow("Unable to kill process and cleanup binary: %s", err)
		}
	}()

	// open browser
	if f.Browser {
		err = browser.OpenURL(f.DevServerURL().String())
		if err != nil {
			return err
		}
	}

	logutils.LogGreen("Using DevServer URL: %s", f.DevServerURL())
	if f.FrontendDevServerURL != "" {
		logutils.LogGreen("Using Frontend DevServer URL: %s", f.FrontendDevServerURL)
	}
	logutils.LogGreen("Using reload debounce setting of %d milliseconds", f.Debounce)

	// Show dev server URL in terminal after 3 seconds
	go func() {
		time.Sleep(3 * time.Second)
		logutils.LogGreen("\n\nTo develop in the browser and call your bound Go methods from Javascript, navigate to: %s", f.DevServerURL())
	}()

	// Watch for changes and trigger restartApp()
	debugBinaryProcess, err = doWatcherLoop(cwd, projectConfig.ReloadDirectories, buildOptions, debugBinaryProcess, f, exitCodeChannel, quitChannel, f.DevServerURL(), legacyUseDevServerInsteadofCustomScheme)
	if err != nil {
		return err
	}

	// Kill the current program if running and remove dev binary
	if err := killProcessAndCleanupBinary(debugBinaryProcess, appBinary); err != nil {
		return err
	}

	// Reset the process and the binary so defer knows about it and is a nop.
	debugBinaryProcess = nil
	appBinary = ""

	logutils.LogGreen("Development mode exited")

	return nil
}

func killProcessAndCleanupBinary(process *process.Process, binary string) error {
	if process != nil && process.Running {
		if err := process.Kill(); err != nil {
			return err
		}
	}

	if binary != "" {
		err := os.Remove(binary)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func runCommand(dir string, exitOnError bool, command string, args ...string) error {
	logutils.LogGreen("Executing: " + command + " " + strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		println(string(output))
		println(err.Error())
		if exitOnError {
			os.Exit(1)
		}
		return err
	}
	return nil
}

// runFrontendDevWatcherCommand will run the `frontend:dev:watcher` command if it was given, ex- `npm run dev`
func runFrontendDevWatcherCommand(frontendDirectory string, devCommand string, discoverViteServerURL bool) (func(), string, string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	scanner := NewStdoutScanner()
	cmdSlice := strings.Split(devCommand, " ")
	cmd := exec.CommandContext(ctx, cmdSlice[0], cmdSlice[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = scanner
	cmd.Dir = frontendDirectory
	setParentGID(cmd)

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, "", "", fmt.Errorf("unable to start frontend DevWatcher: %w", err)
	}

	var viteServerURL string
	if discoverViteServerURL {
		select {
		case serverURL := <-scanner.ViteServerURLChan:
			viteServerURL = serverURL
		case <-time.After(time.Second * 10):
			cancel()
			return nil, "", "", errors.New("failed to find Vite server URL")
		}
	}

	viteVersion := ""
	select {
	case version := <-scanner.ViteServerVersionC:
		viteVersion = version

	case <-time.After(time.Second * 5):
		// That's fine, then most probably it was not vite that was running
	}

	logutils.LogGreen("Running frontend DevWatcher command: '%s'", devCommand)
	var wg sync.WaitGroup
	wg.Add(1)

	const (
		stateRunning   int32 = 0
		stateCanceling int32 = 1
		stateStopped   int32 = 2
	)
	state := stateRunning
	go func() {
		if err := cmd.Wait(); err != nil {
			wasRunning := atomic.CompareAndSwapInt32(&state, stateRunning, stateStopped)
			if err.Error() != "exit status 1" && wasRunning {
				logutils.LogRed("Error from DevWatcher '%s': %s", devCommand, err.Error())
			}
		}
		atomic.StoreInt32(&state, stateStopped)
		wg.Done()
	}()

	return func() {
		if atomic.CompareAndSwapInt32(&state, stateRunning, stateCanceling) {
			killProc(cmd, devCommand)
		}
		cancel()
		wg.Wait()
	}, viteServerURL, viteVersion, nil
}

// restartApp does the actual rebuilding of the application when files change
func restartApp(buildOptions *build.Options, debugBinaryProcess *process.Process, f *flags.Dev, exitCodeChannel chan int, legacyUseDevServerInsteadofCustomScheme bool) (*process.Process, string, error) {
	appBinary, err := build.Build(buildOptions)
	println()
	if err != nil {
		logutils.LogRed("Build error - " + err.Error())

		msg := "Continuing to run current version"
		if debugBinaryProcess == nil {
			msg = "No version running, build will be retriggered as soon as changes have been detected"
		}
		logutils.LogDarkYellow(msg)
		return nil, "", nil
	}

	// Kill existing binary if need be
	if debugBinaryProcess != nil {
		killError := debugBinaryProcess.Kill()

		if killError != nil {
			buildOptions.Logger.Fatal("Unable to kill debug binary (PID: %d)!", debugBinaryProcess.PID())
		}

		debugBinaryProcess = nil
	}

	// parse appargs if any
	args, err := shlex.Split(f.AppArgs)
	if err != nil {
		buildOptions.Logger.Fatal("Unable to parse appargs: %s", err.Error())
	}

	// Set environment variables accordingly
	os.Setenv("loglevel", f.LogLevel)
	os.Setenv("assetdir", f.AssetDir)
	os.Setenv("devserver", f.DevServer)
	os.Setenv("frontenddevserverurl", f.FrontendDevServerURL)

	// Start up new binary with correct args

	var command string
	if len(f.DlvFlag) == 0 {
		command = appBinary
	} else {
		command = "dlv"
		newArgs := append(strings.Split(f.DlvFlag, " "), "exec", appBinary)
		if len(args) > 0 {
			newArgs = append(newArgs, "--")
			args = append(newArgs, args...)
		}
		args = newArgs
	}

	logutils.LogGreen("Executing: " + command + " " + strings.Join(args, " "))
	newProcess := process.NewProcess(command, args...)
	err = newProcess.Start(exitCodeChannel)
	if err != nil {
		// Remove binary
		if fs.FileExists(appBinary) {
			deleteError := fs.DeleteFile(appBinary)
			if deleteError != nil {
				buildOptions.Logger.Fatal("Unable to delete app binary: " + appBinary)
			}
		}
		buildOptions.Logger.Fatal("Unable to start application: %s", err.Error())
	}

	return newProcess, appBinary, nil
}

// doWatcherLoop is the main watch loop that runs while dev is active
func doWatcherLoop(cwd string, reloadDirs string, buildOptions *build.Options, debugBinaryProcess *process.Process, f *flags.Dev, exitCodeChannel chan int, quitChannel chan os.Signal, devServerURL *url.URL, legacyUseDevServerInsteadofCustomScheme bool) (*process.Process, error) {
	// create the project files watcher
	watcher, err := initialiseWatcher(cwd, reloadDirs)
	if err != nil {
		logutils.LogRed("Unable to create filesystem watcher. Reloads will not occur.")
		return nil, err
	}

	defer func(watcher *fsnotify.Watcher) {
		err := watcher.Close()
		if err != nil {
			log.Fatal(err.Error())
		}
	}(watcher)

	logutils.LogGreen("Watching (sub)/directory: %s", cwd)

	// Main Loop
	extensionsThatTriggerARebuild := sliceToMap(strings.Split(f.Extensions, ","))
	var dirsThatTriggerAReload []string
	for _, dir := range strings.Split(f.ReloadDirs, ",") {
		if dir == "" {
			continue
		}
		thePath, err := filepath.Abs(dir)
		if err != nil {
			logutils.LogRed("Unable to expand reloadDir '%s': %s", dir, err)
			continue
		}
		dirsThatTriggerAReload = append(dirsThatTriggerAReload, thePath)
		err = watcher.Add(thePath)
		if err != nil {
			logutils.LogRed("Unable to watch path: %s due to error %v", thePath, err)
		} else {
			logutils.LogGreen("Watching (sub)/directory: %s", thePath)
		}
	}

	quit := false
	interval := time.Duration(f.Debounce) * time.Millisecond
	timer := time.NewTimer(interval)
	rebuild := false
	reload := false
	assetDir := ""
	changedPaths := map[string]struct{}{}

	// If we are using an external dev server, the reloading of the frontend part can be skipped or if the user requested it
	skipAssetsReload := f.FrontendDevServerURL != "" || f.NoReload

	assetDirURL := joinPath(devServerURL, "/wails/assetdir")
	reloadURL := joinPath(devServerURL, "/wails/reload")
	for !quit {
		// reload := false
		select {
		case exitCode := <-exitCodeChannel:
			if exitCode == 0 {
				quit = true
			}
		case err := <-watcher.Errors:
			logutils.LogDarkYellow(err.Error())
		case item := <-watcher.Events:
			isEligibleFile := func(fileName string) bool {
				// Iterate all file patterns
				ext := filepath.Ext(fileName)
				if ext != "" {
					ext = ext[1:]
					if _, exists := extensionsThatTriggerARebuild[ext]; exists {
						return true
					}
				}
				return false
			}

			// Handle write operations
			if item.Op&fsnotify.Write == fsnotify.Write {
				// Ignore directories
				itemName := item.Name
				if fs.DirExists(itemName) {
					continue
				}

				if isEligibleFile(itemName) {
					rebuild = true
					timer.Reset(interval)
					continue
				}

				for _, reloadDir := range dirsThatTriggerAReload {
					if strings.HasPrefix(itemName, reloadDir) {
						reload = true
						break
					}
				}

				if !reload {
					changedPaths[filepath.Dir(itemName)] = struct{}{}
				}

				timer.Reset(interval)
			}

			// Handle new fs entries that are created
			if item.Op&fsnotify.Create == fsnotify.Create {
				// If this is a folder, add it to our watch list
				if fs.DirExists(item.Name) {
					// node_modules is BANNED!
					if !strings.Contains(item.Name, "node_modules") {
						err := watcher.Add(item.Name)
						if err != nil {
							buildOptions.Logger.Fatal("%s", err.Error())
						}
						logutils.LogGreen("Added new directory to watcher: %s", item.Name)
					}
				} else if isEligibleFile(item.Name) {
					// Handle creation of new file.
					// Note: On some platforms an update to a file is represented as
					// REMOVE -> CREATE instead of WRITE, so this is not only new files
					// but also updates to existing files
					rebuild = true
					timer.Reset(interval)
					continue
				}
			}
		case <-timer.C:
			if rebuild {
				rebuild = false
				if f.NoGoRebuild {
					logutils.LogGreen("[Rebuild triggered] skipping due to flag -nogorebuild")
				} else {
					logutils.LogGreen("[Rebuild triggered] files updated")
					// Try and build the app

					newBinaryProcess, _, err := restartApp(buildOptions, debugBinaryProcess, f, exitCodeChannel, legacyUseDevServerInsteadofCustomScheme)
					if err != nil {
						logutils.LogRed("Error during build: %s", err.Error())
						continue
					}
					// If we have a new process, saveConfig it
					if newBinaryProcess != nil {
						debugBinaryProcess = newBinaryProcess
					}
				}
			}

			if !skipAssetsReload && len(changedPaths) != 0 {
				if assetDir == "" {
					resp, err := http.Get(assetDirURL)
					if err != nil {
						logutils.LogRed("Error during retrieving assetdir: %s", err.Error())
					} else {
						content, err := io.ReadAll(resp.Body)
						if err != nil {
							logutils.LogRed("Error reading assetdir from devserver: %s", err.Error())
						} else {
							assetDir = conv.BytesToString(content)
						}
						resp.Body.Close()
					}
				}

				if assetDir != "" {
					for thePath := range changedPaths {
						if strings.HasPrefix(thePath, assetDir) {
							reload = true
							break
						}
					}
				} else if len(dirsThatTriggerAReload) == 0 {
					logutils.LogRed("Reloading couldn't be triggered: Please specify -assetdir or -reloaddirs")
				}
			}
			if reload {
				reload = false
				_, err := http.Get(reloadURL)
				if err != nil {
					logutils.LogRed("Error during refresh: %s", err.Error())
				}
			}
			changedPaths = map[string]struct{}{}
		case <-quitChannel:
			logutils.LogGreen("\nCaught quit")
			quit = true
		}
	}
	return debugBinaryProcess, nil
}

func joinPath(url *url.URL, subPath string) string {
	u := *url
	u.Path = path.Join(u.Path, subPath)
	return u.String()
}
