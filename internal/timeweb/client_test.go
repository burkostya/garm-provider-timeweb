package timeweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "private-test-bearer-token"

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing bearer authentication")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing JSON accept header")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, testToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func respondJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestListServersPaginationUsesTotalAndExactIntegerIDs(t *testing.T) {
	var offsets []string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/servers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "100" {
			t.Error("unexpected page limit")
		}
		offset := r.URL.Query().Get("offset")
		offsets = append(offsets, offset)
		switch offset {
		case "0":
			io.WriteString(w, `{"servers":[{"id":9007199254740993,"name":"runner-a"},{"id":9007199254740994,"name":"runner-b"}],"meta":{"total":3}}`)
		case "2":
			io.WriteString(w, `{"servers":[{"id":9007199254740995,"name":"runner-c"}],"meta":{"total":3}}`)
		default:
			t.Error("unneeded or incorrect page offset")
		}
	})
	servers, err := client.ListServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offsets, []string{"0", "2"}) {
		t.Errorf("offsets = %v", offsets)
	}
	if len(servers) != 3 || servers[2].ID != 9007199254740995 {
		t.Errorf("incorrect decoded servers: %+v", servers)
	}
}

func TestListServersWithoutTotalContinuesUntilEmpty(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("offset") {
		case "0":
			io.WriteString(w, `{"servers":[{"id":7}]}`)
		case "1":
			io.WriteString(w, `{"servers":[{"id":8}]}`)
		case "2":
			io.WriteString(w, `{"servers":[]}`)
		default:
			t.Error("wrong offset")
		}
	})
	servers, err := client.ListServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || calls != 3 {
		t.Errorf("got %d servers in %d requests", len(servers), calls)
	}
}

func TestListServersDoesNotReturnIncompleteInventory(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"repeated page", `{"servers":[{"id":1}],"meta":{"total":3}}`, `{"servers":[{"id":1}],"meta":{"total":3}}`},
		{"overlapping final page", `{"servers":[{"id":1},{"id":2}],"meta":{"total":4}}`, `{"servers":[{"id":2},{"id":3}],"meta":{"total":4}}`},
		{"premature empty page", `{"servers":[{"id":1}],"meta":{"total":3}}`, `{"servers":[],"meta":{"total":3}}`},
		{"missing collection", `{}`, `{}`},
		{"null collection", `{"servers":null}`, `{}`},
		{"invalid ID", `{"servers":[{"id":0}]}`, `{}`},
		{"fractional ID", `{"servers":[{"id":1.25}]}`, `{}`},
		{"negative total", `{"servers":[],"meta":{"total":-1}}`, `{}`},
		{"inconsistent total", `{"servers":[{"id":1}],"meta":{"total":0}}`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					io.WriteString(w, tc.first)
				} else {
					io.WriteString(w, tc.second)
				}
			})
			servers, err := client.ListServers(context.Background())
			if err == nil || servers != nil {
				t.Fatalf("expected error and no partial inventory, got %v, %v", servers, err)
			}
			if calls > 2 {
				t.Errorf("client repeated invalid pagination %d times", calls)
			}
		})
	}
}

func TestListServersEmptyInventory(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"servers":[],"meta":{"total":0}}`) })
	servers, err := client.ListServers(context.Background())
	if err != nil || servers == nil || len(servers) != 0 {
		t.Errorf("empty inventory = %#v, %v", servers, err)
	}
}

func TestGetServerStatusAndNetworks(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/servers/42" {
			t.Error("wrong server request")
		}
		io.WriteString(w, `{"server":{"id":42,"name":"runner","comment":"ownership","status":"install","location":"ru-3","availability_zone":"msk-1","os":{"id":244,"name":"ubuntu","version":"24.04"},"networks":[{"id":"network-abc","type":"local","nat_mode":"snat","ips":[{"type":"ipv4","ip":"10.0.0.4","is_main":true}]},{"type":"public","ips":[{"type":"ipv6","ip":"2001:db8::4","is_main":true}]}],"root_pass":"sensitive-password","cloud_init":"sensitive-registration-token"}}`)
	})
	server, err := client.GetServer(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if server.Status != "install" || server.OS.Name != "ubuntu" || server.OS.Version != "24.04" || server.Comment != "ownership" || server.AvailabilityZone != "msk-1" {
		t.Errorf("incorrect decoded server: %+v", server)
	}
	if len(server.Networks) != 2 || server.Networks[0].NATMode != "snat" || server.Networks[0].IPs[0].IP != "10.0.0.4" || server.Networks[1].IPs[0].Type != "ipv6" {
		t.Errorf("incorrect network decoding: %+v", server.Networks)
	}
	encoded, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sensitive-") {
		t.Error("client retained passwords or registration token")
	}
}

func TestGetServerRejectsMissingMismatchedAndMalformedData(t *testing.T) {
	for _, response := range []string{`{}`, `{"server":null}`, `{"server":{"id":0}}`, `{"server":{"id":43}}`, `{"server":{"id":"secret"}}`, `{token-secret}`} {
		t.Run(response, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, response) })
			_, err := client.GetServer(context.Background(), 42)
			if err == nil {
				t.Fatal("expected invalid response error")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Error("decoder leaked server body")
			}
		})
	}
}

func TestCreateServerRequestContract(t *testing.T) {
	for _, useImage := range []bool{false, true} {
		t.Run(fmt.Sprint(useImage), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/servers" {
					t.Error("wrong create endpoint")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("wrong content type")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{
					"name": "runner", "comment": "owner-marker", "preset_id": float64(11),
					"cloud_init": "#cloud-config\nsecret-token", "network": map[string]any{"id": "network-123"},
					"ssh_keys_ids": []any{float64(99)}, "availability_zone": "msk-1", "project_id": float64(3),
					"hostname": "runner", "is_ddos_guard": false,
				}
				if useImage {
					want["image_id"] = "c1e84e7e-5924-445b-ae27-c7fd31b56541"
				} else {
					want["os_id"] = float64(244)
				}
				if !reflect.DeepEqual(payload, want) {
					t.Errorf("unexpected create JSON fields: got %v; want %v", payload, want)
				}
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"server":{"id":42,"name":"runner","status":"install"}}`)
			})
			req := CreateServerRequest{Name: "runner", Comment: "owner-marker", PresetID: 11, CloudInit: "#cloud-config\nsecret-token", Network: &CreateServerNetwork{ID: "network-123"}, SSHKeysIDs: []int64{99}, AvailabilityZone: "msk-1", ProjectID: 3, Hostname: "runner"}
			if useImage {
				req.ImageID = "c1e84e7e-5924-445b-ae27-c7fd31b56541"
			} else {
				req.OSID = 244
			}
			server, err := client.CreateServer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if server.ID != 42 || server.Status != "install" {
				t.Errorf("unexpected create result: %+v", server)
			}
		})
	}
}

func TestCreateAndPowerActionsNeverRetry(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			})
			_, err := client.CreateServer(context.Background(), CreateServerRequest{Name: "runner", PresetID: 1, OSID: 244})
			if err == nil || calls != 1 {
				t.Fatalf("create made %d attempts; error %v", calls, err)
			}
			if err := client.PerformAction(context.Background(), 42, "start"); err == nil || calls != 2 {
				t.Fatalf("power action made extra attempts; requests %d; error %v", calls, err)
			}
		})
	}
}

func TestDeleteServerConfirmationAndAcceptedResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		want        error
		wantInvalid bool
	}{
		{"immediate", 204, "", nil, false},
		{"already absent", 404, "", nil, false},
		{"deleted", 200, `{"server_delete":{"is_moved_in_quarantine":false}}`, nil, false},
		{"quarantined", 200, `{"server_delete":{"is_moved_in_quarantine":true}}`, nil, false},
		{"requires confirmation", 200, `{"server_delete":{"hash":"secret-confirmation-hash","is_moved_in_quarantine":false}}`, ErrDeleteConfirmationRequired, false},
		{"confirmation without flag", 200, `{"server_delete":{"hash":"secret-confirmation-hash"}}`, ErrDeleteConfirmationRequired, false},
		{"empty accepted", 202, "", ErrDeletionPending, false},
		{"accepted", 202, `{"server_delete":{"is_moved_in_quarantine":false}}`, ErrDeletionPending, false},
		{"accepted confirmation", 202, `{"server_delete":{"hash":"secret-confirmation-hash"}}`, ErrDeleteConfirmationRequired, false},
		{"unknown state", 200, `{"server_delete":{}}`, nil, true},
		{"missing state", 200, `{}`, nil, true},
		{"malformed state", 200, `secret-confirmation-hash`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/servers/42" || r.URL.RawQuery != "" {
					t.Error("wrong delete endpoint or unrequested confirmation")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			err := client.DeleteServer(context.Background(), 42)
			if tc.wantInvalid {
				if err == nil {
					t.Fatal("expected invalid deletion response error")
				}
			} else if !errors.Is(err, tc.want) {
				t.Errorf("error = %v; want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret-confirmation-hash") {
				t.Error("confirmation hash leaked")
			}
			if calls != 1 {
				t.Errorf("delete retried %d times", calls)
			}
		})
	}
}

func TestPowerAndNATRequestContracts(t *testing.T) {
	for _, action := range []string{"start", "shutdown", "hard_shutdown", "reboot", "hard_reboot"} {
		t.Run(action, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/servers/42/action" {
					t.Error("wrong action endpoint")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(body, map[string]string{"action": action}) {
					t.Errorf("action payload = %v", body)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			if err := client.PerformAction(context.Background(), 42, action); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, mode := range []string{"snat", "no_nat", "dnat_and_snat"} {
		t.Run(mode, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/servers/42/local-networks/nat-mode" {
					t.Error("wrong NAT endpoint")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(body, map[string]string{"nat_mode": mode}) {
					t.Errorf("NAT payload = %v", body)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			if err := client.SetNATMode(context.Background(), 42, mode); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAPIErrorSecrecyAndStatusClassification(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 423, 429, 500, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				respondJSON(t, w, map[string]any{"status_code": status, "error_code": testToken, "message": "cloud-init-and-root-password-secret", "response_id": testToken})
			})
			_, err := client.GetServer(context.Background(), 42)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
				t.Fatalf("wrong error: %v", err)
			}
			if errors.Is(err, ErrNotFound) != (status == 404) {
				t.Errorf("not-found classification for %d", status)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "secret") || len(err.Error()) > 100 {
				t.Errorf("unsafe error: %s", err)
			}
		})
	}
}

func TestRedirectsNeverForwardBearerOrReplayPOST(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalls.Add(1); w.WriteHeader(http.StatusCreated) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/secret-target", status) })
			_, err := client.CreateServer(context.Background(), CreateServerRequest{Name: "runner", PresetID: 1, OSID: 244})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
				t.Errorf("expected redirect rejection; got %v", err)
			}
		})
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorCannotLeakSecrets(t *testing.T) {
	client, err := NewClient("https://api.example.test", testToken, &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("arbitrary transport includes " + testToken)
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetServer(context.Background(), 42)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestRequestCancellationAndTimeout(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		calls := 0
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.GetServer(ctx, 42)
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Errorf("cancelled request: %v; calls %d", err, calls)
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		client, err := NewClient(server.URL, testToken, &http.Client{Timeout: 10 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetServer(context.Background(), 42)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout not classified: %v", err)
		}
	})
}

func TestNewClientValidatesConfigurationWithoutEchoingIt(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, token string }{
		{"http", "http://api.example.test", testToken},
		{"credentials", "https://user:secret@api.example.test", testToken},
		{"query", "https://api.example.test?token=secret", testToken},
		{"fragment", "https://api.example.test#secret", testToken},
		{"malformed", "https://%secret", testToken},
		{"relative", "/relative/secret", testToken},
		{"missing host", "https://:443", testToken},
		{"empty token", DefaultBaseURL, ""},
		{"newline injection", DefaultBaseURL, "secret\nheader"},
		{"spaces", DefaultBaseURL, "secret token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.endpoint, tc.token, nil)
			if err == nil {
				t.Fatal("expected invalid configuration")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), testToken) {
				t.Errorf("configuration leaked: %v", err)
			}
		})
	}
}

func TestNewClientDefaultsAndDoesNotMutateCallerClient(t *testing.T) {
	originalRedirect := func(_ *http.Request, _ []*http.Request) error { return nil }
	original := &http.Client{CheckRedirect: originalRedirect}
	client, err := NewClient("", testToken, original)
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL != DefaultBaseURL || client.httpClient.Timeout != 30*time.Second {
		t.Errorf("wrong client defaults: %+v", client)
	}
	if original.Timeout != 0 || original.CheckRedirect(nil, nil) != nil {
		t.Error("caller client mutated")
	}
	if client.httpClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Error("redirect protection absent")
	}
}

func TestClientAcceptsExplicitAPIVersionSuffix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/api/v1/servers/42" {
			t.Errorf("incorrect base URL joining: %s", r.URL.Path)
		}
		io.WriteString(w, `{"server":{"id":42}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/proxy/api/v1/", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetServer(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsInvalidMutationBeforeNetwork(t *testing.T) {
	calls := 0
	client := testClient(t, func(_ http.ResponseWriter, _ *http.Request) { calls++ })
	for _, req := range []CreateServerRequest{
		{Name: "runner", PresetID: 1},
		{Name: "runner", PresetID: 1, OSID: 244, ImageID: "custom"},
		{Name: "runner", PresetID: 0, OSID: 244},
		{Name: "runner", PresetID: 1, OSID: -1, ImageID: "custom"},
		{PresetID: 1, OSID: 244},
	} {
		if _, err := client.CreateServer(context.Background(), req); err == nil {
			t.Error("invalid create accepted")
		}
	}
	if _, err := client.GetServer(context.Background(), 0); err == nil {
		t.Error("invalid get ID accepted")
	}
	if err := client.DeleteServer(context.Background(), -1); err == nil {
		t.Error("invalid delete ID accepted")
	}
	if err := client.PerformAction(context.Background(), 0, "start"); err == nil {
		t.Error("invalid action ID accepted")
	}
	if err := client.PerformAction(context.Background(), 1, "remove"); err == nil {
		t.Error("confirmation-bypassing remove action accepted")
	}
	if err := client.SetNATMode(context.Background(), 0, "snat"); err == nil {
		t.Error("invalid NAT ID accepted")
	}
	if err := client.SetNATMode(context.Background(), 1, "wrong"); err == nil {
		t.Error("invalid NAT mode accepted")
	}
	if calls != 0 {
		t.Errorf("invalid input made %d requests", calls)
	}
}

func TestResponseSizeLimit(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		io.CopyN(w, strings.NewReader(strings.Repeat("x", maxResponseBytes+1)), maxResponseBytes+1)
	})
	_, err := client.GetServer(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Errorf("unbounded response: %v", err)
	}
}
