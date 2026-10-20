package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/v3/internal/store"
)

// reserveLocalPort returns a loopback port with no listener.
func reserveLocalPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(port)
}

// serveLocalEngram impersonates a managed local server on port. sessionStatus
// answers the s9 lookup; 404 lets the gate register it as a first-time session.
func serveLocalEngram(t *testing.T, port, instanceID string, sessionStatus int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": instanceID})
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/s9":
			w.WriteHeader(sessionStatus)
			_, _ = io.WriteString(w, `{"error":"lookup"}`)
		case r.URL.Path == "/project/current":
			_, _ = io.WriteString(w, `{"project":"docs","project_source":"dir_basename"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"s9","status":"created"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
}

func TestClaudeWriteGateRecoversManagedLocalServer(t *testing.T) {
	// A host can terminate the shared server it happened to start (Claude
	// Desktop ends its process job on quit, #1956). With nothing listening on
	// the managed local endpoint the gate restarts it and confirms once; every
	// other failure keeps the fail-closed denial.
	const foreign = "ffffffffffffffffffffffffffffffff"
	for _, tc := range []struct {
		name         string
		explicitURL  bool
		unsupported  bool
		running      bool // a reachable server already answers, failing the lookup
		launched     string
		wantAllow    bool
		wantLaunches int
	}{
		{name: "refused managed endpoint restarts the server and allows", launched: "own", wantAllow: true, wantLaunches: 1},
		{name: "explicit URL never launches", explicitURL: true, launched: "own"},
		{name: "unsupported platform never launches", unsupported: true, launched: "own"},
		{name: "reachable failing server is not restarted", running: true, launched: "own"},
		{name: "foreign instance on the port stays denied", launched: foreign, wantLaunches: 1},
		{name: "server that never becomes healthy is denied", launched: "", wantLaunches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := reserveLocalPort(t)
			dataDir := filepath.Join(t.TempDir(), "data")
			t.Setenv("ENGRAM_URL", "")
			t.Setenv("ENGRAM_SOCKET", "")
			t.Setenv("ENGRAM_PORT", port)
			t.Setenv("ENGRAM_DATA_DIR", dataDir)
			t.Setenv("ENGRAM_HOOK_SESSION_CONFIRMATION_TIMEOUT", "500ms")
			if tc.explicitURL {
				t.Setenv("ENGRAM_URL", "http://127.0.0.1:"+port)
			}
			if tc.running {
				serveLocalEngram(t, port, "", http.StatusInternalServerError)
			}
			oldSupported, oldStart := hookLocalServerRecoverySupported, hookStartLocalServer
			t.Cleanup(func() { hookLocalServerRecoverySupported, hookStartLocalServer = oldSupported, oldStart })
			hookLocalServerRecoverySupported = !tc.unsupported
			launches := 0
			hookStartLocalServer = func(dir string) error {
				launches++
				if dir != dataDir {
					t.Errorf("launch data dir = %q, want %q", dir, dataDir)
				}
				instance := tc.launched
				if instance == "own" {
					var err error
					if instance, err = store.EnsureInstanceID(dir); err != nil {
						return err
					}
				}
				if instance != "" {
					serveLocalEngram(t, port, instance, http.StatusNotFound)
				}
				return nil
			}

			input, _ := json.Marshal(map[string]any{"session_id": "s9", "cwd": "/w/docs", "tool_name": "mcp__engram__mem_save", "tool_input": map[string]any{"title": "t", "content": "c"}})
			start := time.Now()
			var hook struct {
				HookSpecificOutput struct {
					PermissionDecision       string         `json:"permissionDecision"`
					PermissionDecisionReason string         `json:"permissionDecisionReason"`
					UpdatedInput             map[string]any `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(guardClaudePreToolUse(input), &hook); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("gate took %s, want it bounded by the confirmation budget", elapsed)
			}
			bound := hook.HookSpecificOutput
			if launches != tc.wantLaunches {
				t.Fatalf("launches = %d, want %d", launches, tc.wantLaunches)
			}
			if tc.wantAllow {
				if bound.PermissionDecision == "deny" || bound.UpdatedInput["session_id"] != "s9" {
					t.Fatalf("recovered server must confirm the write: decision=%q (%s)", bound.PermissionDecision, bound.PermissionDecisionReason)
				}
				return
			}
			if bound.PermissionDecision != "deny" || bound.UpdatedInput != nil {
				t.Fatalf("%s must fail closed: decision=%q input=%v", tc.name, bound.PermissionDecision, bound.UpdatedInput)
			}
			if tc.unsupported {
				if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
					t.Fatalf("unsupported platform touched the data dir: %v", err)
				}
			}
		})
	}
}
