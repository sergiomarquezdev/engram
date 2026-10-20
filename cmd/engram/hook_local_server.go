package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Gentleman-Programming/engram/v3/internal/store"
)

const hookLocalServerPollInterval = 100 * time.Millisecond

// hookLocalServerRecoverySupported gates recovery to platforms where
// serve-background exists; tests enable it to exercise the gate everywhere.
var hookLocalServerRecoverySupported = runtime.GOOS == "windows"

// hookStartLocalServer launches the managed local server the way SessionStart
// does. It is a variable so tests can stand in for the child process.
var hookStartLocalServer = func(dataDir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return launchServeBackground(executable, filepath.Join(dataDir, "serve.err.log"), "ENGRAM_CLOUD_AUTOSYNC=1")
}

// hookManagedLocalDataDir returns the data directory that owns the default
// local endpoint, or "" when an explicit URL or socket names another owner.
func hookManagedLocalDataDir() string {
	if strings.TrimSpace(os.Getenv("ENGRAM_URL")) != "" || strings.TrimSpace(os.Getenv("ENGRAM_SOCKET")) != "" {
		return ""
	}
	if dir := strings.TrimSpace(os.Getenv("ENGRAM_DATA_DIR")); dir != "" {
		return dir
	}
	cfg, err := storeDefaultConfig()
	if err != nil {
		return ""
	}
	return cfg.DataDir
}

// recoverManagedLocalServer restarts the managed local server when nothing is
// listening on its endpoint, so a host that terminated the shared server (for
// example Claude Desktop ending its process job) does not deny every write
// until the next SessionStart. It reports true only once a server answering
// for this data directory is healthy. Any reachable listener, explicit
// endpoint or foreign instance leaves the original denial in place.
func recoverManagedLocalServer(ctx context.Context, client *http.Client, base string) bool {
	if !hookLocalServerRecoverySupported {
		return false
	}
	dataDir := hookManagedLocalDataDir()
	if dataDir == "" || !hookEndpointRefused(ctx, client, base) {
		return false
	}
	expected, err := hookInstanceID(ctx, dataDir)
	if err != nil || hookStartLocalServer(dataDir) != nil {
		return false
	}
	ticker := time.NewTicker(hookLocalServerPollInterval)
	defer ticker.Stop()
	for {
		if instance, ok := hookHealthInstance(ctx, client, base); ok {
			return instance == expected
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// hookInstanceID bounds the instance-ID lock wait by the gate's deadline.
func hookInstanceID(ctx context.Context, dataDir string) (string, error) {
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := store.EnsureInstanceID(dataDir)
		done <- result{id, err}
	}()
	select {
	case r := <-done:
		return r.id, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// wsaeConnRefused is Winsock's WSAECONNREFUSED; syscall.ECONNREFUSED does not
// match it on Windows.
const wsaeConnRefused syscall.Errno = 10061

// hookEndpointRefused reports whether the endpoint actively refused the
// connection. Timeouts, other dial errors and HTTP-level failures are not
// evidence of a dead server.
func hookEndpointRefused(ctx context.Context, client *http.Client, base string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		return false
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == syscall.ECONNREFUSED || errno == wsaeConnRefused)
}

// hookHealthInstance returns the instance ID a healthy endpoint reports.
func hookHealthInstance(ctx context.Context, client *http.Client, base string) (string, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return "", false
	}
	response, err := client.Do(request)
	if err != nil {
		return "", false
	}
	defer func() { _ = response.Body.Close() }()
	var health struct {
		InstanceID string `json:"instance_id"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&health) != nil {
		return "", false
	}
	return health.InstanceID, true
}
