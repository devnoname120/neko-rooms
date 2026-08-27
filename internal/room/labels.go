package room

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/m1k1o/neko-rooms/internal/types"
)

var labelRegex = regexp.MustCompile(`^[a-z0-9.-]+$`)

type RoomLabels struct {
	Name string
	URL  string
	Mux  bool
	Epr  EprPorts

	NekoImage  string
	ApiVersion int

	BrowserPolicy *BrowserPolicyLabels
	BrowserHost   *BrowserHostLabels
	UserDefined   map[string]string
}

type BrowserPolicyLabels struct {
	Type    types.BrowserPolicyType
	Path    string
	Profile string
}

type BrowserHostLabels struct {
	Key          string
	ContainerID  string
	WindowID     uint64
	WindowSlot   int
	WindowX      int
	WindowY      int
	WindowWidth  int
	WindowHeight int
	SharedPath   string
	ProfilePath  string
}

func (manager *RoomManagerCtx) extractLabels(labels map[string]string) (*RoomLabels, error) {
	name, ok := labels["m1k1o.neko_rooms.name"]
	if !ok {
		return nil, fmt.Errorf("damaged container labels: name not found")
	}

	url, ok := labels["m1k1o.neko_rooms.url"]
	if !ok {
		// TODO: It should be always available.
		url = manager.config.GetRoomUrl(name)
		//return nil, fmt.Errorf("damaged container labels: url not found")
	}

	var mux bool
	var epr EprPorts

	muxStr, ok := labels["m1k1o.neko_rooms.mux"]
	if ok {
		muxPort, err := strconv.ParseUint(muxStr, 10, 16)
		if err != nil {
			return nil, err
		}

		mux = true
		epr = EprPorts{
			Min: uint16(muxPort),
			Max: uint16(muxPort),
		}
	} else {
		eprMinStr, ok := labels["m1k1o.neko_rooms.epr.min"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: epr.min not found")
		}

		eprMin, err := strconv.ParseUint(eprMinStr, 10, 16)
		if err != nil {
			return nil, err
		}

		eprMaxStr, ok := labels["m1k1o.neko_rooms.epr.max"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: epr.max not found")
		}

		eprMax, err := strconv.ParseUint(eprMaxStr, 10, 16)
		if err != nil {
			return nil, err
		}

		mux = false
		epr = EprPorts{
			Min: uint16(eprMin),
			Max: uint16(eprMax),
		}
	}

	nekoImage, ok := labels["m1k1o.neko_rooms.neko_image"]
	if !ok {
		return nil, fmt.Errorf("damaged container labels: neko_image not found")
	}

	apiVersion := 2 // default, prior to api versioning
	apiVersionStr, ok := labels["m1k1o.neko_rooms.api_version"]
	if ok {
		var err error
		apiVersion, err = strconv.Atoi(apiVersionStr)
		if err != nil {
			return nil, err
		}
	}

	var browserPolicy *BrowserPolicyLabels
	if val, ok := labels["m1k1o.neko_rooms.browser_policy"]; ok && val == "true" {
		policyType, ok := labels["m1k1o.neko_rooms.browser_policy.type"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_policy.type not found")
		}

		policyPath, ok := labels["m1k1o.neko_rooms.browser_policy.path"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_policy.path not found")
		}

		browserPolicy = &BrowserPolicyLabels{
			Type:    types.BrowserPolicyType(policyType),
			Path:    policyPath,
			Profile: labels["m1k1o.neko_rooms.browser_policy.profile"],
		}
	}

	var browserHost *BrowserHostLabels
	if key, ok := labels["m1k1o.neko_rooms.browser_host.key"]; ok {
		containerID, ok := labels["m1k1o.neko_rooms.browser_host.container_id"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_host.container_id not found")
		}
		windowIDValue, ok := labels["m1k1o.neko_rooms.browser_host.window_id"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_host.window_id not found")
		}
		windowID, err := strconv.ParseUint(windowIDValue, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_id: %w", err)
		}
		sharedPath, ok := labels["m1k1o.neko_rooms.browser_host.shared_path"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_host.shared_path not found")
		}
		profilePath, ok := labels["m1k1o.neko_rooms.browser_host.profile_path"]
		if !ok {
			return nil, fmt.Errorf("damaged container labels: browser_host.profile_path not found")
		}
		windowSlot, err := strconv.Atoi(labels["m1k1o.neko_rooms.browser_host.window_slot"])
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_slot: %w", err)
		}
		if windowSlot < 0 || windowSlot >= browserHostColumns*browserHostRows {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_slot: %d", windowSlot)
		}
		windowX, err := strconv.Atoi(labels["m1k1o.neko_rooms.browser_host.window_x"])
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_x: %w", err)
		}
		if windowX < 0 {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_x: %d", windowX)
		}
		windowY, err := strconv.Atoi(labels["m1k1o.neko_rooms.browser_host.window_y"])
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_y: %w", err)
		}
		if windowY < 0 {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_y: %d", windowY)
		}
		windowWidth, err := strconv.Atoi(labels["m1k1o.neko_rooms.browser_host.window_width"])
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_width: %w", err)
		}
		if windowWidth <= 0 {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_width: %d", windowWidth)
		}
		windowHeight, err := strconv.Atoi(labels["m1k1o.neko_rooms.browser_host.window_height"])
		if err != nil {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_height: %w", err)
		}
		if windowHeight <= 0 {
			return nil, fmt.Errorf("damaged container labels: invalid browser_host.window_height: %d", windowHeight)
		}

		browserHost = &BrowserHostLabels{
			Key:          key,
			ContainerID:  containerID,
			WindowID:     windowID,
			WindowSlot:   windowSlot,
			WindowX:      windowX,
			WindowY:      windowY,
			WindowWidth:  windowWidth,
			WindowHeight: windowHeight,
			SharedPath:   sharedPath,
			ProfilePath:  profilePath,
		}
	}

	// extract user defined labels
	userDefined := map[string]string{}
	for key, val := range labels {
		if after, ok := strings.CutPrefix(key, "m1k1o.neko_rooms.x-"); ok {
			userDefined[after] = val
		}
	}

	return &RoomLabels{
		Name: name,
		URL:  url,
		Mux:  mux,
		Epr:  epr,

		NekoImage:  nekoImage,
		ApiVersion: apiVersion,

		BrowserPolicy: browserPolicy,
		BrowserHost:   browserHost,
		UserDefined:   userDefined,
	}, nil
}

func (manager *RoomManagerCtx) serializeLabels(labels RoomLabels) map[string]string {
	labelsMap := map[string]string{
		"m1k1o.neko_rooms.name":       labels.Name,
		"m1k1o.neko_rooms.url":        manager.config.GetRoomUrl(labels.Name),
		"m1k1o.neko_rooms.instance":   manager.config.InstanceName,
		"m1k1o.neko_rooms.neko_image": labels.NekoImage,
	}

	// api version 2 is currently default
	if labels.ApiVersion != 2 {
		labelsMap["m1k1o.neko_rooms.api_version"] = fmt.Sprintf("%d", labels.ApiVersion)
	}

	if labels.Mux && labels.Epr.Min == labels.Epr.Max {
		labelsMap["m1k1o.neko_rooms.mux"] = fmt.Sprintf("%d", labels.Epr.Min)
	} else {
		labelsMap["m1k1o.neko_rooms.epr.min"] = fmt.Sprintf("%d", labels.Epr.Min)
		labelsMap["m1k1o.neko_rooms.epr.max"] = fmt.Sprintf("%d", labels.Epr.Max)
	}

	if labels.BrowserPolicy != nil {
		labelsMap["m1k1o.neko_rooms.browser_policy"] = "true"
		labelsMap["m1k1o.neko_rooms.browser_policy.type"] = string(labels.BrowserPolicy.Type)
		labelsMap["m1k1o.neko_rooms.browser_policy.path"] = labels.BrowserPolicy.Path
		if labels.BrowserPolicy.Profile != "" {
			labelsMap["m1k1o.neko_rooms.browser_policy.profile"] = labels.BrowserPolicy.Profile
		}
	}

	if labels.BrowserHost != nil {
		labelsMap["m1k1o.neko_rooms.browser_host.key"] = labels.BrowserHost.Key
		labelsMap["m1k1o.neko_rooms.browser_host.container_id"] = labels.BrowserHost.ContainerID
		labelsMap["m1k1o.neko_rooms.browser_host.window_id"] = strconv.FormatUint(labels.BrowserHost.WindowID, 10)
		labelsMap["m1k1o.neko_rooms.browser_host.window_slot"] = strconv.Itoa(labels.BrowserHost.WindowSlot)
		labelsMap["m1k1o.neko_rooms.browser_host.window_x"] = strconv.Itoa(labels.BrowserHost.WindowX)
		labelsMap["m1k1o.neko_rooms.browser_host.window_y"] = strconv.Itoa(labels.BrowserHost.WindowY)
		labelsMap["m1k1o.neko_rooms.browser_host.window_width"] = strconv.Itoa(labels.BrowserHost.WindowWidth)
		labelsMap["m1k1o.neko_rooms.browser_host.window_height"] = strconv.Itoa(labels.BrowserHost.WindowHeight)
		labelsMap["m1k1o.neko_rooms.browser_host.shared_path"] = labels.BrowserHost.SharedPath
		labelsMap["m1k1o.neko_rooms.browser_host.profile_path"] = labels.BrowserHost.ProfilePath
	}

	for key, val := range labels.UserDefined {
		// to lowercase
		key = strings.ToLower(key)

		labelsMap[fmt.Sprintf("m1k1o.neko_rooms.x-%s", key)] = val
	}

	return labelsMap
}

func CheckLabelKey(name string) bool {
	return labelRegex.MatchString(name)
}
