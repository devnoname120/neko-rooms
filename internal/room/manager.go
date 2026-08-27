package room

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/opts"
	dockerContainer "github.com/docker/docker/api/types/container"
	dockerMount "github.com/docker/docker/api/types/mount"
	dockerNetwork "github.com/docker/docker/api/types/network"
	dockerStrslice "github.com/docker/docker/api/types/strslice"
	dockerClient "github.com/docker/docker/client"
	dockerNames "github.com/docker/docker/daemon/names"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/policies"
	"github.com/m1k1o/neko-rooms/internal/types"
	"github.com/m1k1o/neko-rooms/internal/utils"
)

const (
	frontendPort        = 8080
	templateStoragePath = "./templates"
	privateStoragePath  = "./rooms"
	sharedStoragePath   = "./shared"
	managedStorageUid   = 1000
	managedStorageGid   = 1000
)

func New(client *dockerClient.Client, config *config.Room) *RoomManagerCtx {
	logger := log.With().Str("module", "room").Logger()

	manager := &RoomManagerCtx{
		logger:              logger,
		config:              config,
		client:              client,
		browserReservations: make(map[string]map[int]browserWindow),
		browserHostLeases:   make(map[string]*os.File),
	}
	manager.events = newEvents(config, client, manager.recoverBrowserHost, manager.guardBrowserHosts)
	return manager
}

type RoomManagerCtx struct {
	logger zerolog.Logger
	config *config.Room
	client *dockerClient.Client
	events *events

	browserHostsMu sync.Mutex
	// Reservations bridge the gap between allocating a browser window and the
	// room container becoming visible through Docker. Without them, concurrent
	// Create calls can select the same slot and window.
	browserReservations map[string]map[int]browserWindow
	// The open file descriptors hold process-scoped leases in addition to the
	// persistent owner metadata stored on shared storage.
	browserHostLeases map[string]*os.File
}

func (manager *RoomManagerCtx) Config() types.RoomsConfig {
	return types.RoomsConfig{
		Connections:    manager.config.EprMax - manager.config.EprMin + 1,
		NekoImages:     manager.config.NekoImages,
		StorageEnabled: manager.config.StorageEnabled,
		UsesMux:        manager.config.Mux,
	}
}

func (manager *RoomManagerCtx) List(ctx context.Context, labels map[string]string) ([]types.RoomEntry, error) {
	containers, err := manager.listContainers(ctx, labels)
	if err != nil {
		return nil, err
	}

	result := make([]types.RoomEntry, 0, len(containers))
	for _, container := range containers {
		entry, err := manager.containerToEntry(container)
		if err != nil {
			return nil, err
		}

		result = append(result, *entry)
	}

	return result, nil
}

func (manager *RoomManagerCtx) ExportAsDockerCompose(ctx context.Context) ([]byte, error) {
	services := map[string]any{}

	dockerCompose := map[string]any{
		"version": "3.8",
		"networks": map[string]any{
			"default": map[string]any{
				"name":     manager.config.InstanceNetwork,
				"external": true,
			},
		},
		"services": services,
	}

	containers, err := manager.listContainers(ctx, nil)
	if err != nil {
		return nil, err
	}

	for _, container := range containers {
		containerJson, err := manager.inspectContainer(ctx, container.ID)
		if err != nil {
			return nil, err
		}

		labels, err := manager.extractLabels(containerJson.Config.Labels)
		if err != nil {
			return nil, err
		}
		if labels.BrowserHost != nil {
			return nil, fmt.Errorf("Docker Compose export is not supported while shared browser-window rooms exist")
		}

		containerName := containerJson.Name
		containerName = strings.TrimPrefix(containerName, "/")

		service := map[string]any{}
		services[containerName] = service

		service["image"] = labels.NekoImage
		service["container_name"] = containerName
		service["hostname"] = containerJson.Config.Hostname
		service["restart"] = containerJson.HostConfig.RestartPolicy.Name

		// privileged
		if containerJson.HostConfig.Privileged {
			service["privileged"] = true
		}

		// total shm memory usage
		service["shm_size"] = containerJson.HostConfig.ShmSize

		// capabilites
		capAdd := []string{}
		for _, cap := range containerJson.HostConfig.CapAdd {
			capAdd = append(capAdd, string(cap))
		}
		if len(capAdd) > 0 {
			service["cap_add"] = capAdd
		}

		// resources
		resources := map[string]any{}
		{
			limits := map[string]string{}
			// TODO: CPUShares
			if containerJson.HostConfig.NanoCPUs > 0 {
				limits["cpus"] = fmt.Sprintf("%f", float64(containerJson.HostConfig.NanoCPUs)/1000000000)
			}
			if containerJson.HostConfig.Memory > 0 {
				limits["memory"] = fmt.Sprintf("%dM", containerJson.HostConfig.Memory/1024/1024)
			}
			if len(limits) > 0 {
				resources["limits"] = limits
			}

			deviceRequests := []any{}
			for _, device := range containerJson.HostConfig.DeviceRequests {
				deviceRequests = append(deviceRequests, map[string]any{
					"driver":       device.Driver,
					"count":        device.Count,
					"capabilities": device.Capabilities,
				})
			}
			if len(deviceRequests) > 0 {
				resources["reservations"] = map[string]any{
					"devices": deviceRequests,
				}
			}
		}
		if len(resources) > 0 {
			service["deploy"] = map[string]any{
				"resources": resources,
			}
		}

		// hostname
		if containerJson.Config.Hostname != containerName {
			service["hostname"] = containerJson.Config.Hostname
		}

		// dns
		if len(containerJson.HostConfig.DNS) > 0 {
			service["dns"] = containerJson.HostConfig.DNS
		}

		// ports
		ports := []string{}
		for port, host := range containerJson.HostConfig.PortBindings {
			for _, binding := range host {
				ports = append(ports, fmt.Sprintf("%s:%s", binding.HostPort, port))
			}
		}
		if len(ports) > 0 {
			service["ports"] = ports
		}

		// environment variables
		if len(containerJson.Config.Env) > 0 {
			service["environment"] = containerJson.Config.Env
		}

		// volumes
		volumes := []string{}
		for _, mount := range container.Mounts {
			if !mount.RW {
				volumes = append(volumes, fmt.Sprintf("%s:%s:ro", mount.Source, mount.Destination))
			} else {
				volumes = append(volumes, fmt.Sprintf("%s:%s", mount.Source, mount.Destination))
			}
		}
		if len(volumes) > 0 {
			service["volumes"] = volumes
		}

		// devices
		devices := []string{}
		for _, device := range containerJson.HostConfig.Devices {
			devices = append(devices, fmt.Sprintf("%s:%s:%s", device.PathOnHost, device.PathInContainer, device.CgroupPermissions))
		}
		if len(devices) > 0 {
			service["devices"] = devices
		}

		// labels
		labelsArr := []string{}
		for key, val := range containerJson.Config.Labels {
			labelsArr = append(labelsArr, fmt.Sprintf("%s=%s", key, val))
		}
		if len(labelsArr) > 0 {
			service["labels"] = labelsArr
		}
	}

	return yaml.Marshal(dockerCompose)
}

func (manager *RoomManagerCtx) Create(ctx context.Context, settings types.RoomSettings) (string, error) {
	if settings.Name != "" && !dockerNames.RestrictedNamePattern.MatchString(settings.Name) {
		return "", fmt.Errorf("invalid container name, must match %s", dockerNames.RestrictedNameChars)
	}

	if !slices.Contains(manager.config.NekoImages, settings.NekoImage) {
		return "", fmt.Errorf("invalid neko image")
	}

	browserProfile := manager.browserProfile(&settings)
	if settings.BrowserPolicy != nil && !settings.BrowserPolicy.Content.PersistentData {
		for _, mount := range settings.Mounts {
			if mount.Type == types.MountShared &&
				filepath.Clean(mount.ContainerPath) == filepath.Clean(settings.BrowserPolicy.Profile) {
				return "", fmt.Errorf("a shared browser profile mount requires persistent browser data")
			}
		}
	}
	var browserImageID string
	if browserProfile != nil {
		if len(settings.Resources.Gpus) != 0 || len(settings.Resources.Devices) != 0 {
			return "", fmt.Errorf("GPU and host-device passthrough are not supported by shared browser windows")
		}
		if settings.Hostname != "" {
			return "", fmt.Errorf("custom hostnames are not supported by shared browser windows")
		}
		if settings.VideoPipeline != "" || settings.BroadcastPipeline != "" {
			return "", fmt.Errorf("custom video and broadcast pipelines are not supported by shared browser windows")
		}
		profileMounts := 0
		for _, mount := range settings.Mounts {
			if filepath.Clean(mount.ContainerPath) != filepath.Clean(browserProfile.ProfilePath) {
				return "", fmt.Errorf("additional mounts are not supported by shared browser windows")
			}
			profileMounts++
			if mount.Type != types.MountShared || filepath.Clean(mount.HostPath) != filepath.Clean(browserProfile.SharedPath) {
				return "", fmt.Errorf("shared browser profile path %q has a conflicting mount", browserProfile.ProfilePath)
			}
		}
		if profileMounts != 1 {
			return "", fmt.Errorf("shared browser profile path %q must have exactly one mount", browserProfile.ProfilePath)
		}
		inspect, err := manager.client.ImageInspect(ctx, settings.NekoImage)
		if err != nil {
			return "", err
		}
		if inspect.Config.Labels["net.m1k1o.neko.window-rooms"] != "1" {
			return "", fmt.Errorf("neko image %q does not support shared browser windows", settings.NekoImage)
		}
		if inspect.Config.Labels["net.m1k1o.neko.window-rooms-runtime"] != "1" {
			return "", fmt.Errorf("neko image %q uses an unsupported shared-window runtime flavor", settings.NekoImage)
		}
		browserImageID = inspect.ID
	}

	// if api version is not set, try to detect it
	if settings.ApiVersion == 0 {
		inspect, err := manager.client.ImageInspect(ctx, settings.NekoImage)
		if err != nil {
			return "", err
		}

		// based on image label (preferred)
		if val, ok := inspect.Config.Labels["net.m1k1o.neko.api-version"]; ok {
			var err error
			settings.ApiVersion, err = strconv.Atoi(val)
			if err != nil {
				return "", err
			}
		} else

		// based on opencontainers image url label
		if val, ok := inspect.Config.Labels["org.opencontainers.image.url"]; ok {
			// TODO: this should be removed in future, but since we have a lot of legacy images, we need to support it
			switch val {
			case "https://github.com/m1k1o/neko",
				"https://github.com/m1k1o/neko-apps":
				settings.ApiVersion = 2
			case "https://github.com/demodesk/neko":
				settings.ApiVersion = 3
			}
		}

		// still unable to detect api version
		if settings.ApiVersion == 0 {
			// TODO: this should be removed in future, but since we have a lot of v2 images, we need to support it
			log.Warn().Str("image", settings.NekoImage).Msg("unable to detect api version, fallback to v2")
			settings.ApiVersion = 2
		}
	}

	// TODO: Check if path name exists.
	roomName := settings.Name
	if roomName == "" {
		var err error
		roomName, err = utils.NewUID(8)
		if err != nil {
			return "", err
		}
	}

	containerName := manager.config.InstanceName + "-" + roomName

	//
	// Allocate ports
	//

	portsNeeded := settings.MaxConnections
	if manager.config.Mux {
		portsNeeded = 1
	}

	epr, err := manager.allocatePorts(ctx, portsNeeded)
	if err != nil {
		return "", err
	}

	portBindings := nat.PortMap{}
	for port := epr.Min; port <= epr.Max; port++ {
		portBindings[nat.Port(fmt.Sprintf("%d/udp", port))] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: fmt.Sprintf("%d", port),
			},
		}

		// expose TCP port as well when using mux
		if manager.config.Mux {
			portBindings[nat.Port(fmt.Sprintf("%d/tcp", port))] = []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: fmt.Sprintf("%d", port),
				},
			}
		}
	}

	exposedPorts := nat.PortSet{
		nat.Port(fmt.Sprintf("%d/tcp", frontendPort)): struct{}{},
	}

	for port := range portBindings {
		exposedPorts[port] = struct{}{}
	}

	//
	// Set internal labels
	//

	var browserPolicyLabels *BrowserPolicyLabels
	var browserPolicySource string
	if settings.BrowserPolicy != nil {
		browserPolicyLabels = &BrowserPolicyLabels{
			Type:    settings.BrowserPolicy.Type,
			Path:    settings.BrowserPolicy.Path,
			Profile: settings.BrowserPolicy.Profile,
		}
	}

	labels := manager.serializeLabels(RoomLabels{
		Name: roomName,
		Mux:  manager.config.Mux,
		Epr:  epr,

		NekoImage:  settings.NekoImage,
		ApiVersion: settings.ApiVersion,

		BrowserPolicy: browserPolicyLabels,
		UserDefined:   settings.Labels,
	})

	//
	// Set traefik labels
	//

	pathPrefix := path.Join("/", manager.config.PathPrefix, roomName)

	if t := manager.config.Traefik; t.Enabled {
		// create traefik rule
		traefikRule := "PathPrefix(`" + pathPrefix + "`)"
		if t.Domain != "" && t.Domain != "*" {
			// match *.domain.tld as subdomain
			if after, ok := strings.CutPrefix(t.Domain, "*."); ok {
				traefikRule = fmt.Sprintf(
					"Host(`%s.%s`)",
					roomName,
					after,
				)
			} else {
				traefikRule += " && Host(`" + t.Domain + "`)"
			}
		} else {
			traefikRule += " && HostRegexp(`{host:.+}`)"
		}

		labels["traefik.enable"] = "true"
		labels["traefik.http.services."+containerName+"-frontend.loadbalancer.server.port"] = fmt.Sprintf("%d", frontendPort)
		labels["traefik.http.routers."+containerName+".entrypoints"] = t.Entrypoint
		labels["traefik.http.routers."+containerName+".rule"] = traefikRule
		labels["traefik.http.middlewares."+containerName+"-rdr.redirectregex.regex"] = pathPrefix + "$$"
		labels["traefik.http.middlewares."+containerName+"-rdr.redirectregex.replacement"] = pathPrefix + "/"
		labels["traefik.http.middlewares."+containerName+"-prf.stripprefix.prefixes"] = pathPrefix + "/"
		labels["traefik.http.routers."+containerName+".middlewares"] = containerName + "-rdr," + containerName + "-prf"
		labels["traefik.http.routers."+containerName+".service"] = containerName + "-frontend"

		// optional HTTPS
		if t.Certresolver != "" {
			labels["traefik.http.routers."+containerName+".tls"] = "true"
			labels["traefik.http.routers."+containerName+".tls.certresolver"] = t.Certresolver
		}
	} else {
		labels["m1k1o.neko_rooms.proxy.enabled"] = "true"
		labels["m1k1o.neko_rooms.proxy.path"] = pathPrefix
		labels["m1k1o.neko_rooms.proxy.port"] = fmt.Sprintf("%d", frontendPort)
	}

	// add custom labels
	for _, label := range manager.config.Labels {
		// replace dynamic values in labels
		label = strings.ReplaceAll(label, "{containerName}", containerName)
		label = strings.ReplaceAll(label, "{roomName}", roomName)

		if t := manager.config.Traefik; t.Enabled {
			label = strings.ReplaceAll(label, "{traefikEntrypoint}", t.Entrypoint)
			label = strings.ReplaceAll(label, "{traefikCertresolver}", t.Certresolver)
		}

		v := strings.SplitN(label, "=", 2)
		if len(v) != 2 {
			manager.logger.Warn().Str("label", label).Msg("invalid custom label")
			continue
		}

		key, val := v[0], v[1]
		labels[key] = val
	}

	//
	// Set environment variables
	//

	env, err := settings.ToEnv(
		manager.config,
		types.PortSettings{
			FrontendPort: frontendPort,
			EprMin:       epr.Min,
			EprMax:       epr.Max,
		})
	if err != nil {
		return "", err
	}

	//
	// Set browser policies
	//

	if settings.BrowserPolicy != nil {
		if !manager.config.StorageEnabled {
			return "", fmt.Errorf("policies cannot be specified, because storage is disabled or unavailable")
		}

		policyJson, err := policies.Generate(settings.BrowserPolicy.Content, settings.BrowserPolicy.Type)
		if err != nil {
			return "", err
		}

		// create policy path (+ also get host path)
		policyPath := fmt.Sprintf("/%s-%s-policy.json", roomName, settings.BrowserPolicy.Type)
		templateInternalPath := path.Join(manager.config.StorageInternal, templateStoragePath)
		policyInternalPath := path.Join(templateInternalPath, policyPath)
		browserPolicySource = path.Join(manager.config.StorageExternal, templateStoragePath, policyPath)

		// create dir if does not exist
		if _, err := os.Stat(templateInternalPath); os.IsNotExist(err) {
			if err := os.MkdirAll(templateInternalPath, os.ModePerm); err != nil {
				return "", err
			}
		}

		// write policy to file
		if err := os.WriteFile(policyInternalPath, []byte(policyJson), 0644); err != nil {
			return "", err
		}

		// mount policy file
		settings.Mounts = append(settings.Mounts, types.RoomMount{
			Type:          types.MountTemplate,
			HostPath:      policyPath,
			ContainerPath: settings.BrowserPolicy.Path,
		})
	}

	//
	// Set container mounts
	//

	paths := map[string]bool{}
	mounts := []dockerMount.Mount{}
	for _, mount := range settings.Mounts {
		hostPath := filepath.Clean(mount.HostPath)
		containerPath := filepath.Clean(mount.ContainerPath)

		// ignore duplicates
		if _, ok := paths[containerPath]; ok {
			continue
		}

		readOnly := false

		if !filepath.IsAbs(hostPath) || !filepath.IsAbs(containerPath) {
			return "", fmt.Errorf("mount paths must be absolute")
		}
		isBrowserProfile := browserProfile != nil &&
			mount.Type == types.MountShared &&
			containerPath == path.Clean(browserProfile.ProfilePath)

		switch mount.Type {
		case types.MountPrivate, types.MountShared:
			if !manager.config.StorageEnabled {
				return "", fmt.Errorf("%s mounts cannot be specified, because storage is disabled or unavailable", mount.Type)
			}

			storagePath, ok := writableStoragePath(mount.Type, roomName, hostPath)
			if !ok {
				return "", fmt.Errorf("unknown writable mount type %q", mount.Type)
			}

			// ensure that target exists with correct permissions
			internalPath := path.Join(manager.config.StorageInternal, storagePath)
			if _, err := os.Stat(internalPath); os.IsNotExist(err) {
				if err := os.MkdirAll(internalPath, os.ModePerm); err != nil {
					return "", err
				}

				if err := utils.ChownR(internalPath, managedStorageUid, managedStorageGid); err != nil {
					return "", err
				}
			}

			// prefix host path
			hostPath = path.Join(manager.config.StorageExternal, storagePath)
		case types.MountTemplate:
			if !manager.config.StorageEnabled {
				return "", fmt.Errorf("template mounts cannot be specified, because storage is disabled or unavailable")
			}

			// readonly template data
			readOnly = true

			// prefix host path
			hostPath = path.Join(manager.config.StorageExternal, templateStoragePath, hostPath)
		case types.MountProtected, types.MountPublic:
			// readonly if mount type is protected
			readOnly = mount.Type == types.MountProtected

			// public whitelisted mounts
			var isAllowed = false
			for _, path := range manager.config.MountsWhitelist {
				if strings.HasPrefix(hostPath, path) {
					isAllowed = true
					break
				}
			}

			if !isAllowed {
				return "", fmt.Errorf("mount path is not whitelisted in config")
			}
		default:
			return "", fmt.Errorf("unknown mount type %q", mount.Type)
		}

		// A shared browser profile is mounted only in its browser-host
		// container. The room container attaches to one window from that host.
		if isBrowserProfile {
			browserProfile.ExternalPath = hostPath
			paths[containerPath] = true
			continue
		}

		mounts = append(mounts,
			dockerMount.Mount{
				Type:        dockerMount.TypeBind,
				Source:      hostPath,
				Target:      containerPath,
				ReadOnly:    readOnly,
				Consistency: dockerMount.ConsistencyDefault,

				BindOptions: &dockerMount.BindOptions{
					Propagation:  dockerMount.PropagationRPrivate,
					NonRecursive: false,
				},
			},
		)

		paths[containerPath] = true
	}

	//
	// Set container device requests
	//

	var deviceRequests []dockerContainer.DeviceRequest

	if len(settings.Resources.Gpus) > 0 {
		gpuOpts := opts.GpuOpts{}

		// convert to csv
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		if err := w.Write(settings.Resources.Gpus); err != nil {
			return "", err
		}
		w.Flush()

		// set GPU opts
		if err := gpuOpts.Set(buf.String()); err != nil {
			return "", err
		}

		deviceRequests = append(deviceRequests, gpuOpts.Value()...)
	}

	//
	// Set container devices
	//

	var devices []dockerContainer.DeviceMapping
	for _, device := range settings.Resources.Devices {
		devices = append(devices, dockerContainer.DeviceMapping{
			PathOnHost:        device,
			PathInContainer:   device,
			CgroupPermissions: "rwm",
		})
	}

	var host *browserHost
	if browserProfile != nil {
		host, err = manager.ensureBrowserHost(
			ctx,
			settings,
			*browserProfile,
			browserImageID,
			browserPolicySource,
			deviceRequests,
			devices,
		)
		if err != nil {
			return "", err
		}
		defer manager.releaseBrowserReservation(host)

		for _, target := range []string{browserHostX11Path, browserHostRuntimePath} {
			if paths[target] {
				manager.rollbackBrowserHost(host)
				return "", fmt.Errorf("mount path %q is reserved for shared browser windows", target)
			}
		}
		mounts = append(mounts,
			browserHostVolume(host.X11Volume, browserHostX11Path),
			browserHostVolume(host.RuntimeVolume, browserHostRuntimePath),
		)

		env = setContainerEnv(env, "NEKO_RUNTIME_XSERVER_ENABLED", "false")
		env = setContainerEnv(env, "NEKO_RUNTIME_PULSEAUDIO_ENABLED", "false")
		env = setContainerEnv(env, "NEKO_RUNTIME_APP_ENABLED", "false")
		env = setContainerEnv(env, "NEKO_RUNTIME_SERVER_ENABLED", "true")
		env = setContainerEnv(env, "NEKO_FILETRANSFER_ENABLED", "false")
		env = setContainerEnv(env, "NEKO_DESKTOP_UPLOAD_DROP", "false")
		env = setContainerEnv(env, "NEKO_DESKTOP_FILE_CHOOSER_DIALOG", "false")
		env = setContainerEnv(env, "NEKO_DESKTOP_INPUT_ENABLED", "false")
		windowWidth := host.WindowWidth
		windowHeight := host.WindowHeight
		windowX := host.WindowX
		windowY := host.WindowY
		env = setContainerEnv(env, "NEKO_DESKTOP_WINDOW_ID", "0")
		env = setContainerEnv(env, "NEKO_DESKTOP_WINDOW_X", strconv.Itoa(windowX))
		env = setContainerEnv(env, "NEKO_DESKTOP_WINDOW_Y", strconv.Itoa(windowY))
		env = setContainerEnv(env, "NEKO_DESKTOP_WINDOW_WIDTH", strconv.Itoa(windowWidth))
		env = setContainerEnv(env, "NEKO_DESKTOP_WINDOW_HEIGHT", strconv.Itoa(windowHeight))
		env = setContainerEnv(env, "NEKO_CAPTURE_VIDEO_WINDOW_ID", "0")
		env = setContainerEnv(env, "NEKO_CAPTURE_VIDEO_WINDOW_X", strconv.Itoa(windowX))
		env = setContainerEnv(env, "NEKO_CAPTURE_VIDEO_WINDOW_Y", strconv.Itoa(windowY))
		env = setContainerEnv(env, "NEKO_CAPTURE_VIDEO_WINDOW_WIDTH", strconv.Itoa(windowWidth))
		env = setContainerEnv(env, "NEKO_CAPTURE_VIDEO_WINDOW_HEIGHT", strconv.Itoa(windowHeight))

		labels["m1k1o.neko_rooms.browser_host.key"] = host.Key
		labels["m1k1o.neko_rooms.browser_host.container_id"] = host.ContainerID
		labels["m1k1o.neko_rooms.browser_host.window_id"] = strconv.FormatUint(host.WindowID, 10)
		labels["m1k1o.neko_rooms.browser_host.window_slot"] = strconv.Itoa(host.WindowSlot)
		labels["m1k1o.neko_rooms.browser_host.window_x"] = strconv.Itoa(host.WindowX)
		labels["m1k1o.neko_rooms.browser_host.window_y"] = strconv.Itoa(host.WindowY)
		labels["m1k1o.neko_rooms.browser_host.window_width"] = strconv.Itoa(host.WindowWidth)
		labels["m1k1o.neko_rooms.browser_host.window_height"] = strconv.Itoa(host.WindowHeight)
		labels["m1k1o.neko_rooms.browser_host.shared_path"] = host.SharedPath
		labels["m1k1o.neko_rooms.browser_host.profile_path"] = host.ProfilePath
	}

	//
	// Set container configs
	//

	hostname := containerName
	if settings.Hostname != "" {
		hostname = settings.Hostname
	}

	config := &dockerContainer.Config{
		// Hostname
		Hostname: hostname,
		// Domainname is preventing from running container on LXC (Proxmox)
		// https://www.gitmemory.com/issue/docker/for-linux/743/524569376
		// Domainname: containerName,
		// List of exposed ports
		ExposedPorts: exposedPorts,
		// List of environment variable to set in the container
		Env: env,
		// Name of the image as it was passed by the operator (e.g. could be symbolic)
		Image: settings.NekoImage,
		// List of labels set to this container
		Labels: labels,
	}

	hostConfig := &dockerContainer.HostConfig{
		// Port mapping between the exposed port (container) and the host
		PortBindings: portBindings,
		// Configuration of the logs for this container
		LogConfig: dockerContainer.LogConfig{
			Type:   "json-file",
			Config: map[string]string{},
		},
		// Restart policy to be used for the container
		RestartPolicy: dockerContainer.RestartPolicy{
			Name: "unless-stopped",
		},
		// List of kernel capabilities to add to the container
		CapAdd: dockerStrslice.StrSlice{
			"SYS_ADMIN",
		},
		// Total shm memory usage
		ShmSize: settings.Resources.ShmSize,
		// Mounts specs used by the container
		Mounts: mounts,
		// Resources contains container's resources (cgroups config, ulimits...)
		Resources: dockerContainer.Resources{
			CPUShares:      settings.Resources.CPUShares,
			NanoCPUs:       settings.Resources.NanoCPUs,
			Memory:         settings.Resources.Memory,
			DeviceRequests: deviceRequests,
			Devices:        devices,
		},
		// DNS
		DNS: settings.DNS,
		// Privileged
		Privileged: slices.Contains(manager.config.NekoPrivilegedImages, settings.NekoImage),
	}
	if host != nil {
		hostConfig.IpcMode = dockerContainer.IpcMode("container:" + host.ContainerID)
		// The room shares the browser host's /dev/shm through its IPC namespace.
		hostConfig.ShmSize = 0
	}

	networkingConfig := &dockerNetwork.NetworkingConfig{
		EndpointsConfig: map[string]*dockerNetwork.EndpointSettings{
			manager.config.InstanceNetwork: {},
		},
	}

	// Creating the actual container
	container, err := manager.client.ContainerCreate(
		ctx,
		config,
		hostConfig,
		networkingConfig,
		nil,
		containerName,
	)

	if err != nil {
		if host != nil {
			manager.rollbackBrowserHost(host)
		}
		return "", err
	}

	return container.ID[:12], nil
}

func (manager *RoomManagerCtx) rollbackBrowserHost(host *browserHost) {
	if host == nil {
		return
	}
	manager.releaseBrowserReservation(host)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := manager.cleanupBrowserHost(cleanupCtx, browserHostLabels(host)); err != nil {
		manager.logger.Warn().Err(err).
			Str("browser_host", host.ContainerID).
			Msg("failed to roll back browser host allocation")
	}
}

func (manager *RoomManagerCtx) GetEntry(ctx context.Context, id string) (*types.RoomEntry, error) {
	// we don't support id shorter than 12 chars
	// because they can be ambiguous
	if len(id) < 12 {
		return nil, types.ErrRoomNotFound
	}

	container, err := manager.containerById(ctx, id)
	if err != nil {
		return nil, err
	}

	return manager.containerToEntry(*container)
}

func (manager *RoomManagerCtx) GetEntryByName(ctx context.Context, name string) (*types.RoomEntry, error) {
	container, err := manager.containerByName(ctx, name)
	if err != nil {
		return nil, err
	}

	return manager.containerToEntry(*container)
}

func (manager *RoomManagerCtx) Remove(ctx context.Context, id string) error {
	container, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return err
	}
	labels, err := manager.extractLabels(container.Config.Labels)
	if err != nil {
		return err
	}

	// Stop the actual container
	err = manager.client.ContainerStop(ctx, id, dockerContainer.StopOptions{
		Signal:  "SIGTERM",
		Timeout: &manager.config.StopTimeoutSec,
	})

	if err != nil {
		return err
	}

	// Remove the actual container
	err = manager.client.ContainerRemove(ctx, id, dockerContainer.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil {
		return err
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return manager.cleanupBrowserHost(cleanupCtx, labels.BrowserHost)
}

func (manager *RoomManagerCtx) GetSettings(ctx context.Context, id string) (*types.RoomSettings, error) {
	container, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}

	labels, err := manager.extractLabels(container.Config.Labels)
	if err != nil {
		return nil, err
	}

	privateStorageRoot := path.Join(manager.config.StorageExternal, privateStoragePath, labels.Name)
	sharedStorageRoot := path.Join(manager.config.StorageExternal, sharedStoragePath)
	templateStorageRoot := path.Join(manager.config.StorageExternal, templateStoragePath)

	mounts := []types.RoomMount{}
	for _, mount := range container.Mounts {
		if labels.BrowserHost != nil &&
			(mount.Destination == browserHostX11Path || mount.Destination == browserHostRuntimePath) {
			continue
		}
		mountType := types.MountPublic
		hostPath := mount.Source

		if relativePath, ok := relativeMountPath(hostPath, privateStorageRoot); ok {
			mountType = types.MountPrivate
			hostPath = relativePath
		} else if relativePath, ok := relativeMountPath(hostPath, sharedStorageRoot); ok {
			mountType = types.MountShared
			hostPath = relativePath
		} else if relativePath, ok := relativeMountPath(hostPath, templateStorageRoot); ok {
			mountType = types.MountTemplate
			hostPath = relativePath
		} else if !mount.RW {
			mountType = types.MountProtected
		}

		mounts = append(mounts, types.RoomMount{
			Type:          mountType,
			HostPath:      hostPath,
			ContainerPath: mount.Destination,
		})
	}
	if labels.BrowserHost != nil {
		mounts = append(mounts, types.RoomMount{
			Type:          types.MountShared,
			HostPath:      labels.BrowserHost.SharedPath,
			ContainerPath: labels.BrowserHost.ProfilePath,
		})
	}

	var browserPolicy *types.BrowserPolicy
	if labels.BrowserPolicy != nil {
		browserPolicy = &types.BrowserPolicy{
			Type:    labels.BrowserPolicy.Type,
			Path:    labels.BrowserPolicy.Path,
			Profile: labels.BrowserPolicy.Profile,
		}

		var policyMount *types.RoomMount
		for _, mount := range mounts {
			if mount.ContainerPath == labels.BrowserPolicy.Path {
				policyMount = &mount
				break
			}
		}

		// TODO: Refactor.
		if policyMount != nil && policyMount.Type == types.MountTemplate {
			templateInternalPath := path.Join(manager.config.StorageInternal, templateStoragePath, policyMount.HostPath)
			if _, err := os.Stat(templateInternalPath); !os.IsNotExist(err) {
				if data, err := os.ReadFile(templateInternalPath); err == nil {
					if content, err := policies.Parse(string(data), labels.BrowserPolicy.Type); err == nil {
						browserPolicy.Content = *content
					}
				}
			}
		}
	}

	var roomResources types.RoomResources
	if container.HostConfig != nil {
		gpus := []string{}
		for _, req := range container.HostConfig.DeviceRequests {
			var isGpu bool
			var caps []string
			for _, cc := range req.Capabilities {
				for _, c := range cc {
					if c == "gpu" {
						isGpu = true
						continue
					}
					caps = append(caps, c)
				}
			}
			if !isGpu {
				continue
			}

			if req.Count > 1 {
				gpus = append(gpus, fmt.Sprintf("count=%d", req.Count))
			} else if req.Count == -1 {
				gpus = append(gpus, "all")
			}
			if req.Driver != "" {
				gpus = append(gpus, fmt.Sprintf("driver=%s", req.Driver))
			}
			if len(req.DeviceIDs) > 0 {
				gpus = append(gpus, fmt.Sprintf("device=%s", strings.Join(req.DeviceIDs, ",")))
			}
			if len(caps) > 0 {
				gpus = append(gpus, fmt.Sprintf("capabilities=%s", strings.Join(caps, ",")))
			}
			var opts []string
			for key, val := range req.Options {
				opts = append(opts, fmt.Sprintf("%s=%s", key, val))
			}
			if len(opts) > 0 {
				gpus = append(gpus, fmt.Sprintf("options=%s", strings.Join(opts, ",")))
			}
		}

		devices := []string{}
		for _, dev := range container.HostConfig.Devices {
			// TODO: dev.CgroupPermissions
			if dev.PathOnHost == dev.PathInContainer {
				devices = append(devices, dev.PathOnHost)
			} else {
				devices = append(devices, fmt.Sprintf("%s:%s", dev.PathOnHost, dev.PathInContainer))
			}
		}

		roomResources = types.RoomResources{
			CPUShares: container.HostConfig.CPUShares,
			NanoCPUs:  container.HostConfig.NanoCPUs,
			ShmSize:   container.HostConfig.ShmSize,
			Memory:    container.HostConfig.Memory,
			Gpus:      gpus,
			Devices:   devices,
		}
		if labels.BrowserHost != nil {
			if hostContainer, err := manager.inspectContainer(ctx, labels.BrowserHost.ContainerID); err == nil && hostContainer.HostConfig != nil {
				roomResources.ShmSize = hostContainer.HostConfig.ShmSize
			}
		}
	}

	settings := types.RoomSettings{
		ApiVersion:     labels.ApiVersion,
		Name:           labels.Name,
		NekoImage:      labels.NekoImage,
		MaxConnections: labels.Epr.Max - labels.Epr.Min + 1,
		Labels:         labels.UserDefined,
		Mounts:         mounts,
		Resources:      roomResources,
		Hostname:       container.Config.Hostname,
		DNS:            container.HostConfig.DNS,
		BrowserPolicy:  browserPolicy,
	}

	if labels.Mux {
		settings.MaxConnections = 0
	}

	if err := settings.FromEnv(labels.ApiVersion, container.Config.Env); err != nil {
		return nil, err
	}
	if labels.BrowserHost != nil {
		for _, key := range browserRoomManagedEnvKeys {
			delete(settings.Envs, key)
		}
	}
	return &settings, nil
}

func (manager *RoomManagerCtx) GetStats(ctx context.Context, id string) (*types.RoomStats, error) {
	container, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}

	labels, err := manager.extractLabels(container.Config.Labels)
	if err != nil {
		return nil, err
	}

	settings := types.RoomSettings{}
	err = settings.FromEnv(labels.ApiVersion, container.Config.Env)
	if err != nil {
		return nil, err
	}

	var stats types.RoomStats
	switch labels.ApiVersion {
	case 2:
		output, err := manager.containerExec(ctx, id, []string{
			"wget", "-q", "-O-", "http://127.0.0.1:8080/stats?pwd=" + url.QueryEscape(settings.AdminPass),
		})
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal([]byte(output), &stats); err != nil {
			return nil, err
		}
	case 3:
		output, err := manager.containerExec(ctx, id, []string{
			"wget", "-q", "-O-", "http://127.0.0.1:8080/api/sessions?token=" + url.QueryEscape(settings.AdminPass),
		})
		if err != nil {
			return nil, err
		}

		var sessions []struct {
			ID      string `json:"id"`
			Profile struct {
				Name    string `json:"name"`
				IsAdmin bool   `json:"is_admin"`
			} `json:"profile"`
			State struct {
				IsConnected       bool       `json:"is_connected"`
				NotConnectedSince *time.Time `json:"not_connected_since"`
			} `json:"state"`
		}

		if err := json.Unmarshal([]byte(output), &sessions); err != nil {
			return nil, err
		}

		// create empty array so that it's not null in json
		stats.Members = []*types.RoomMember{}

		for _, session := range sessions {
			if session.State.IsConnected {
				stats.Connections++
				// append members
				stats.Members = append(stats.Members, &types.RoomMember{
					ID:    session.ID,
					Name:  session.Profile.Name,
					Admin: session.Profile.IsAdmin,
					Muted: false, // not supported
				})
			} else if session.State.NotConnectedSince != nil {
				// populate last admin left time
				if session.Profile.IsAdmin && (stats.LastAdminLeftAt == nil || (*session.State.NotConnectedSince).After(*stats.LastAdminLeftAt)) {
					stats.LastAdminLeftAt = session.State.NotConnectedSince
				}
				// populate last user left time
				if !session.Profile.IsAdmin && (stats.LastUserLeftAt == nil || (*session.State.NotConnectedSince).After(*stats.LastUserLeftAt)) {
					stats.LastUserLeftAt = session.State.NotConnectedSince
				}
			}
		}

		// parse started time
		if container.State.StartedAt != "" {
			stats.ServerStartedAt, err = time.Parse(time.RFC3339, container.State.StartedAt)
			if err != nil {
				return nil, err
			}
		}

		// TODO: settings & host
	default:
		return nil, fmt.Errorf("unsupported API version: %d", labels.ApiVersion)
	}

	return &stats, nil
}

func (manager *RoomManagerCtx) Start(ctx context.Context, id string) error {
	container, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return err
	}

	// If paused, we need to unpause the container
	if container.State.Paused {
		if err := manager.client.ContainerUnpause(ctx, id); err != nil {
			return err
		}

		return nil
	}

	// Start the actual container
	return manager.client.ContainerStart(ctx, id, dockerContainer.StartOptions{})
}

func (manager *RoomManagerCtx) Stop(ctx context.Context, id string) error {
	_, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return err
	}

	// Stop the actual container
	return manager.client.ContainerStop(ctx, id, dockerContainer.StopOptions{
		Signal:  "SIGTERM",
		Timeout: &manager.config.StopTimeoutSec,
	})
}

func (manager *RoomManagerCtx) Restart(ctx context.Context, id string) error {
	_, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return err
	}

	// Restart the actual container
	return manager.client.ContainerRestart(ctx, id, dockerContainer.StopOptions{
		Signal:  "SIGTERM",
		Timeout: &manager.config.StopTimeoutSec,
	})
}

func (manager *RoomManagerCtx) Pause(ctx context.Context, id string) error {
	_, err := manager.inspectContainer(ctx, id)
	if err != nil {
		return err
	}

	// Pause the actual container
	return manager.client.ContainerPause(ctx, id)
}

// events

func (manager *RoomManagerCtx) EventsLoopStart() {
	manager.events.Start()
}

func (manager *RoomManagerCtx) EventsLoopStop() error {
	return manager.events.Shutdown()
}

func (manager *RoomManagerCtx) Events(ctx context.Context) (<-chan types.RoomEvent, <-chan error) {
	return manager.events.Events(ctx)
}
