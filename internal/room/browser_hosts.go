package room

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	dockerFilters "github.com/docker/docker/api/types/filters"
	dockerMount "github.com/docker/docker/api/types/mount"
	dockerNetwork "github.com/docker/docker/api/types/network"
	dockerStrslice "github.com/docker/docker/api/types/strslice"

	"github.com/m1k1o/neko-rooms/internal/types"
)

const (
	browserHostX11Path     = "/tmp/.X11-unix"
	browserHostRuntimePath = "/tmp/neko-runtime"
	browserHostColumns     = 3
	browserHostRows        = 2
	browserHostGutter      = 8
	browserWindowAttempts  = 300
)

var browserRoomManagedEnvKeys = []string{
	"NEKO_RUNTIME_XSERVER_ENABLED",
	"NEKO_RUNTIME_PULSEAUDIO_ENABLED",
	"NEKO_RUNTIME_APP_ENABLED",
	"NEKO_DESKTOP_INPUT_ENABLED",
	"NEKO_DESKTOP_WINDOW_ID",
	"NEKO_DESKTOP_WINDOW_X",
	"NEKO_DESKTOP_WINDOW_Y",
	"NEKO_DESKTOP_WINDOW_WIDTH",
	"NEKO_DESKTOP_WINDOW_HEIGHT",
	"NEKO_CAPTURE_VIDEO_WINDOW_ID",
	"NEKO_CAPTURE_VIDEO_WINDOW_X",
	"NEKO_CAPTURE_VIDEO_WINDOW_Y",
	"NEKO_CAPTURE_VIDEO_WINDOW_WIDTH",
	"NEKO_CAPTURE_VIDEO_WINDOW_HEIGHT",
}

type browserProfileMount struct {
	SharedPath    string
	ProfilePath   string
	ExternalPath  string
	ContainerPath string
}

type browserHost struct {
	Key           string
	ContainerID   string
	WindowID      uint64
	WindowSlot    int
	WindowX       int
	WindowY       int
	WindowWidth   int
	WindowHeight  int
	SharedPath    string
	ProfilePath   string
	X11Volume     string
	RuntimeVolume string
}

type browserHostSpec struct {
	ImageID       string               `json:"image_id"`
	APIVersion    int                  `json:"api_version"`
	ProfilePath   string               `json:"profile_path"`
	Screen        string               `json:"screen"`
	BrowserPolicy *types.BrowserPolicy `json:"browser_policy"`
	Envs          map[string]string    `json:"envs"`
	DNS           []string             `json:"dns"`
	Resources     types.RoomResources  `json:"resources"`
}

type browserWindow struct {
	ID      uint64
	PID     int
	Process string
	X       int
	Y       int
	Width   int
	Height  int
}

func browserHostLabels(host *browserHost) *BrowserHostLabels {
	if host == nil {
		return nil
	}
	return &BrowserHostLabels{
		Key:          host.Key,
		ContainerID:  host.ContainerID,
		WindowID:     host.WindowID,
		WindowSlot:   host.WindowSlot,
		WindowX:      host.WindowX,
		WindowY:      host.WindowY,
		WindowWidth:  host.WindowWidth,
		WindowHeight: host.WindowHeight,
		SharedPath:   host.SharedPath,
		ProfilePath:  host.ProfilePath,
	}
}

func setContainerEnv(env []string, key, value string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, prefix+value)
}

func browserHostKey(sharedPath string) string {
	digest := sha256.Sum256([]byte(path.Clean(sharedPath)))
	return hex.EncodeToString(digest[:])
}

func browserHostSpecHash(settings types.RoomSettings, profile browserProfileMount, imageID string) (string, error) {
	width, height, rate, err := parseBrowserHostScreen(settings.Screen)
	if err != nil {
		return "", err
	}
	envs := make(map[string]string, len(settings.Envs))
	for key, value := range settings.Envs {
		envs[key] = value
	}
	dns := append([]string{}, settings.DNS...)
	resources := settings.Resources
	resources.Gpus = append([]string{}, settings.Resources.Gpus...)
	resources.Devices = append([]string{}, settings.Resources.Devices...)
	var browserPolicy *types.BrowserPolicy
	if settings.BrowserPolicy != nil {
		copy := *settings.BrowserPolicy
		copy.Path = path.Clean(copy.Path)
		copy.Profile = path.Clean(copy.Profile)
		copy.Content.Extensions = append([]types.BrowserPolicyExtension{}, copy.Content.Extensions...)
		browserPolicy = &copy
	}
	spec := browserHostSpec{
		ImageID:       imageID,
		APIVersion:    settings.ApiVersion,
		ProfilePath:   path.Clean(profile.ProfilePath),
		Screen:        fmt.Sprintf("%dx%d@%d", width, height, rate),
		BrowserPolicy: browserPolicy,
		Envs:          envs,
		DNS:           dns,
		Resources:     resources,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode browser host configuration: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (manager *RoomManagerCtx) browserProfile(settings *types.RoomSettings) *browserProfileMount {
	if settings.BrowserPolicy == nil ||
		!settings.BrowserPolicy.Content.PersistentData ||
		settings.BrowserPolicy.Profile == "" {
		return nil
	}

	for _, mount := range settings.Mounts {
		if mount.Type != types.MountShared ||
			path.Clean(mount.ContainerPath) != path.Clean(settings.BrowserPolicy.Profile) {
			continue
		}

		return &browserProfileMount{
			SharedPath:    mount.HostPath,
			ProfilePath:   settings.BrowserPolicy.Profile,
			ExternalPath:  path.Join(manager.config.StorageExternal, sharedStoragePath, mount.HostPath),
			ContainerPath: mount.ContainerPath,
		}
	}

	return nil
}

func (manager *RoomManagerCtx) browserHostVolumeNames(key string) (string, string) {
	prefix := fmt.Sprintf("%s-browser-%s", manager.config.InstanceName, key[:12])
	return prefix + "-x11", prefix + "-runtime"
}

func (manager *RoomManagerCtx) findBrowserHost(ctx context.Context, key string) (*dockerContainer.Summary, error) {
	filters := dockerFilters.NewArgs(
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.instance=%s", manager.config.InstanceName)),
		dockerFilters.Arg("label", "m1k1o.neko_rooms.browser_host=true"),
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.browser_host.key=%s", key)),
	)
	containers, err := manager.client.ContainerList(ctx, dockerContainer.ListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, nil
	}
	if len(containers) != 1 {
		return nil, fmt.Errorf("multiple browser hosts found for profile %s", key)
	}
	return &containers[0], nil
}

func (manager *RoomManagerCtx) ensureBrowserHost(
	ctx context.Context,
	settings types.RoomSettings,
	profile browserProfileMount,
	imageID string,
	policySource string,
	deviceRequests []dockerContainer.DeviceRequest,
	devices []dockerContainer.DeviceMapping,
) (*browserHost, error) {
	manager.browserHostsMu.Lock()
	defer manager.browserHostsMu.Unlock()

	key := browserHostKey(profile.SharedPath)
	specHash, err := browserHostSpecHash(settings, profile, imageID)
	if err != nil {
		return nil, err
	}
	lockCreated, err := manager.acquireBrowserHostLock(ctx, key)
	if err != nil {
		return nil, err
	}
	x11Volume, runtimeVolume := manager.browserHostVolumeNames(key)

	container, err := manager.findBrowserHost(ctx, key)
	if err != nil {
		if lockCreated {
			_ = manager.releaseBrowserHostLock(ctx, key)
		}
		return nil, err
	}
	createdHost := false
	if container == nil {
		container, err = manager.createBrowserHost(
			ctx,
			key,
			specHash,
			settings,
			profile,
			policySource,
			x11Volume,
			runtimeVolume,
			deviceRequests,
			devices,
		)
		if err != nil {
			if lockCreated {
				_ = manager.releaseBrowserHostLock(ctx, key)
			}
			return nil, err
		}
		createdHost = true
	} else if container.Labels["m1k1o.neko_rooms.browser_host.spec_hash"] != specHash {
		return nil, fmt.Errorf(
			"shared browser profile %q is already active with an incompatible image, screen, policy, environment, DNS, or resource configuration",
			profile.SharedPath,
		)
	}

	if container.State != "running" {
		if err := manager.client.ContainerStart(ctx, container.ID, dockerContainer.StartOptions{}); err != nil {
			if createdHost {
				_ = manager.cleanupBrowserHostLocked(ctx, &BrowserHostLabels{Key: key, ContainerID: container.ID})
			}
			return nil, fmt.Errorf("start browser host: %w", err)
		}
	}
	if err := manager.recoverBrowserHostLocked(ctx, container.ID, key, false); err != nil {
		if createdHost {
			_ = manager.cleanupBrowserHostLocked(ctx, &BrowserHostLabels{Key: key, ContainerID: container.ID})
		}
		return nil, fmt.Errorf("recover browser host: %w", err)
	}

	window, windowSlot, err := manager.allocateBrowserWindow(ctx, container.ID, key, settings.Screen)
	if err != nil {
		if createdHost {
			_ = manager.cleanupBrowserHostLocked(ctx, &BrowserHostLabels{Key: key, ContainerID: container.ID})
		}
		return nil, err
	}

	return &browserHost{
		Key:           key,
		ContainerID:   container.ID,
		WindowID:      window.ID,
		WindowSlot:    windowSlot,
		WindowX:       window.X,
		WindowY:       window.Y,
		WindowWidth:   window.Width,
		WindowHeight:  window.Height,
		SharedPath:    profile.SharedPath,
		ProfilePath:   profile.ProfilePath,
		X11Volume:     x11Volume,
		RuntimeVolume: runtimeVolume,
	}, nil
}

func (manager *RoomManagerCtx) createBrowserHost(
	ctx context.Context,
	key string,
	specHash string,
	settings types.RoomSettings,
	profile browserProfileMount,
	policySource string,
	x11Volume string,
	runtimeVolume string,
	deviceRequests []dockerContainer.DeviceRequest,
	devices []dockerContainer.DeviceMapping,
) (*dockerContainer.Summary, error) {
	env, err := settings.ToEnv(manager.config, types.PortSettings{
		FrontendPort: frontendPort,
		EprMin:       manager.config.EprMin,
		EprMax:       manager.config.EprMin,
	})
	if err != nil {
		return nil, err
	}
	env = setContainerEnv(env, "NEKO_RUNTIME_XSERVER_ENABLED", "true")
	env = setContainerEnv(env, "NEKO_RUNTIME_PULSEAUDIO_ENABLED", "true")
	env = setContainerEnv(env, "NEKO_RUNTIME_APP_ENABLED", "true")
	windowWidth, windowHeight, refreshRate, err := parseBrowserHostScreen(settings.Screen)
	if err != nil {
		return nil, err
	}
	hostScreen := fmt.Sprintf(
		"%dx%d@%d",
		(windowWidth+browserHostGutter)*browserHostColumns,
		(windowHeight+browserHostGutter)*browserHostRows,
		refreshRate,
	)
	env = setContainerEnv(env, "NEKO_DESKTOP_SCREEN", hostScreen)
	env = setContainerEnv(env, "NEKO_SCREEN", hostScreen)

	mounts := []dockerMount.Mount{
		browserHostBind(profile.ExternalPath, profile.ContainerPath, false),
		browserHostVolume(x11Volume, browserHostX11Path),
		browserHostVolume(runtimeVolume, browserHostRuntimePath),
	}
	if settings.BrowserPolicy != nil && policySource != "" {
		mounts = append(mounts, browserHostBind(policySource, settings.BrowserPolicy.Path, true))
	}

	name := fmt.Sprintf("%s-browser-%s", manager.config.InstanceName, key[:12])
	labels := map[string]string{
		"m1k1o.neko_rooms.instance":               manager.config.InstanceName,
		"m1k1o.neko_rooms.browser_host":           "true",
		"m1k1o.neko_rooms.browser_host.key":       key,
		"m1k1o.neko_rooms.browser_host.spec_hash": specHash,
		"m1k1o.neko_rooms.neko_image":             settings.NekoImage,
	}

	config := &dockerContainer.Config{
		Hostname: name,
		Env:      env,
		Image:    settings.NekoImage,
		Labels:   labels,
	}
	hostConfig := &dockerContainer.HostConfig{
		RestartPolicy: dockerContainer.RestartPolicy{Name: "unless-stopped"},
		IpcMode:       dockerContainer.IpcMode("shareable"),
		CapAdd:        dockerStrslice.StrSlice{"SYS_ADMIN"},
		ShmSize:       settings.Resources.ShmSize,
		Mounts:        mounts,
		DNS:           settings.DNS,
		Privileged:    slices.Contains(manager.config.NekoPrivilegedImages, settings.NekoImage),
		Resources: dockerContainer.Resources{
			CPUShares:      settings.Resources.CPUShares,
			NanoCPUs:       settings.Resources.NanoCPUs,
			Memory:         settings.Resources.Memory,
			DeviceRequests: deviceRequests,
			Devices:        devices,
		},
	}
	networking := &dockerNetwork.NetworkingConfig{EndpointsConfig: map[string]*dockerNetwork.EndpointSettings{
		manager.config.InstanceNetwork: {},
	}}

	created, err := manager.client.ContainerCreate(ctx, config, hostConfig, networking, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create browser host: %w", err)
	}

	return &dockerContainer.Summary{
		ID:     created.ID,
		State:  "created",
		Labels: labels,
	}, nil
}

func browserHostBind(source, target string, readOnly bool) dockerMount.Mount {
	return dockerMount.Mount{
		Type:     dockerMount.TypeBind,
		Source:   source,
		Target:   target,
		ReadOnly: readOnly,
		BindOptions: &dockerMount.BindOptions{
			Propagation:  dockerMount.PropagationRPrivate,
			NonRecursive: false,
		},
	}
}

func browserHostVolume(source, target string) dockerMount.Mount {
	return dockerMount.Mount{
		Type:   dockerMount.TypeVolume,
		Source: source,
		Target: target,
	}
}

func (manager *RoomManagerCtx) visibleWindows(ctx context.Context, containerID string) ([]browserWindow, error) {
	output, err := manager.containerExec(ctx, containerID, []string{
		"bash", "-lc", `
for window_id in $(xdotool search --onlyvisible --name '.+' 2>/dev/null); do
  eval "$(xdotool getwindowgeometry --shell "$window_id")"
  window_pid=$(xdotool getwindowpid "$window_id" 2>/dev/null || printf '0')
  window_process=$(cat "/proc/$window_pid/comm" 2>/dev/null | tr -d '\n' || true)
  [ -n "$window_process" ] || window_process=unknown
  printf '%s %s %s %s %s %s %s\n' "$WINDOW" "$window_pid" "$window_process" "$X" "$Y" "$WIDTH" "$HEIGHT"
done
true`,
	})
	if err != nil {
		return nil, err
	}

	windows := []browserWindow{}
	fields := strings.Fields(output)
	for len(fields) >= 7 {
		id, idErr := strconv.ParseUint(fields[0], 10, 64)
		pid, pidErr := strconv.Atoi(fields[1])
		process := fields[2]
		x, xErr := strconv.Atoi(fields[3])
		y, yErr := strconv.Atoi(fields[4])
		width, widthErr := strconv.Atoi(fields[5])
		height, heightErr := strconv.Atoi(fields[6])
		if idErr == nil && pidErr == nil && xErr == nil && yErr == nil && widthErr == nil && heightErr == nil && id != 0 {
			windows = append(windows, browserWindow{ID: id, PID: pid, Process: process, X: x, Y: y, Width: width, Height: height})
		}
		fields = fields[7:]
	}
	sort.Slice(windows, func(i, j int) bool {
		leftArea := windows[i].Width * windows[i].Height
		rightArea := windows[j].Width * windows[j].Height
		if leftArea != rightArea {
			return leftArea > rightArea
		}
		return windows[i].ID < windows[j].ID
	})
	return windows, nil
}

func (manager *RoomManagerCtx) browserWindows(ctx context.Context, containerID string) ([]browserWindow, error) {
	windows, err := manager.visibleWindows(ctx, containerID)
	if err != nil {
		return nil, err
	}
	return filterBrowserProcessWindows(windows), nil
}

func filterBrowserProcessWindows(windows []browserWindow) []browserWindow {
	hasKnownBrowser := false
	for _, window := range windows {
		if isBrowserProcess(window.Process) {
			hasKnownBrowser = true
			break
		}
	}
	areas := map[int]int{}
	for _, window := range windows {
		if hasKnownBrowser && !isBrowserProcess(window.Process) {
			continue
		}
		if window.PID > 0 {
			areas[window.PID] += window.Width * window.Height
		}
	}
	bestPID := 0
	bestArea := 0
	for pid, area := range areas {
		if area > bestArea {
			bestPID = pid
			bestArea = area
		}
	}
	if bestPID == 0 {
		return windows
	}
	filtered := make([]browserWindow, 0, len(windows))
	for _, window := range windows {
		if window.PID == bestPID && (!hasKnownBrowser || isBrowserProcess(window.Process)) {
			filtered = append(filtered, window)
		}
	}
	return filtered
}

func isBrowserProcess(process string) bool {
	process = strings.ToLower(process)
	for _, name := range []string{"firefox", "chrome", "chromium", "brave", "msedge", "microsoft-edge"} {
		if strings.Contains(process, name) {
			return true
		}
	}
	return false
}

func (manager *RoomManagerCtx) closeNonBrowserWindows(ctx context.Context, containerID string) error {
	for attempt := 0; attempt < 10; attempt++ {
		allWindows, err := manager.visibleWindows(ctx, containerID)
		if err != nil {
			return err
		}
		browserWindows := filterBrowserProcessWindows(allWindows)
		browserIDs := make(map[uint64]struct{}, len(browserWindows))
		for _, window := range browserWindows {
			browserIDs[window.ID] = struct{}{}
		}
		nonBrowserWindows := 0
		for _, window := range allWindows {
			if _, browserWindow := browserIDs[window.ID]; browserWindow {
				continue
			}
			nonBrowserWindows++
			_, _ = manager.containerExec(ctx, containerID, []string{
				"xdotool", "windowclose", strconv.FormatUint(window.ID, 10),
			})
		}
		if nonBrowserWindows == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out closing non-browser windows")
}

func (manager *RoomManagerCtx) assignedBrowserWindowLabels(ctx context.Context, key string) ([]*BrowserHostLabels, error) {
	containers, err := manager.browserHostRoomContainers(ctx, key)
	if err != nil {
		return nil, err
	}

	assigned := make([]*BrowserHostLabels, 0, len(containers))
	for _, container := range containers {
		labels, err := manager.extractLabels(container.Labels)
		if err != nil {
			return nil, err
		}
		assigned = append(assigned, labels.BrowserHost)
	}
	return assigned, nil
}

func (manager *RoomManagerCtx) browserHostRoomContainers(ctx context.Context, key string) ([]dockerContainer.Summary, error) {
	filters := dockerFilters.NewArgs(
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.instance=%s", manager.config.InstanceName)),
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.browser_host.key=%s", key)),
	)
	containers, err := manager.client.ContainerList(ctx, dockerContainer.ListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}

	rooms := make([]dockerContainer.Summary, 0, len(containers))
	for _, container := range containers {
		if container.Labels["m1k1o.neko_rooms.browser_host"] == "true" {
			continue
		}
		rooms = append(rooms, container)
	}
	sort.Slice(rooms, func(i, j int) bool {
		left, _ := strconv.Atoi(rooms[i].Labels["m1k1o.neko_rooms.browser_host.window_slot"])
		right, _ := strconv.Atoi(rooms[j].Labels["m1k1o.neko_rooms.browser_host.window_slot"])
		return left < right
	})
	return rooms, nil
}

func (manager *RoomManagerCtx) allocateBrowserWindow(ctx context.Context, containerID, key, screen string) (browserWindow, int, error) {
	assigned, err := manager.assignedBrowserWindowLabels(ctx, key)
	if err != nil {
		return browserWindow{}, 0, err
	}
	windowWidth, windowHeight, _, err := parseBrowserHostScreen(screen)
	if err != nil {
		return browserWindow{}, 0, err
	}

	occupiedSlots := map[int]struct{}{}
	for _, host := range assigned {
		occupiedSlots[host.WindowSlot] = struct{}{}
	}
	windowSlot := -1
	for slot := 0; slot < browserHostColumns*browserHostRows; slot++ {
		if _, occupied := occupiedSlots[slot]; !occupied {
			windowSlot = slot
			break
		}
	}
	if windowSlot == -1 {
		return browserWindow{}, 0, fmt.Errorf("browser host has reached its %d-window capacity", browserHostColumns*browserHostRows)
	}

	for attempt := 0; attempt < browserWindowAttempts; attempt++ {
		windows, err := manager.browserWindows(ctx, containerID)
		if err != nil {
			return browserWindow{}, 0, err
		}
		for _, window := range windows {
			assignedByGeometry := false
			for _, host := range assigned {
				if _, ok := browserWindowForRegion([]browserWindow{window}, host, map[uint64]struct{}{}); ok {
					assignedByGeometry = true
					break
				}
			}
			if !assignedByGeometry {
				if err := manager.configureBrowserWindow(ctx, containerID, window.ID, windowSlot, windowWidth, windowHeight); err != nil {
					return browserWindow{}, 0, err
				}
				configured, err := manager.waitForBrowserWindowGeometry(ctx, containerID, window.ID, windowWidth, windowHeight)
				if err != nil {
					return browserWindow{}, 0, err
				}
				return configured, windowSlot, nil
			}
		}

		if len(windows) > 0 && attempt%10 == 0 {
			window := strconv.FormatUint(windows[0].ID, 10)
			command := fmt.Sprintf(
				"xdotool windowactivate --sync %s; sleep 0.2; xdotool key --clearmodifiers ctrl+n",
				window,
			)
			if _, err := manager.containerExec(ctx, containerID, []string{
				"bash", "-lc", command,
			}); err != nil {
				return browserWindow{}, 0, fmt.Errorf("open browser window: %w", err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	return browserWindow{}, 0, fmt.Errorf("timed out waiting for a browser window")
}

func parseBrowserHostScreen(screen string) (width, height, rate int, err error) {
	if _, err = fmt.Sscanf(screen, "%dx%d@%d", &width, &height, &rate); err != nil || width <= 0 || height <= 0 || rate <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid browser host screen %q", screen)
	}
	return width, height, rate, nil
}

func (manager *RoomManagerCtx) waitForBrowserWindowGeometry(
	ctx context.Context,
	containerID string,
	windowID uint64,
	width int,
	height int,
) (browserWindow, error) {
	for attempt := 0; attempt < 20; attempt++ {
		windows, err := manager.browserWindows(ctx, containerID)
		if err == nil {
			for _, window := range windows {
				if window.ID == windowID && window.Width == width && window.Height == height {
					return window, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return browserWindow{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return browserWindow{}, fmt.Errorf("timed out waiting for browser window %d geometry", windowID)
}

func (manager *RoomManagerCtx) configureBrowserWindow(
	ctx context.Context,
	containerID string,
	windowID uint64,
	slot int,
	width int,
	height int,
) error {
	x := (slot % browserHostColumns) * (width + browserHostGutter)
	y := (slot / browserHostColumns) * (height + browserHostGutter)
	window := strconv.FormatUint(windowID, 10)
	hexWindow := fmt.Sprintf("0x%x", windowID)
	command := fmt.Sprintf(
		"wmctrl -i -r %s -b remove,maximized_vert,maximized_horz; sleep 0.1; "+
			"xdotool windowsize --sync %s %d %d; xdotool windowmove --sync %s %d %d",
		hexWindow,
		window,
		width,
		height,
		window,
		x,
		y,
	)
	if _, err := manager.containerExec(ctx, containerID, []string{"bash", "-lc", command}); err != nil {
		return fmt.Errorf("position browser window: %w", err)
	}
	return nil
}

func (manager *RoomManagerCtx) restoreBrowserWindow(
	ctx context.Context,
	containerID string,
	windowID uint64,
	host *BrowserHostLabels,
) error {
	targetX := host.WindowX
	targetY := host.WindowY
	for attempt := 0; attempt < 4; attempt++ {
		window := strconv.FormatUint(windowID, 10)
		hexWindow := fmt.Sprintf("0x%x", windowID)
		command := fmt.Sprintf(
			"wmctrl -i -r %s -b remove,maximized_vert,maximized_horz; sleep 0.1; "+
				"xdotool windowsize %s %d %d; xdotool windowmove %s %d %d; sleep 0.2",
			hexWindow,
			window,
			host.WindowWidth,
			host.WindowHeight,
			window,
			targetX,
			targetY,
		)
		if _, err := manager.containerExec(ctx, containerID, []string{"bash", "-lc", command}); err != nil {
			return fmt.Errorf("restore browser window position: %w", err)
		}
		geometry, err := manager.waitForBrowserWindowGeometry(
			ctx,
			containerID,
			windowID,
			host.WindowWidth,
			host.WindowHeight,
		)
		if err != nil {
			return err
		}
		if geometry.X == host.WindowX && geometry.Y == host.WindowY {
			return nil
		}
		targetX += host.WindowX - geometry.X
		targetY += host.WindowY - geometry.Y
	}
	return fmt.Errorf("browser window %d did not return to its assigned region", windowID)
}

func (manager *RoomManagerCtx) resetBrowserWindow(ctx context.Context, containerID string, windowID uint64) error {
	command := fmt.Sprintf(
		"xdotool windowactivate --sync %d; sleep 0.2; xdotool key --clearmodifiers ctrl+l; sleep 0.1; "+
			"xdotool type --delay 1 about:blank; xdotool key Return; sleep 0.2",
		windowID,
	)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := manager.containerExec(ctx, containerID, []string{"bash", "-lc", command}); err != nil {
			return fmt.Errorf("reset browser window: %w", err)
		}
	}
	return nil
}

func browserWindowForRegion(windows []browserWindow, host *BrowserHostLabels, used map[uint64]struct{}) (browserWindow, bool) {
	for _, window := range windows {
		if _, ok := used[window.ID]; ok {
			continue
		}
		centerX := window.X + window.Width/2
		centerY := window.Y + window.Height/2
		if centerX >= host.WindowX && centerX < host.WindowX+host.WindowWidth &&
			centerY >= host.WindowY && centerY < host.WindowY+host.WindowHeight {
			return window, true
		}
	}
	return browserWindow{}, false
}

func (manager *RoomManagerCtx) containerIPCNamespace(ctx context.Context, containerID string) (string, error) {
	output, err := manager.containerExec(ctx, containerID, []string{"readlink", "/proc/1/ns/ipc"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (manager *RoomManagerCtx) browserHostNeedsRecovery(
	ctx context.Context,
	containerID string,
	windows []browserWindow,
	rooms []dockerContainer.Summary,
) (bool, error) {
	hostIPC, err := manager.containerIPCNamespace(ctx, containerID)
	if err != nil {
		return false, err
	}
	used := map[uint64]struct{}{}
	for _, room := range rooms {
		labels, err := manager.extractLabels(room.Labels)
		if err != nil {
			return false, err
		}
		window, ok := browserWindowForRegion(windows, labels.BrowserHost, used)
		if !ok {
			return true, nil
		}
		used[window.ID] = struct{}{}
		if room.State == "running" {
			roomIPC, err := manager.containerIPCNamespace(ctx, room.ID)
			if err != nil || roomIPC != hostIPC {
				return true, nil
			}
		}
	}
	return false, nil
}

func (manager *RoomManagerCtx) waitForStableBrowserWindows(
	ctx context.Context,
	containerID string,
	minimumWindowArea int,
) ([]browserWindow, error) {
	lastSignature := ""
	stablePolls := 0
	for attempt := 0; attempt < 150; attempt++ {
		windows, err := manager.browserWindows(ctx, containerID)
		if err == nil {
			hasExpectedWindow := false
			ids := make([]string, 0, len(windows))
			for _, window := range windows {
				ids = append(ids, strconv.FormatUint(window.ID, 10))
				if window.Width*window.Height >= minimumWindowArea {
					hasExpectedWindow = true
				}
			}
			sort.Strings(ids)
			signature := strings.Join(ids, ",")
			if hasExpectedWindow && signature != "" && signature == lastSignature {
				stablePolls++
				if stablePolls >= 15 {
					return windows, nil
				}
			} else {
				lastSignature = signature
				stablePolls = 0
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("timed out waiting for browser windows to stabilize")
}

func (manager *RoomManagerCtx) recoverBrowserHost(ctx context.Context, containerID, key string, force bool) error {
	manager.browserHostsMu.Lock()
	defer manager.browserHostsMu.Unlock()
	return manager.recoverBrowserHostLocked(ctx, containerID, key, force)
}

func (manager *RoomManagerCtx) recoverBrowserHostLocked(ctx context.Context, containerID, key string, force bool) error {
	rooms, err := manager.browserHostRoomContainers(ctx, key)
	if err != nil || len(rooms) == 0 {
		return err
	}
	firstLabels, err := manager.extractLabels(rooms[0].Labels)
	if err != nil {
		return err
	}
	minimumWindowArea := firstLabels.BrowserHost.WindowWidth * firstLabels.BrowserHost.WindowHeight / 2
	windows, err := manager.waitForStableBrowserWindows(ctx, containerID, minimumWindowArea)
	if err != nil {
		return err
	}
	if err := manager.closeNonBrowserWindows(ctx, containerID); err != nil {
		return err
	}
	if !force {
		needsRecovery, err := manager.browserHostNeedsRecovery(ctx, containerID, windows, rooms)
		if err != nil || !needsRecovery {
			return err
		}
	}

	for len(windows) < len(rooms) {
		windowCount := len(windows)
		for attempt := 0; attempt < browserWindowAttempts && len(windows) == windowCount; attempt++ {
			if attempt%10 == 0 {
				command := fmt.Sprintf(
					"xdotool windowactivate --sync %d; sleep 0.1; xdotool key --clearmodifiers ctrl+n",
					windows[0].ID,
				)
				if _, err := manager.containerExec(ctx, containerID, []string{"bash", "-lc", command}); err != nil {
					return fmt.Errorf("open recovery browser window: %w", err)
				}
			}
			time.Sleep(100 * time.Millisecond)
			windows, err = manager.browserWindows(ctx, containerID)
			if err != nil {
				return err
			}
		}
		if len(windows) == windowCount {
			return fmt.Errorf("timed out opening recovery browser window")
		}
	}
	windows, err = manager.waitForStableBrowserWindows(ctx, containerID, minimumWindowArea)
	if err != nil {
		return err
	}
	sort.Slice(windows, func(i, j int) bool {
		leftArea := windows[i].Width * windows[i].Height
		rightArea := windows[j].Width * windows[j].Height
		if leftArea != rightArea {
			return leftArea > rightArea
		}
		return windows[i].ID < windows[j].ID
	})
	assignedWindows := windows[:len(rooms)]
	for _, window := range windows {
		if err := manager.resetBrowserWindow(ctx, containerID, window.ID); err != nil {
			return err
		}
	}

	occupiedSlots := map[int]struct{}{}
	var windowWidth, windowHeight int
	for index, room := range rooms {
		labels, err := manager.extractLabels(room.Labels)
		if err != nil {
			return err
		}
		host := labels.BrowserHost
		occupiedSlots[host.WindowSlot] = struct{}{}
		windowWidth = host.WindowWidth
		windowHeight = host.WindowHeight
		if err := manager.restoreBrowserWindow(
			ctx,
			containerID,
			assignedWindows[index].ID,
			host,
		); err != nil {
			return err
		}
	}
	freeSlots := make([]int, 0, browserHostColumns*browserHostRows-len(occupiedSlots))
	for slot := 0; slot < browserHostColumns*browserHostRows; slot++ {
		if _, occupied := occupiedSlots[slot]; !occupied {
			freeSlots = append(freeSlots, slot)
		}
	}
	for index, window := range windows[len(rooms):] {
		if index >= len(freeSlots) {
			_, _ = manager.containerExec(ctx, containerID, []string{
				"xdotool", "windowclose", strconv.FormatUint(window.ID, 10),
			})
			continue
		}
		if err := manager.configureBrowserWindow(
			ctx,
			containerID,
			window.ID,
			freeSlots[index],
			windowWidth,
			windowHeight,
		); err != nil {
			return err
		}
	}

	for _, room := range rooms {
		if room.State != "running" {
			continue
		}
		if err := manager.client.ContainerRestart(ctx, room.ID, dockerContainer.StopOptions{
			Signal:  "SIGTERM",
			Timeout: &manager.config.StopTimeoutSec,
		}); err != nil {
			return fmt.Errorf("restart room %s after browser host recovery: %w", room.ID[:12], err)
		}
	}
	manager.logger.Info().
		Str("browser_host", containerID[:12]).
		Int("rooms", len(rooms)).
		Msg("recovered browser room windows")
	return nil
}

func (manager *RoomManagerCtx) closeBrowserWindow(ctx context.Context, host *BrowserHostLabels) error {
	if host == nil || host.ContainerID == "" || host.WindowID == 0 {
		return nil
	}
	windowID := host.WindowID
	if host.WindowWidth > 0 && host.WindowHeight > 0 {
		windows, err := manager.browserWindows(ctx, host.ContainerID)
		if err == nil {
			if window, ok := browserWindowForRegion(windows, host, map[uint64]struct{}{}); ok {
				windowID = window.ID
			}
		}
	}
	if err := manager.resetBrowserWindow(ctx, host.ContainerID, windowID); err != nil {
		return err
	}
	_, err := manager.containerExec(ctx, host.ContainerID, []string{
		"xdotool", "windowclose", strconv.FormatUint(windowID, 10),
	})
	return err
}

func (manager *RoomManagerCtx) cleanupBrowserHost(ctx context.Context, host *BrowserHostLabels) error {
	if host == nil {
		return nil
	}

	manager.browserHostsMu.Lock()
	defer manager.browserHostsMu.Unlock()
	return manager.cleanupBrowserHostLocked(ctx, host)
}

func (manager *RoomManagerCtx) cleanupBrowserHostLocked(ctx context.Context, host *BrowserHostLabels) error {

	if err := manager.closeBrowserWindow(ctx, host); err != nil {
		manager.logger.Warn().Err(err).
			Str("browser_host", host.ContainerID).
			Uint64("window_id", host.WindowID).
			Msg("failed to close browser window")
	}

	filters := dockerFilters.NewArgs(
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.instance=%s", manager.config.InstanceName)),
		dockerFilters.Arg("label", fmt.Sprintf("m1k1o.neko_rooms.browser_host.key=%s", host.Key)),
	)
	containers, err := manager.client.ContainerList(ctx, dockerContainer.ListOptions{All: true, Filters: filters})
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.Labels["m1k1o.neko_rooms.browser_host"] != "true" {
			return nil
		}
	}

	browserContainer, err := manager.findBrowserHost(ctx, host.Key)
	if err != nil || browserContainer == nil {
		return err
	}
	if browserContainer.State == "running" || browserContainer.State == "paused" {
		if err := manager.client.ContainerStop(ctx, browserContainer.ID, dockerContainer.StopOptions{
			Signal:  "SIGTERM",
			Timeout: &manager.config.StopTimeoutSec,
		}); err != nil {
			return err
		}
	}
	if err := manager.client.ContainerRemove(ctx, browserContainer.ID, dockerContainer.RemoveOptions{Force: true}); err != nil {
		return err
	}

	x11Volume, runtimeVolume := manager.browserHostVolumeNames(host.Key)
	for _, volume := range []string{x11Volume, runtimeVolume} {
		if err := manager.client.VolumeRemove(ctx, volume, true); err != nil {
			return fmt.Errorf("remove browser host volume %q: %w", volume, err)
		}
	}
	return manager.releaseBrowserHostLock(ctx, host.Key)
}
