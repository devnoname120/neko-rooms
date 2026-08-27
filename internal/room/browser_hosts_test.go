package room

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

func TestBrowserHostKeyIdentifiesBackingProfile(t *testing.T) {
	base := browserHostKey("/accounts/main")
	if got := browserHostKey("/accounts/main/"); got != base {
		t.Fatalf("equivalent paths produced different keys: %q != %q", got, base)
	}
	if got := browserHostKey("/accounts/other"); got == base {
		t.Fatal("different shared paths produced the same browser host key")
	}
}

func TestBrowserHostSpecSeparatesIncompatibleHosts(t *testing.T) {
	profile := browserProfileMount{SharedPath: "/accounts/main", ProfilePath: "/home/neko/profile"}
	settings := types.RoomSettings{
		ApiVersion: 3,
		Screen:     "1280x720@30",
		Envs:       map[string]string{"NEKO_FIREFOX_ARGS": "--private-window"},
		BrowserPolicy: &types.BrowserPolicy{
			Type:    types.FirefoxBrowserPolicy,
			Path:    "/usr/lib/firefox/distribution/policies.json",
			Profile: profile.ProfilePath,
			Content: types.BrowserPolicyContent{PersistentData: true},
		},
	}

	base, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil {
		t.Fatal(err)
	}
	same, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil || same != base {
		t.Fatalf("equivalent specs produced different hashes: %q != %q (%v)", same, base, err)
	}
	settings.DNS = []string{}
	settings.Resources.Gpus = []string{}
	settings.Resources.Devices = []string{}
	settings.BrowserPolicy.Content.Extensions = []types.BrowserPolicyExtension{}
	normalized, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil || normalized != base {
		t.Fatalf("nil and empty collections produced different hashes: %q != %q (%v)", normalized, base, err)
	}

	changedImage, err := browserHostSpecHash(settings, profile, "sha256:image-b")
	if err != nil {
		t.Fatal(err)
	}
	if changedImage == base {
		t.Fatal("different image IDs produced the same browser host spec")
	}

	settings.Screen = "1920x1080@30"
	changedScreen, err := browserHostSpecHash(settings, profile, "sha256:image-a")
	if err != nil {
		t.Fatal(err)
	}
	if changedScreen == base {
		t.Fatal("different screens produced the same browser host spec")
	}
}

func TestSetContainerEnvReplacesExistingValue(t *testing.T) {
	env := setContainerEnv([]string{"A=1", "B=2", "A=old"}, "A", "new")
	want := []string{"B=2", "A=new"}
	if len(env) != len(want) {
		t.Fatalf("environment = %#v, want %#v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Fatalf("environment = %#v, want %#v", env, want)
		}
	}
}

func TestBrowserWindowForRegionUsesSlotGeometry(t *testing.T) {
	windows := []browserWindow{
		{ID: 11, X: 2, Y: 2, Width: 1280, Height: 720},
		{ID: 22, X: 1282, Y: 2, Width: 1280, Height: 720},
	}
	host := &BrowserHostLabels{WindowX: 1280, WindowY: 0, WindowWidth: 1280, WindowHeight: 720}
	window, ok := browserWindowForRegion(windows, host, map[uint64]struct{}{})
	if !ok || window.ID != 22 {
		t.Fatalf("window = (%+v, %v), want ID 22", window, ok)
	}
	if _, ok := browserWindowForRegion(windows, host, map[uint64]struct{}{22: {}}); ok {
		t.Fatal("used window was assigned twice")
	}
}

func TestBrowserWindowForAssignmentKeepsTheOriginalWindow(t *testing.T) {
	windows := []browserWindow{
		{ID: 11, X: 1288, Y: 0, Width: 1280, Height: 720},
		{ID: 22, X: 0, Y: 0, Width: 1280, Height: 720},
	}
	host := &BrowserHostLabels{WindowID: 11, WindowX: 0, WindowY: 0, WindowWidth: 1280, WindowHeight: 720}
	window, ok := browserWindowForAssignment(windows, host, map[uint64]struct{}{})
	if !ok || window.ID != 11 {
		t.Fatalf("window = (%+v, %v), want moved original window ID 11", window, ok)
	}
}

func TestBrowserWindowReservationsMakeAllocationTransactional(t *testing.T) {
	assigned := []*BrowserHostLabels{{WindowSlot: 0}}
	reservations := map[int]browserWindow{1: {ID: 22}}
	slot, ok := firstAvailableBrowserWindowSlot(assigned, reservations)
	if !ok || slot != 2 {
		t.Fatalf("slot = (%d, %v), want (2, true)", slot, ok)
	}
	for slot := 2; slot < browserHostColumns*browserHostRows; slot++ {
		reservations[slot] = browserWindow{ID: uint64(slot + 100)}
	}
	if slot, ok := firstAvailableBrowserWindowSlot(assigned, reservations); ok {
		t.Fatalf("full allocation returned slot %d", slot)
	}
}

func TestFilterBrowserProcessWindowsIgnoresCrashReporter(t *testing.T) {
	windows := []browserWindow{
		{ID: 1, PID: 100, Process: "firefox-bin", Width: 1280, Height: 720},
		{ID: 2, PID: 100, Process: "firefox-bin", Width: 1280, Height: 720},
		{ID: 3, PID: 200, Process: "crashreporter", Width: 4000, Height: 2000},
	}
	filtered := filterBrowserProcessWindows(windows)
	if len(filtered) != 2 || filtered[0].PID != 100 || filtered[1].PID != 100 {
		t.Fatalf("filtered windows = %#v", filtered)
	}
}

func TestBrowserProfileRequiresMatchingSharedPersistentMount(t *testing.T) {
	manager := &RoomManagerCtx{config: &config.Room{StorageExternal: "/data"}}
	settings := &types.RoomSettings{
		BrowserPolicy: &types.BrowserPolicy{
			Profile: "/home/neko/profile",
			Content: types.BrowserPolicyContent{PersistentData: true},
		},
		Mounts: []types.RoomMount{{
			Type:          types.MountShared,
			HostPath:      "/accounts/main",
			ContainerPath: "/home/neko/profile",
		}},
	}

	profile := manager.browserProfile(settings)
	if profile == nil {
		t.Fatal("matching shared browser profile was not detected")
	}
	if profile.ExternalPath != "/data/shared/accounts/main" {
		t.Fatalf("external path = %q", profile.ExternalPath)
	}

	settings.BrowserPolicy.Content.PersistentData = false
	if profile := manager.browserProfile(settings); profile != nil {
		t.Fatal("non-persistent browser policy produced a browser host")
	}
}

func TestBrowserHostLockIsExclusiveAndOwnerChecked(t *testing.T) {
	lockPath := t.TempDir() + "/profile"
	first := browserHostLockOwner{Version: 1, Key: "profile", DockerID: "docker-a", InstanceName: "rooms-a"}
	second := browserHostLockOwner{Version: 1, Key: "profile", DockerID: "docker-b", InstanceName: "rooms-b"}

	created, err := acquireBrowserHostLockPath(lockPath, first)
	if err != nil || !created {
		t.Fatalf("first lock acquire = (%v, %v), want (true, nil)", created, err)
	}
	created, err = acquireBrowserHostLockPath(lockPath, first)
	if err != nil || created {
		t.Fatalf("owner lock reacquire = (%v, %v), want (false, nil)", created, err)
	}
	if _, err := acquireBrowserHostLockPath(lockPath, second); err == nil {
		t.Fatal("second orchestrator acquired the same browser host lock")
	}
	if err := releaseBrowserHostLockPath(lockPath, second); err == nil {
		t.Fatal("foreign orchestrator released browser host lock")
	}
	if err := releaseBrowserHostLockPath(lockPath, first); err != nil {
		t.Fatalf("owner failed to release browser host lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock path still exists after release: %v", err)
	}
}

func TestBrowserHostProcessLeaseIsExclusive(t *testing.T) {
	lockPath := t.TempDir()
	first, err := acquireBrowserHostLeasePath(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireBrowserHostLeasePath(lockPath); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("second process lease error = %v, want EWOULDBLOCK", err)
	}
	if err := syscall.Flock(int(first.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireBrowserHostLeasePath(lockPath)
	if err != nil {
		t.Fatalf("lease was not adoptable after release: %v", err)
	}
	_ = syscall.Flock(int(second.Fd()), syscall.LOCK_UN)
	_ = second.Close()
}

func TestBrowserHostOpenboxConfigStartsWindowsInQuarantine(t *testing.T) {
	root := t.TempDir()
	manager := &RoomManagerCtx{config: &config.Room{StorageInternal: root, StorageExternal: "/external"}}
	source, err := manager.writeBrowserHostOpenboxConfig(strings.Repeat("a", 64), "1280x720@30")
	if err != nil {
		t.Fatal(err)
	}
	if source != "/external/templates/browser-host-aaaaaaaaaaaa-openbox.xml" {
		t.Fatalf("source = %q", source)
	}
	data, err := os.ReadFile(root + "/templates/browser-host-aaaaaaaaaaaa-openbox.xml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<x>3864</x>") || !strings.Contains(string(data), "<maximized>false</maximized>") {
		t.Fatalf("Openbox quarantine config is incomplete:\n%s", data)
	}
}
