package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The child runs before testing parses the launcher's positional serve argument.
func init() {
	if os.Getenv("ENGRAM_BACKGROUND_TEST_CHILD") != "1" {
		return
	}
	marker := os.Getenv("ENGRAM_BACKGROUND_TEST_MARKER")
	if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if envMarker := os.Getenv("ENGRAM_BACKGROUND_TEST_ENV_MARKER"); envMarker != "" {
		if err := os.WriteFile(envMarker, []byte(os.Getenv("ENGRAM_CLOUD_AUTOSYNC")), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	fmt.Fprintln(os.Stderr, "background child stderr marker")
	time.Sleep(8 * time.Second)
	os.Exit(0)
}

func TestServeBackground(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows handle inheritance regression")
	}
	if testing.Short() {
		t.Skip("runs a child process")
	}
	t.Run("long-lived child and log", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "child.pid")
		logPath := filepath.Join(dir, "serve.err.log")
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("ENGRAM_BACKGROUND_TEST_CHILD", "1")
		t.Setenv("ENGRAM_BACKGROUND_TEST_MARKER", marker)
		start := time.Now()
		if err := launchServeBackground(executable, logPath); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("launch took %s", elapsed)
		}
		var pid int
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(marker)
			if err == nil {
				pid, err = strconv.Atoi(string(data))
				if err != nil {
					t.Fatal(err)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if pid == 0 {
			t.Fatal("child did not write its process marker")
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = process.Kill(); _, _ = process.Wait() })
		time.Sleep(4100 * time.Millisecond)
		if err := process.Kill(); err != nil {
			t.Fatalf("child died before four seconds: %v", err)
		}
		_, _ = process.Wait()
		data, err := os.ReadFile(logPath)
		if err != nil || !strings.Contains(string(data), "background child stderr marker") {
			t.Fatalf("stderr log=%q err=%v", data, err)
		}
	})
	t.Run("extra environment reaches the child", func(t *testing.T) {
		dir := t.TempDir()
		envMarker := filepath.Join(dir, "child.env")
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("ENGRAM_BACKGROUND_TEST_CHILD", "1")
		t.Setenv("ENGRAM_BACKGROUND_TEST_MARKER", filepath.Join(dir, "child.pid"))
		t.Setenv("ENGRAM_BACKGROUND_TEST_ENV_MARKER", envMarker)
		t.Setenv("ENGRAM_CLOUD_AUTOSYNC", "")
		if err := launchServeBackground(executable, filepath.Join(dir, "serve.err.log"), "ENGRAM_CLOUD_AUTOSYNC=1"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(envMarker); err == nil && len(data) > 0 {
				if string(data) != "1" {
					t.Fatalf("ENGRAM_CLOUD_AUTOSYNC = %q, want 1", data)
				}
				if pid, err := os.ReadFile(filepath.Join(dir, "child.pid")); err == nil {
					if n, err := strconv.Atoi(string(pid)); err == nil {
						if process, err := os.FindProcess(n); err == nil {
							_ = process.Kill()
							_, _ = process.Wait()
						}
					}
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("child did not record its environment")
	})
	t.Run("bad log path", func(t *testing.T) {
		if err := launchServeBackground("unused.exe", filepath.Join(t.TempDir(), "missing", "log")); err == nil {
			t.Fatal("expected log open error")
		}
	})
	t.Run("invalid executable", func(t *testing.T) {
		executable := filepath.Join(t.TempDir(), "child.exe")
		if err := os.WriteFile(executable, []byte("not executable"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := launchServeBackground(executable, filepath.Join(t.TempDir(), "serve.err.log")); err == nil || !strings.Contains(err.Error(), "start") {
			t.Fatalf("invalid executable: %v", err)
		}
	})
}
