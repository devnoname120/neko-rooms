package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"time"
)

const browserHostLocksStoragePath = "./browser-host-locks"

type browserHostLockOwner struct {
	Version      int       `json:"version"`
	Key          string    `json:"key"`
	DockerID     string    `json:"docker_id"`
	InstanceName string    `json:"instance_name"`
	AcquiredAt   time.Time `json:"acquired_at"`
}

func (manager *RoomManagerCtx) browserHostLockPath(key string) string {
	return path.Join(manager.config.StorageInternal, browserHostLocksStoragePath, key)
}

func (manager *RoomManagerCtx) acquireBrowserHostLock(ctx context.Context, key string) (bool, error) {
	info, err := manager.client.Info(ctx)
	if err != nil {
		return false, fmt.Errorf("get Docker host identity: %w", err)
	}

	root := path.Join(manager.config.StorageInternal, browserHostLocksStoragePath)
	if err := os.MkdirAll(root, 0755); err != nil {
		return false, fmt.Errorf("create browser host lock root: %w", err)
	}

	owner := browserHostLockOwner{
		Version:      1,
		Key:          key,
		DockerID:     info.ID,
		InstanceName: manager.config.InstanceName,
		AcquiredAt:   time.Now().UTC(),
	}
	return acquireBrowserHostLockPath(manager.browserHostLockPath(key), owner)
}

func acquireBrowserHostLockPath(lockPath string, owner browserHostLockOwner) (bool, error) {
	if err := os.Mkdir(lockPath, 0755); err == nil {
		data, err := json.MarshalIndent(owner, "", "  ")
		if err == nil {
			err = os.WriteFile(path.Join(lockPath, "owner.json"), data, 0644)
		}
		if err != nil {
			_ = os.RemoveAll(lockPath)
			return false, fmt.Errorf("write browser host lock owner: %w", err)
		}
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("acquire browser host lock: %w", err)
	}

	existing, err := readBrowserHostLockOwner(lockPath)
	if err != nil {
		return false, fmt.Errorf("browser profile %s is locked by an unknown orchestrator", owner.Key)
	}
	if existing.DockerID == owner.DockerID && existing.InstanceName == owner.InstanceName {
		return false, nil
	}

	return false, fmt.Errorf(
		"browser profile %s is owned by instance %q on Docker host %q",
		owner.Key,
		existing.InstanceName,
		existing.DockerID,
	)
}

func (manager *RoomManagerCtx) releaseBrowserHostLock(ctx context.Context, key string) error {
	info, err := manager.client.Info(ctx)
	if err != nil {
		return fmt.Errorf("get Docker host identity: %w", err)
	}

	return releaseBrowserHostLockPath(manager.browserHostLockPath(key), browserHostLockOwner{
		Key:          key,
		DockerID:     info.ID,
		InstanceName: manager.config.InstanceName,
	})
}

func releaseBrowserHostLockPath(lockPath string, expected browserHostLockOwner) error {
	owner, err := readBrowserHostLockOwner(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read browser host lock owner: %w", err)
	}
	if owner.DockerID != expected.DockerID || owner.InstanceName != expected.InstanceName || owner.Key != expected.Key {
		return fmt.Errorf("cannot release browser host lock owned by another orchestrator")
	}
	return os.RemoveAll(lockPath)
}

func readBrowserHostLockOwner(lockPath string) (browserHostLockOwner, error) {
	data, err := os.ReadFile(path.Join(lockPath, "owner.json"))
	if err != nil {
		return browserHostLockOwner{}, err
	}
	var owner browserHostLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return browserHostLockOwner{}, err
	}
	return owner, nil
}
