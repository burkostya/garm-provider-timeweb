package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudbase/garm-provider-common/execution/common"
)

const (
	testVersion      = "v0.1.0-test"
	testToken        = "cli-test-secret-token-never-print"
	testControllerID = "12345678-1234-4234-8234-123456789abc"
	testPoolID       = "abcdefab-abcd-4abc-8abc-abcdefabcdef"
)

var testBinary string

// These tests execute the built provider, so exit codes, stdin, the clean GARM
// environment and the stdout/stderr boundary are exercised together.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "garm-provider-timeweb-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testBinary = filepath.Join(dir, "garm-provider-timeweb")
	if runtime.GOOS == "windows" {
		testBinary += ".exe"
	}
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBinary += ".exe"
	}
	build := exec.Command(goBinary, "build", "-trimpath", "-ldflags",
		"-X main.version="+testVersion+" -X main.commit=testcommit -X main.date=2026-09-21T00:00:00Z",
		"-o", testBinary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build provider for CLI tests: %v\n%s", err, output)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type cliResult struct {
	stdout string
	stderr string
	code   int
}

func invokeCLI(t *testing.T, args []string, env map[string]string, stdin string) cliResult {
	t.Helper()
	cmd := exec.Command(testBinary, args...)
	// Providers receive a clean environment from GARM. Do not accidentally
	// inherit the developer's GARM settings or cloud credentials in tests.
	cmd.Env = []string{}
	if runtime.GOOS == "windows" {
		cmd.Env = append(cmd.Env, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("execute provider: %v", err)
		}
		code = exitErr.ExitCode()
	}
	result := cliResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
	if strings.Contains(result.stdout+result.stderr, testToken) {
		t.Fatal("provider exposed the API token in its output")
	}
	return result
}

func writeConfig(t *testing.T, apiURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider.toml")
	contents := fmt.Sprintf("token = %q\napi_url = %q\nnetwork_id = \"test-vpc\"\n", testToken, apiURL)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func protocolEnvironment(configPath, command string) map[string]string {
	return map[string]string{
		"GARM_COMMAND":              command,
		"GARM_CONTROLLER_ID":        testControllerID,
		"GARM_PROVIDER_CONFIG_FILE": configPath,
		"GARM_INTERFACE_VERSION":    common.Version010,
	}
}

func requireFailure(t *testing.T, result cliResult, code int, message string) {
	t.Helper()
	if result.code != code || result.stdout != "" || !strings.Contains(result.stderr, message) {
		t.Fatalf("want exit %d, empty stdout and stderr containing %q; got exit %d, stdout %q, stderr %q",
			code, message, result.code, result.stdout, result.stderr)
	}
}

func TestVersionCommandsDoNotNeedConfiguration(t *testing.T) {
	t.Parallel()
	result := invokeCLI(t, []string{"--version"}, nil, "")
	if result.code != 0 || result.stderr != "" || result.stdout != "garm-provider-timeweb "+testVersion+" (commit testcommit, built 2026-09-21T00:00:00Z)\n" {
		t.Fatalf("unexpected --version result: %+v", result)
	}
	for _, interfaceVersion := range []string{"", common.Version010} {
		t.Run("GetVersion_"+interfaceVersion, func(t *testing.T) {
			result := invokeCLI(t, nil, map[string]string{
				"GARM_COMMAND":           "GetVersion",
				"GARM_INTERFACE_VERSION": interfaceVersion,
			}, "")
			if result.code != 0 || result.stderr != "" || result.stdout != testVersion+"\n" {
				t.Fatalf("unexpected GetVersion result: %+v", result)
			}
		})
	}
}

func TestCheckConfigIsOfflineAndRedactsInvalidTOML(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "configuration checks must not call the API", http.StatusInternalServerError)
	}))
	defer server.Close()
	path := writeConfig(t, server.URL+"/api/v1")
	result := invokeCLI(t, []string{"--check-config", path}, nil, "")
	if result.code != 0 || result.stderr != "" || result.stdout != "Configuration is valid.\n" {
		t.Fatalf("unexpected valid config result: %+v", result)
	}
	if err := os.WriteFile(path, []byte("token = \""+testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = invokeCLI(t, []string{"--check-config", path}, nil, "")
	requireFailure(t, result, 1, "invalid TOML")
	if got := requests.Load(); got != 0 {
		t.Fatalf("--check-config made %d API requests", got)
	}
}

func TestCLIRejectsConflictingOrIncompleteArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		message string
	}{
		{name: "positional", args: []string{"unexpected"}, message: "unexpected positional arguments"},
		{name: "conflicting", args: []string{"--version", "--check-config=config.toml"}, message: "cannot be combined"},
		{name: "empty config path", args: []string{"--check-config="}, message: "requires a configuration file path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireFailure(t, invokeCLI(t, tc.args, nil, ""), 1, tc.message)
		})
	}
}

func TestProtocolRejectsInvalidEnvironment(t *testing.T) {
	t.Parallel()
	configPath := writeConfig(t, "http://127.0.0.1:1/api/v1")
	for _, tc := range []struct {
		name    string
		change  func(map[string]string)
		message string
	}{
		{name: "missing command", change: func(env map[string]string) { delete(env, "GARM_COMMAND") }, message: "missing GARM_COMMAND"},
		{name: "missing config", change: func(env map[string]string) { delete(env, "GARM_PROVIDER_CONFIG_FILE") }, message: "missing GARM_PROVIDER_CONFIG_FILE"},
		{name: "missing controller", change: func(env map[string]string) { delete(env, "GARM_CONTROLLER_ID") }, message: "missing GARM_CONTROLLER_ID"},
		{name: "missing instance", change: func(env map[string]string) { env["GARM_COMMAND"] = "GetInstance" }, message: "missing instance ID"},
		{name: "missing pool", change: func(env map[string]string) { delete(env, "GARM_POOL_ID") }, message: "missing pool ID"},
		{name: "unknown command", change: func(env map[string]string) { env["GARM_COMMAND"] = "LaunchServer" }, message: "unknown GARM_COMMAND"},
		{name: "unsupported interface", change: func(env map[string]string) { env["GARM_INTERFACE_VERSION"] = "v0.1.1" }, message: "supports v0.1.0"},
		{name: "unprefixed interface", change: func(env map[string]string) { env["GARM_INTERFACE_VERSION"] = "0.1.0" }, message: "supports v0.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := protocolEnvironment(configPath, "ListInstances")
			env["GARM_POOL_ID"] = testPoolID
			tc.change(env)
			requireFailure(t, invokeCLI(t, nil, env, ""), 1, tc.message)
		})
	}
}

func TestCreateRejectsInvalidBootstrapBeforeAPICalls(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "bootstrap must be validated first", http.StatusInternalServerError)
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL+"/api/v1")
	for _, tc := range []struct {
		name    string
		stdin   string
		message string
	}{
		{name: "empty input", stdin: "", message: "requires data passed into stdin"},
		{name: "invalid JSON", stdin: `{"instance-token":"` + testToken + `",`, message: "failed to decode instance params"},
		{name: "missing bootstrap name", stdin: `{}`, message: "missing bootstrap params"},
		{name: "pool mismatch", stdin: `{"name":"garm-test","pool_id":"other-pool","instance-token":"` + testToken + `"}`, message: "bootstrap pool_id does not match GARM_POOL_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := protocolEnvironment(configPath, "CreateInstance")
			env["GARM_POOL_ID"] = testPoolID
			requireFailure(t, invokeCLI(t, nil, env, tc.stdin), 1, tc.message)
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid bootstrap caused %d API requests", got)
	}
}

func TestListInstancesReturnsOnlyJSONArray(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/servers" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("unexpected API request: %s %s (authorized=%t)", r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer "+testToken)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"servers":[],"meta":{"total":0}}`)
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL+"/api/v1")
	for _, interfaceVersion := range []string{"", common.Version010} {
		t.Run("interface_"+interfaceVersion, func(t *testing.T) {
			env := protocolEnvironment(configPath, "ListInstances")
			env["GARM_POOL_ID"] = testPoolID
			env["GARM_INTERFACE_VERSION"] = interfaceVersion
			result := invokeCLI(t, nil, env, "")
			if result.code != 0 || result.stderr != "" || strings.TrimSpace(result.stdout) != "[]" || !json.Valid([]byte(result.stdout)) {
				t.Fatalf("unexpected ListInstances result: %+v", result)
			}
		})
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("want 2 list calls, got %d", got)
	}
}

func TestCreateInstanceUsesBootstrapAndReturnsProviderJSON(t *testing.T) {
	t.Parallel()
	const bootstrapToken = "instance-bootstrap-secret-never-print"
	type createRequest struct {
		Name      string `json:"name"`
		Comment   string `json:"comment"`
		PresetID  int64  `json:"preset_id"`
		OSID      int64  `json:"os_id"`
		CloudInit string `json:"cloud_init"`
		Network   struct {
			ID string `json:"id"`
		} `json:"network"`
	}
	created := make(chan createRequest, 1)
	var networkCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/servers":
			fmt.Fprint(w, `{"servers":[],"meta":{"total":0}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/servers":
			var request createRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
				http.Error(w, "invalid create body", http.StatusBadRequest)
				return
			}
			select {
			case created <- request:
			default:
				t.Error("provider unexpectedly repeated CreateServer")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server": map[string]any{
					"id": 5001, "name": request.Name, "comment": request.Comment,
					"status": "install", "root_pass": "private-cloud-root-password",
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/servers/5001/local-networks/nat-mode":
			networkCalls.Add(1)
			var request struct {
				Mode string `json:"nat_mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Mode != "snat" {
				t.Error("expected the configured SNAT mode")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL+"/api/v1")
	bootstrap, err := json.Marshal(map[string]any{
		"name": "garm-cli-created", "pool_id": testPoolID,
		"os_type": "linux", "arch": "amd64", "flavor": "2447", "image": "os:79",
		"repo_url":       "https://github.com/example/example",
		"callback-url":   "https://garm.example.test/api/v1/callbacks",
		"metadata-url":   "https://garm.example.test/api/v1/metadata",
		"instance-token": bootstrapToken, "jit_config_enabled": true,
		"extra_specs": map[string]string{"network_mode": "snat"},
		"tools": []map[string]string{{
			"os": "linux", "architecture": "x64",
			"filename":     "actions-runner-linux-x64-test.tar.gz",
			"download_url": "https://github.com/actions/runner/releases/download/test/actions-runner-linux-x64-test.tar.gz",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := protocolEnvironment(configPath, "CreateInstance")
	env["GARM_POOL_ID"] = testPoolID
	result := invokeCLI(t, nil, env, string(bootstrap))
	if result.code != 0 || result.stderr != "" {
		t.Fatalf("unexpected CreateInstance result: %+v", result)
	}
	var instance map[string]any
	if err := json.Unmarshal([]byte(result.stdout), &instance); err != nil {
		t.Fatalf("CreateInstance stdout is not a single JSON object: %v", err)
	}
	if instance["provider_id"] != "5001" || instance["name"] != "garm-cli-created" || instance["status"] != "unknown" || instance["os_arch"] != "amd64" {
		t.Fatalf("incorrect GARM instance response: %v", instance)
	}
	if strings.Contains(result.stdout, bootstrapToken) || strings.Contains(result.stdout, "private-cloud-root-password") || instance["cloud_init"] != nil || instance["comment"] != nil {
		t.Fatal("CreateInstance exposed private API or bootstrap fields")
	}
	var request createRequest
	select {
	case request = <-created:
	default:
		t.Fatal("CreateInstance never created a server")
	}
	if request.Name != "garm-cli-created" || request.PresetID != 2447 || request.OSID != 79 || request.Network.ID != "test-vpc" {
		t.Fatal("bootstrap image, flavor, name or VPC was not passed to Timeweb")
	}
	if !strings.HasPrefix(request.CloudInit, "#cloud-config\n") || !strings.Contains(request.Comment, testControllerID) || !strings.Contains(request.Comment, testPoolID) {
		t.Fatal("create request lacks cloud-init or controller/pool ownership")
	}
	var installScript string
	for _, line := range strings.Split(request.CloudInit, "\n") {
		if encoded, ok := strings.CutPrefix(strings.TrimSpace(line), "content: "); ok {
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil && bytes.Contains(data, []byte(bootstrapToken)) {
				installScript = string(data)
			}
		}
	}
	if !strings.Contains(installScript, "https://garm.example.test/api/v1/metadata") || !strings.Contains(installScript, "credentials/runner") {
		t.Fatal("cloud-init did not preserve instance authentication and JIT bootstrap")
	}
	if got := networkCalls.Load(); got != 1 {
		t.Fatalf("expected one network configuration call, got %d", got)
	}
}

func TestMissingInstanceExitCodesAndIdempotentDelete(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("missing instance must not be mutated: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/servers" {
			fmt.Fprint(w, `{"servers":[],"meta":{"total":0}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"server not found"}`)
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL+"/api/v1")
	for _, identifier := range []string{"404", "garm-missing"} {
		t.Run(identifier, func(t *testing.T) {
			env := protocolEnvironment(configPath, "GetInstance")
			env["GARM_INSTANCE_ID"] = identifier
			result := invokeCLI(t, nil, env, "")
			requireFailure(t, result, common.ExitCodeNotFound, "")
			if result.stderr == "" {
				t.Fatal("missing instance failure has no diagnostic")
			}
			env["GARM_COMMAND"] = "DeleteInstance"
			result = invokeCLI(t, nil, env, "")
			if result.code != 0 || result.stdout != "" || result.stderr != "" {
				t.Fatalf("absent delete should succeed silently: %+v", result)
			}
		})
	}
}

func TestAPIErrorDoesNotExposeResponseSecrets(t *testing.T) {
	t.Parallel()
	const responseSecret = "private-runner-bootstrap-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"message":"%s","access_token":"%s","cloud_init":"%s"}`, testToken, testToken, responseSecret)
	}))
	defer server.Close()
	configPath := writeConfig(t, server.URL+"/api/v1")
	env := protocolEnvironment(configPath, "ListInstances")
	env["GARM_POOL_ID"] = testPoolID
	result := invokeCLI(t, nil, env, "")
	if result.code != 1 || result.stdout != "" || result.stderr == "" {
		t.Fatalf("unexpected API error result: %+v", result)
	}
	if strings.Contains(result.stderr, responseSecret) {
		t.Fatal("API error exposed the response body")
	}
}
