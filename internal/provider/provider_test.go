package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/burkostya/garm-provider-timeweb/internal/config"
	"github.com/burkostya/garm-provider-timeweb/internal/timeweb"
	garmerrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/params"
	"gopkg.in/yaml.v3"
)

type fakeServerClient struct {
	servers    []timeweb.Server
	listErr    error
	listCalls  int
	getCalls   []int64
	creates    []timeweb.CreateServerRequest
	deletes    []int64
	actions    []recordedAction
	natChanges []recordedAction
	createHook func(context.Context, timeweb.CreateServerRequest) (timeweb.Server, error)
	getHook    func(context.Context, int64) (timeweb.Server, error)
	deleteHook func(context.Context, int64) error
	natHook    func(context.Context, int64, string) error
}

type recordedAction struct {
	id     int64
	action string
}

var _ serverClient = (*fakeServerClient)(nil)

func (f *fakeServerClient) ListServers(context.Context) ([]timeweb.Server, error) {
	f.listCalls++
	return slices.Clone(f.servers), f.listErr
}

func (f *fakeServerClient) GetServer(ctx context.Context, id int64) (timeweb.Server, error) {
	f.getCalls = append(f.getCalls, id)
	if f.getHook != nil {
		return f.getHook(ctx, id)
	}
	for _, server := range f.servers {
		if server.ID == id {
			return server, nil
		}
	}
	return timeweb.Server{}, &timeweb.APIError{Operation: "get server", StatusCode: http.StatusNotFound}
}

func (f *fakeServerClient) CreateServer(ctx context.Context, request timeweb.CreateServerRequest) (timeweb.Server, error) {
	f.creates = append(f.creates, request)
	if f.createHook != nil {
		return f.createHook(ctx, request)
	}
	return f.acceptCreate(request), nil
}

func (f *fakeServerClient) acceptCreate(request timeweb.CreateServerRequest) timeweb.Server {
	server := timeweb.Server{
		ID:      int64(1000 + len(f.creates)),
		Name:    request.Name,
		Comment: request.Comment,
		Status:  "installing",
	}
	f.servers = append(f.servers, server)
	return server
}

func (f *fakeServerClient) DeleteServer(ctx context.Context, id int64) error {
	f.deletes = append(f.deletes, id)
	if f.deleteHook != nil {
		if err := f.deleteHook(ctx, id); err != nil {
			return err
		}
	}
	f.servers = slices.DeleteFunc(f.servers, func(server timeweb.Server) bool { return server.ID == id })
	return nil
}

func (f *fakeServerClient) PerformAction(_ context.Context, id int64, action string) error {
	f.actions = append(f.actions, recordedAction{id, action})
	return nil
}

func (f *fakeServerClient) SetNATMode(ctx context.Context, id int64, mode string) error {
	f.natChanges = append(f.natChanges, recordedAction{id, mode})
	if f.natHook != nil {
		return f.natHook(ctx, id, mode)
	}
	return nil
}

func testProvider(t *testing.T) (*Provider, *fakeServerClient) {
	t.Helper()
	cfg := config.Config{
		APIURL:                config.DefaultAPIURL,
		Token:                 "timeweb-api-secret-never-send-to-runner",
		Namespace:             "test-namespace",
		NetworkID:             "network-test-private",
		RequestTimeoutSeconds: 5,
	}
	p, err := New(cfg, "test-controller", "v0.1.0-test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeServerClient{}
	p.client = fake
	return p, fake
}

func testBootstrap() params.BootstrapInstance {
	str := func(value string) *string { return &value }
	return params.BootstrapInstance{
		Name:          "garm-test-runner",
		PoolID:        "test-pool",
		OSType:        params.Linux,
		OSArch:        params.Amd64,
		Flavor:        "11",
		Image:         "os:79",
		RepoURL:       "https://github.com/example/project",
		MetadataURL:   "https://garm.example.test/api/v1/metadata",
		CallbackURL:   "https://garm.example.test/api/v1/callbacks",
		InstanceToken: "garm-instance-secret",
		Labels:        []string{"linux", "timeweb"},
		SSHKeys:       []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestFixtureOnly runner-test"},
		Tools: []params.RunnerApplicationDownload{{
			OS:                str("linux"),
			Architecture:      str("x64"),
			DownloadURL:       str("https://github.com/actions/runner/releases/download/v2.321.0/actions-runner-linux-x64-2.321.0.tar.gz"),
			Filename:          str("actions-runner-linux-x64-2.321.0.tar.gz"),
			TempDownloadToken: str("runner-download-secret"),
		}},
	}
}

func fixtureServer(t *testing.T, id int64, namespace, controller, pool, name string) timeweb.Server {
	t.Helper()
	// Spell out the persisted format so changing only the implementation cannot
	// silently broaden the set of resources these tests consider owned.
	comment := mustJSON(t, map[string]any{
		"p": "garm-timeweb", "v": 1, "n": namespace, "c": controller, "l": pool,
	})
	return timeweb.Server{ID: id, Name: name, Comment: string(comment), Status: "on"}
}

func ownServer(t *testing.T, p *Provider, id int64, pool, name string) timeweb.Server {
	t.Helper()
	return fixtureServer(t, id, p.config.Namespace, p.controllerID, pool, name)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertNoMutations(t *testing.T, fake *fakeServerClient) {
	t.Helper()
	if len(fake.creates)+len(fake.deletes)+len(fake.actions)+len(fake.natChanges) != 0 {
		t.Fatalf("unexpected mutations: create=%d delete=%v action=%v nat=%v", len(fake.creates), fake.deletes, fake.actions, fake.natChanges)
	}
}

func TestNewValidatesControllerAndReportsVersion(t *testing.T) {
	p, _ := testProvider(t)
	if got := p.GetVersion(context.Background()); got != "v0.1.0-test" {
		t.Fatalf("version = %q", got)
	}
	for _, id := range []string{"", "bad/controller", "bad controller", "-leading", strings.Repeat("x", 65)} {
		t.Run(id, func(t *testing.T) {
			if _, err := New(p.config, id, "test"); err == nil {
				t.Fatal("accepted invalid controller ID")
			}
		})
	}
}

func TestListInstancesRequiresCompleteOwnershipAndPool(t *testing.T) {
	p, fake := testProvider(t)
	base := ownServer(t, p, 90, "test-pool", "garm-invalid")
	comments := []string{
		`{"p":"different-provider","v":1,"n":"test-namespace","c":"test-controller","l":"test-pool"}`,
		`{"p":"garm-timeweb","v":2,"n":"test-namespace","c":"test-controller","l":"test-pool"}`,
		`{"p":"garm-timeweb","v":1,"n":"test-namespace","c":"test-controller"}`,
		`{"p":"garm-timeweb","v":1,"n":"test-namespace","c":"test-controller","l":"bad/pool"}`,
		`{"p":"garm-timeweb"`,
		strings.Repeat("x", 256),
	}
	fake.servers = []timeweb.Server{
		ownServer(t, p, 8, "test-pool", "garm-second"),
		fixtureServer(t, 2, "another-namespace", p.controllerID, "test-pool", "garm-other-namespace"),
		fixtureServer(t, 3, p.config.Namespace, "another-controller", "test-pool", "garm-other-controller"),
		ownServer(t, p, 4, "another-pool", "garm-other-pool"),
		ownServer(t, p, 1, "test-pool", "garm-first"),
	}
	for i, comment := range comments {
		server := base
		server.ID += int64(i)
		server.Comment = comment
		fake.servers = append(fake.servers, server)
	}
	instances, err := p.ListInstances(context.Background(), "test-pool")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, instance := range instances {
		ids = append(ids, instance.ProviderID)
	}
	if !reflect.DeepEqual(ids, []string{"1", "8"}) {
		t.Fatalf("listed IDs = %v, want only this namespace, controller and pool", ids)
	}
	assertNoMutations(t, fake)
	if _, err := p.ListInstances(context.Background(), "bad/pool"); !errors.Is(err, garmerrors.ErrBadRequest) {
		t.Fatalf("invalid pool error = %v", err)
	}
}

func TestForeignNumericIDsNeverMutate(t *testing.T) {
	for _, scope := range []string{"namespace", "controller", "unmarked"} {
		t.Run(scope, func(t *testing.T) {
			p, fake := testProvider(t)
			server := ownServer(t, p, 42, "test-pool", "garm-foreign")
			switch scope {
			case "namespace":
				server = fixtureServer(t, 42, "foreign", p.controllerID, "test-pool", server.Name)
			case "controller":
				server = fixtureServer(t, 42, p.config.Namespace, "foreign", "test-pool", server.Name)
			case "unmarked":
				server.Comment = "ordinary VM with an unrelated user comment"
			}
			fake.servers = []timeweb.Server{server}
			ctx := context.Background()
			if _, err := p.GetInstance(ctx, "42"); !errors.Is(err, garmerrors.ErrNotFound) {
				t.Fatalf("Get foreign = %v", err)
			}
			if err := p.Start(ctx, "42"); !errors.Is(err, garmerrors.ErrNotFound) {
				t.Fatalf("Start foreign = %v", err)
			}
			for _, force := range []bool{false, true} {
				if err := p.Stop(ctx, "42", force); !errors.Is(err, garmerrors.ErrNotFound) {
					t.Fatalf("Stop foreign force=%v: %v", force, err)
				}
			}
			if err := p.DeleteInstance(ctx, "42"); err != nil {
				t.Fatalf("idempotent Delete foreign = %v", err)
			}
			assertNoMutations(t, fake)
		})
	}
}

func TestMissingInstancesUseGARMNotFoundAndIdempotentDelete(t *testing.T) {
	p, fake := testProvider(t)
	for _, id := range []string{"42", "garm-missing"} {
		t.Run(id, func(t *testing.T) {
			if _, err := p.GetInstance(context.Background(), id); !errors.Is(err, garmerrors.ErrNotFound) {
				t.Fatalf("Get missing = %v, want errors.Is GARM ErrNotFound", err)
			}
			if err := p.DeleteInstance(context.Background(), id); err != nil {
				t.Fatalf("Delete missing = %v", err)
			}
		})
	}
	assertNoMutations(t, fake)
	fake.servers = []timeweb.Server{ownServer(t, p, 12, "test-pool", "garm-raced-delete")}
	fake.deleteHook = func(context.Context, int64) error {
		return fmt.Errorf("already removed: %w", timeweb.ErrNotFound)
	}
	if err := p.DeleteInstance(context.Background(), "12"); err != nil {
		t.Fatalf("Delete raced with removal = %v", err)
	}
}

func TestAmbiguousOwnedNamesRejectEveryOperation(t *testing.T) {
	p, fake := testProvider(t)
	b := testBootstrap()
	fake.servers = []timeweb.Server{
		ownServer(t, p, 1, b.PoolID, b.Name),
		ownServer(t, p, 2, "another-pool", b.Name),
	}
	ctx := context.Background()
	operations := map[string]func() error{
		"get":    func() error { _, err := p.GetInstance(ctx, b.Name); return err },
		"create": func() error { _, err := p.CreateInstance(ctx, b); return err },
		"delete": func() error { return p.DeleteInstance(ctx, b.Name) },
		"start":  func() error { return p.Start(ctx, b.Name) },
		"stop":   func() error { return p.Stop(ctx, b.Name, true) },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, garmerrors.ErrDuplicateEntity) {
				t.Fatalf("ambiguous name error = %v", err)
			}
		})
	}
	assertNoMutations(t, fake)
	if _, err := p.GetInstance(ctx, "1"); err != nil {
		t.Fatalf("unique numeric ID should remain usable: %v", err)
	}
}

func TestFailedCreateIsDiscoverableAndCleanedUpByName(t *testing.T) {
	p, fake := testProvider(t)
	b := testBootstrap()
	fake.createHook = func(_ context.Context, request timeweb.CreateServerRequest) (timeweb.Server, error) {
		fake.acceptCreate(request)
		return timeweb.Server{}, context.DeadlineExceeded
	}
	if _, err := p.CreateInstance(context.Background(), b); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ambiguous create error = %v", err)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("create was replayed %d times", len(fake.creates))
	}
	// A new provider process has no in-memory record of the accepted POST.
	restarted, _ := testProvider(t)
	restarted.client = fake
	got, err := restarted.GetInstance(context.Background(), b.Name)
	if err != nil || got.ProviderID != "1001" {
		t.Fatalf("discover by persisted ownership and name = %#v, %v", got, err)
	}
	if err := restarted.DeleteInstance(context.Background(), b.Name); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deletes, []int64{1001}) || len(fake.servers) != 0 {
		t.Fatalf("failed create cleanup: deleted=%v remaining=%v", fake.deletes, fake.servers)
	}
}

func TestRepeatedCreateReconcilesWithoutAnotherPOST(t *testing.T) {
	for _, loseResponse := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-response=%v", loseResponse), func(t *testing.T) {
			p, fake := testProvider(t)
			b := testBootstrap()
			if loseResponse {
				fake.createHook = func(_ context.Context, request timeweb.CreateServerRequest) (timeweb.Server, error) {
					fake.acceptCreate(request)
					return timeweb.Server{}, context.DeadlineExceeded
				}
			}
			_, firstErr := p.CreateInstance(context.Background(), b)
			if (firstErr != nil) != loseResponse {
				t.Fatalf("first create error = %v", firstErr)
			}
			restarted, _ := testProvider(t)
			restarted.client = fake
			got, err := restarted.CreateInstance(context.Background(), b)
			if err != nil || got.ProviderID != "1001" || got.Name != b.Name {
				t.Fatalf("reconciled create = %#v, %v", got, err)
			}
			if len(fake.creates) != 1 || len(fake.deletes) != 0 {
				t.Fatalf("repeat mutated resources: creates=%d deletes=%v", len(fake.creates), fake.deletes)
			}
		})
	}
}

func TestCreateRejectsNameAlreadyOwnedByAnotherPool(t *testing.T) {
	p, fake := testProvider(t)
	b := testBootstrap()
	fake.servers = []timeweb.Server{ownServer(t, p, 1, "another-pool", b.Name)}
	if _, err := p.CreateInstance(context.Background(), b); !errors.Is(err, garmerrors.ErrDuplicateEntity) {
		t.Fatalf("cross-pool duplicate error = %v", err)
	}
	assertNoMutations(t, fake)
}

func TestRemoveAllContinuesAfterFailureAndExcludesForeignServers(t *testing.T) {
	p, fake := testProvider(t)
	failure := errors.New("delete temporarily unavailable")
	fake.servers = []timeweb.Server{
		ownServer(t, p, 1, "pool-one", "garm-fails"),
		fixtureServer(t, 2, p.config.Namespace, "other-controller", "pool-one", "garm-foreign-controller"),
		ownServer(t, p, 3, "pool-two", "garm-succeeds"),
		fixtureServer(t, 4, "other-namespace", p.controllerID, "pool-one", "garm-foreign-namespace"),
		{ID: 5, Name: "ordinary-vm", Comment: "not managed by GARM"},
	}
	fake.deleteHook = func(_ context.Context, id int64) error {
		if id == 1 {
			return failure
		}
		return nil
	}
	if err := p.RemoveAllInstances(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("RemoveAll error = %v", err)
	}
	if !reflect.DeepEqual(fake.deletes, []int64{1, 3}) {
		t.Fatalf("RemoveAll deletion attempts = %v", fake.deletes)
	}
	if !reflect.DeepEqual(fake.getCalls, []int64{1, 3}) {
		t.Fatalf("RemoveAll must recheck ownership before deleting: gets=%v", fake.getCalls)
	}
	var remaining []int64
	for _, server := range fake.servers {
		remaining = append(remaining, server.ID)
	}
	if !reflect.DeepEqual(remaining, []int64{1, 2, 4, 5}) {
		t.Fatalf("remaining servers = %v", remaining)
	}
}

func TestListFailurePreventsCreateAndRemoveAll(t *testing.T) {
	p, fake := testProvider(t)
	fake.listErr = errors.New("cannot establish account inventory")
	if _, err := p.CreateInstance(context.Background(), testBootstrap()); !errors.Is(err, fake.listErr) {
		t.Fatalf("Create after list failure = %v", err)
	}
	if err := p.RemoveAllInstances(context.Background()); !errors.Is(err, fake.listErr) {
		t.Fatalf("RemoveAll after list failure = %v", err)
	}
	assertNoMutations(t, fake)
}

func TestPrivateCreateIncludesAtomicOwnershipAndNoFloatingIP(t *testing.T) {
	p, fake := testProvider(t)
	b := testBootstrap()
	p.config.Namespace = strings.Repeat("n", 64)
	p.controllerID = strings.Repeat("c", 64)
	b.PoolID = strings.Repeat("p", 64)
	p.config.ProjectID = 32
	p.config.AvailabilityZone = "msk-1"
	p.config.SSHKeyIDs = []int64{7, 9}
	got, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.creates) != 1 || len(fake.natChanges) != 0 {
		t.Fatalf("private create calls: create=%d nat=%v", len(fake.creates), fake.natChanges)
	}
	request := fake.creates[0]
	if request.Name != b.Name || request.Hostname != b.Name || request.PresetID != 11 || request.OSID != 79 || request.ImageID != "" {
		t.Fatalf("create fields = %#v", request)
	}
	if got.Name != b.Name || got.ProviderID != "1001" {
		t.Fatalf("create instance = %#v", got)
	}
	if request.ProjectID != 32 || request.AvailabilityZone != "msk-1" || !reflect.DeepEqual(request.SSHKeysIDs, []int64{7, 9}) {
		t.Fatal("create did not preserve project, zone and existing SSH key IDs")
	}
	if len(request.Comment) >= 255 {
		t.Fatalf("maximum ownership comment has %d bytes", len(request.Comment))
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(request.Comment), &marker); err != nil {
		t.Fatalf("ownership missing from initial POST: %v", err)
	}
	wantMarker := map[string]any{"p": "garm-timeweb", "v": float64(1), "n": p.config.Namespace, "c": p.controllerID, "l": b.PoolID}
	if !reflect.DeepEqual(marker, wantMarker) {
		t.Fatalf("atomic ownership = %v", marker)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, request), &payload); err != nil {
		t.Fatal(err)
	}
	var network map[string]json.RawMessage
	if err := json.Unmarshal(payload["network"], &network); err != nil {
		t.Fatal(err)
	}
	if _, exists := network["floating_ip"]; exists {
		t.Fatal("private creation sent floating_ip; omission is required to avoid allocation")
	}
	if string(network["id"]) != strconv.Quote(p.config.NetworkID) || len(network) != 1 {
		t.Fatalf("private network payload = %s", payload["network"])
	}
	if strings.Contains(string(mustJSON(t, request)), p.config.Token) {
		t.Fatal("Timeweb API token leaked into VM create payload")
	}
	if strings.Contains(string(mustJSON(t, got)), b.InstanceToken) || strings.Contains(request.Comment, b.InstanceToken) {
		t.Fatal("bootstrap token leaked into returned instance or persisted ownership")
	}
}

func TestCreateSelectsOSOrCustomImage(t *testing.T) {
	for _, tc := range []struct {
		image   string
		osID    int64
		imageID string
	}{
		{"os:79", 79, ""},
		{"image:01234567-89ab-cdef-0123-456789abcdef", 0, "01234567-89ab-cdef-0123-456789abcdef"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			p, fake := testProvider(t)
			b := testBootstrap()
			b.Image = tc.image
			if _, err := p.CreateInstance(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			request := fake.creates[0]
			if request.OSID != tc.osID || request.ImageID != tc.imageID {
				t.Fatalf("OS/image selection = %d/%q", request.OSID, request.ImageID)
			}
		})
	}
}

func TestInvalidBootstrapRejectedBeforeAnyAPICall(t *testing.T) {
	mutations := map[string]func(*params.BootstrapInstance){
		"numeric-name":       func(b *params.BootstrapInstance) { b.Name = "12345" },
		"empty-name":         func(b *params.BootstrapInstance) { b.Name = "" },
		"long-name":          func(b *params.BootstrapInstance) { b.Name = strings.Repeat("x", 64) },
		"invalid-name":       func(b *params.BootstrapInstance) { b.Name = "runner;id" },
		"trailing-hyphen":    func(b *params.BootstrapInstance) { b.Name = "runner-" },
		"invalid-pool":       func(b *params.BootstrapInstance) { b.PoolID = "pool/other" },
		"windows":            func(b *params.BootstrapInstance) { b.OSType = params.Windows },
		"arm64":              func(b *params.BootstrapInstance) { b.OSArch = params.Arm64 },
		"zero-preset":        func(b *params.BootstrapInstance) { b.Flavor = "0" },
		"negative-preset":    func(b *params.BootstrapInstance) { b.Flavor = "-1" },
		"signed-preset":      func(b *params.BootstrapInstance) { b.Flavor = "+1" },
		"spaced-preset":      func(b *params.BootstrapInstance) { b.Flavor = " 11" },
		"overflow-preset":    func(b *params.BootstrapInstance) { b.Flavor = "9223372036854775808" },
		"missing-image-kind": func(b *params.BootstrapInstance) { b.Image = "79" },
		"bad-image-kind":     func(b *params.BootstrapInstance) { b.Image = "other:79" },
		"zero-os":            func(b *params.BootstrapInstance) { b.Image = "os:0" },
		"bad-custom-image":   func(b *params.BootstrapInstance) { b.Image = "image:not-a-uuid" },
		"missing-tools":      func(b *params.BootstrapInstance) { b.Tools = nil },
		"wrong-tools-os":     func(b *params.BootstrapInstance) { value := "windows"; b.Tools[0].OS = &value },
		"wrong-tools-arch":   func(b *params.BootstrapInstance) { value := "arm64"; b.Tools[0].Architecture = &value },
		"missing-tools-url":  func(b *params.BootstrapInstance) { b.Tools[0].DownloadURL = nil },
		"missing-tools-file": func(b *params.BootstrapInstance) { b.Tools[0].Filename = nil },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, fake := testProvider(t)
			b := testBootstrap()
			mutate(&b)
			if _, err := p.CreateInstance(context.Background(), b); !errors.Is(err, garmerrors.ErrBadRequest) {
				t.Fatalf("invalid bootstrap error = %v", err)
			}
			if fake.listCalls != 0 || len(fake.getCalls) != 0 {
				t.Fatal("invalid bootstrap made API reads before validation")
			}
			assertNoMutations(t, fake)
		})
	}
}

type decodedCloudInit struct {
	SSHKeys        []string `yaml:"ssh_authorized_keys"`
	PackageUpgrade bool     `yaml:"package_upgrade"`
	Packages       []string `yaml:"packages"`
	RunCmd         []string `yaml:"runcmd"`
	Files          []struct {
		Path        string `yaml:"path"`
		Encoding    string `yaml:"encoding"`
		Content     string `yaml:"content"`
		Permissions string `yaml:"permissions"`
	} `yaml:"write_files"`
}

func parseCloudInit(t *testing.T, userData string) (decodedCloudInit, map[string]string) {
	t.Helper()
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatal("userdata is not a cloud-init document")
	}
	var parsed decodedCloudInit
	if err := yaml.Unmarshal([]byte(userData), &parsed); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range parsed.Files {
		if file.Encoding != "b64" {
			t.Fatalf("unsupported cloud-init file encoding %q", file.Encoding)
		}
		decoded, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			t.Fatal(err)
		}
		files[file.Path] = string(decoded)
	}
	return parsed, files
}

func TestCloudInitPreservesJITAndRegistrationBootstrap(t *testing.T) {
	for _, jit := range []bool{false, true} {
		t.Run(fmt.Sprintf("jit=%v", jit), func(t *testing.T) {
			p, fake := testProvider(t)
			b := testBootstrap()
			b.JitConfigEnabled = jit
			b.UserDataOptions = params.UserDataOptions{DisableUpdatesOnBoot: true, ExtraPackages: []string{"git"}}
			if _, err := p.CreateInstance(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			cloudInit, files := parseCloudInit(t, fake.creates[0].CloudInit)
			script := files["/install_runner.sh"]
			if script == "" {
				t.Fatal("runner installer missing")
			}
			for _, required := range []string{b.MetadataURL, b.CallbackURL, b.InstanceToken, b.Tools[0].GetDownloadURL(), b.Tools[0].GetTempDownloadToken()} {
				if !strings.Contains(script, required) {
					t.Fatal("runner installer omitted a bootstrap URL or credential")
				}
			}
			if strings.Contains(script, p.config.Token) {
				t.Fatal("Timeweb management token leaked into guest installer")
			}
			if strings.Contains(script, "credentials/runner") != jit || strings.Contains(script, "runner-registration-token/") == jit {
				t.Fatal("runner installer selected the wrong JIT/registration flow")
			}
			if !reflect.DeepEqual(cloudInit.SSHKeys, b.SSHKeys) || cloudInit.PackageUpgrade || !reflect.DeepEqual(cloudInit.Packages, []string{"git"}) {
				t.Fatalf("SSH keys or user data options were lost: %#v", cloudInit)
			}
			if !slices.Contains(cloudInit.RunCmd, "su -l -c /install_runner.sh runner") {
				t.Fatal("cloud-init never invokes the runner installer as runner")
			}
		})
	}
}

func TestExtraSpecsSupportTemplateAndPerPoolOverrides(t *testing.T) {
	p, fake := testProvider(t)
	b := testBootstrap()
	b.JitConfigEnabled = true
	b.ExtraSpecs = mustJSON(t, map[string]any{
		"network_id":              "network-pool-specific",
		"network_mode":            "no_nat",
		"availability_zone":       "spb-3",
		"project_id":              45,
		"ssh_key_ids":             []int64{8, 10},
		"runner_install_template": []byte("#!/bin/sh\necho {{ .RunnerName }} {{ .UseJITConfig }} {{ .ExtraContext.test_value }}\n"),
		"extra_context":           map[string]string{"test_value": "custom-value"},
		"pre_install_scripts": map[string][]byte{
			"02-second.sh": []byte("#!/bin/sh\necho second\n"),
			"01-first.sh":  []byte("#!/bin/sh\necho first\n"),
		},
	})
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	request := fake.creates[0]
	if request.Network.ID != "network-pool-specific" || request.AvailabilityZone != "spb-3" || request.ProjectID != 45 || !reflect.DeepEqual(request.SSHKeysIDs, []int64{8, 10}) {
		t.Fatal("per-pool Timeweb overrides were not applied")
	}
	if !reflect.DeepEqual(fake.natChanges, []recordedAction{{1001, "no_nat"}}) {
		t.Fatalf("network override calls = %v", fake.natChanges)
	}
	if p.config.NetworkID != "network-test-private" || p.config.NetworkMode != "" || p.config.ProjectID != 0 || len(p.config.SSHKeyIDs) != 0 {
		t.Fatal("per-pool overrides modified shared provider configuration")
	}
	cloudInit, files := parseCloudInit(t, request.CloudInit)
	if got := files["/install_runner.sh"]; got != "#!/bin/sh\necho garm-test-runner true custom-value\n" {
		t.Fatalf("custom runner template output = %q", got)
	}
	wantPrefix := []string{"/garm-pre-install/01-first.sh", "/garm-pre-install/02-second.sh"}
	if len(cloudInit.RunCmd) < 2 || !reflect.DeepEqual(cloudInit.RunCmd[:2], wantPrefix) {
		t.Fatalf("pre-install execution order = %v", cloudInit.RunCmd)
	}
	for _, name := range wantPrefix {
		if files[name] == "" {
			t.Fatalf("pre-install script %s missing from cloud-init", name)
		}
	}
}

func TestExtraSpecsAreStrictAndErrorsDoNotEchoValues(t *testing.T) {
	const secret = "value-that-must-not-appear-in-errors"
	invalid := []string{
		`null`, ` null `, `[]`, `true`, `42`, `"text"`, `{`,
		`{} {}`, `{} trailing-data`,
		`{"unknown":"` + secret + `"}`,
		`{"token":"` + secret + `"}`,
		`{"floating_ip":"create_ip"}`,
		`{"network_id":""}`,
		`{"network_mode":"dnat_and_snat"}`,
		`{"project_id":-1}`,
		`{"ssh_key_ids":[0]}`,
		`{"ssh_key_ids":"` + secret + `"}`,
		`{"runner_install_template":"not base64: ` + secret + `"}`,
	}
	for i, extra := range invalid {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			p, fake := testProvider(t)
			b := testBootstrap()
			b.ExtraSpecs = json.RawMessage(extra)
			_, err := p.CreateInstance(context.Background(), b)
			if !errors.Is(err, garmerrors.ErrBadRequest) {
				t.Fatalf("invalid extra_specs error = %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("extra_specs error exposed a supplied value")
			}
			assertNoMutations(t, fake)
		})
	}
	t.Run("template-render-error", func(t *testing.T) {
		p, fake := testProvider(t)
		b := testBootstrap()
		b.ExtraSpecs = mustJSON(t, map[string]any{
			"runner_install_template": []byte("{{ functionThatDoesNotExist \"" + secret + "\" }}"),
		})
		_, err := p.CreateInstance(context.Background(), b)
		if !errors.Is(err, garmerrors.ErrBadRequest) || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe template error = %v", err)
		}
		assertNoMutations(t, fake)
	})
}

func TestAPIBodySecretsDoNotEscapeCreateError(t *testing.T) {
	// This local HTTP fixture verifies the provider/client boundary as well as
	// the fake-based lifecycle tests; it never contacts Timeweb or GitHub.
	const responseSecret = "root-password-or-echoed-registration-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"servers":[]}`))
		case http.MethodPost:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"message":%q,"root_pass":%q}`, responseSecret, responseSecret)
		default:
			t.Errorf("unexpected HTTP method %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	p, _ := testProvider(t)
	client, err := timeweb.NewClient(server.URL, p.config.Token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	p.client = client
	_, err = p.CreateInstance(context.Background(), testBootstrap())
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("create failure = %v", err)
	}
	for _, secret := range []string{responseSecret, p.config.Token, testBootstrap().InstanceToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("create error exposed a credential or server response body")
		}
	}
}

func TestFailedNATSetupRollsBackEvenWithCanceledContext(t *testing.T) {
	for _, scenario := range []string{"ordinary-failure", "canceled", "locked-and-canceled", "rollback-fails", "rollback-already-deleted"} {
		t.Run(scenario, func(t *testing.T) {
			p, fake := testProvider(t)
			p.config.NetworkMode = "no_nat"
			type contextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "cleanup-trace"))
			defer cancel()
			networkFailure := errors.New("network setup failed")
			rollbackFailure := errors.New("rollback failed")
			fake.natHook = func(context.Context, int64, string) error {
				if scenario == "canceled" || scenario == "locked-and-canceled" {
					cancel()
					if scenario == "locked-and-canceled" {
						return &timeweb.APIError{Operation: "set NAT", StatusCode: http.StatusLocked}
					}
					return context.Canceled
				}
				return networkFailure
			}
			fake.deleteHook = func(cleanupCtx context.Context, id int64) error {
				if cleanupCtx.Err() != nil {
					t.Fatalf("rollback inherited cancellation: %v", cleanupCtx.Err())
				}
				if _, ok := cleanupCtx.Deadline(); !ok {
					t.Fatal("rollback has no bounded timeout")
				}
				if cleanupCtx.Value(contextKey{}) != "cleanup-trace" || id != 1001 {
					t.Fatal("rollback lost context values or selected the wrong server")
				}
				switch scenario {
				case "rollback-fails":
					return rollbackFailure
				case "rollback-already-deleted":
					return timeweb.ErrNotFound
				default:
					return nil
				}
			}
			_, err := p.CreateInstance(ctx, testBootstrap())
			want := networkFailure
			if scenario == "canceled" || scenario == "locked-and-canceled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("network error chain = %v, want %v", err, want)
			}
			if scenario == "rollback-fails" && !errors.Is(err, rollbackFailure) {
				t.Fatalf("lost rollback error: %v", err)
			}
			if !reflect.DeepEqual(fake.deletes, []int64{1001}) || len(fake.creates) != 1 || len(fake.natChanges) != 1 {
				t.Fatalf("rollback calls: deletes=%v creates=%d nat=%v", fake.deletes, len(fake.creates), fake.natChanges)
			}
		})
	}
}

func TestExistingNATConfigurationDoesNotNeedAnotherMutation(t *testing.T) {
	p, fake := testProvider(t)
	p.config.NetworkMode = "no_nat"
	b := testBootstrap()
	server := ownServer(t, p, 4, b.PoolID, b.Name)
	server.Networks = []timeweb.ServerNetwork{{ID: p.config.NetworkID, Type: "private", NATMode: "no_nat"}}
	fake.servers = []timeweb.Server{server}
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	assertNoMutations(t, fake)
}

func TestStartStopUseTimewebActionsAndAvoidCompletedTransitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		start  bool
		force  bool
		action string
	}{
		{"start", "off", true, false, "start"},
		{"already-running", "on", true, false, ""},
		{"stop", "on", false, false, "shutdown"},
		{"hard-stop", "on", false, true, "hard_shutdown"},
		{"already-stopped", "off", false, false, ""},
		{"already-hard-stopped", "off", false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := testProvider(t)
			server := ownServer(t, p, 7, "test-pool", "garm-action")
			server.Status = tc.status
			fake.servers = []timeweb.Server{server}
			var err error
			if tc.start {
				err = p.Start(context.Background(), "7")
			} else {
				err = p.Stop(context.Background(), "garm-action", tc.force)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.action == "" {
				assertNoMutations(t, fake)
			} else if !reflect.DeepEqual(fake.actions, []recordedAction{{7, tc.action}}) {
				t.Fatalf("actions = %v", fake.actions)
			}
		})
	}
}

func TestProviderReportsOnlyGARMCompatibleStatuses(t *testing.T) {
	statuses := []string{
		"on", "off", "fail", "", "installing", "software_install", "reinstalling", "turning_on", "turning_off", "hard_turning_off", "rebooting", "hard_rebooting", "removing", "removed", "cloning", "transfer", "blocked", "configuring", "no_paid", "permanent_blocked", "future-api-status",
	}
	allowed := []params.InstanceStatus{params.InstanceRunning, params.InstanceStopped, params.InstanceError, params.InstanceStatusUnknown}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			p, fake := testProvider(t)
			server := ownServer(t, p, 42, "test-pool", "garm-status")
			server.Status = status
			fake.servers = []timeweb.Server{server}
			got, err := p.GetInstance(context.Background(), "42")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(allowed, got.Status) {
				t.Fatalf("Timeweb status %q mapped to unsupported GARM status %q", status, got.Status)
			}
			want := params.InstanceStatusUnknown
			switch status {
			case "on":
				want = params.InstanceRunning
			case "off":
				want = params.InstanceStopped
			case "fail":
				want = params.InstanceError
			}
			if got.Status != want || (len(got.ProviderFault) > 0) != (want == params.InstanceError) {
				t.Fatalf("status/fault = %q/%q, want %q", got.Status, got.ProviderFault, want)
			}
		})
	}
}

func TestInstanceAddressesAreTypedValidatedAndDeduplicated(t *testing.T) {
	p, fake := testProvider(t)
	server := ownServer(t, p, 9007199254740993, "test-pool", "garm-addresses")
	server.OS = timeweb.ServerOS{ID: 79, Name: "ubuntu", Version: "24.04"}
	server.Networks = []timeweb.ServerNetwork{
		{Type: "private", IPs: []timeweb.IPAddress{{IP: "10.0.0.4"}, {IP: "10.0.0.4"}, {IP: "bad address"}}},
		{Type: "public", IPs: []timeweb.IPAddress{{IP: "198.51.100.8"}, {IP: "2001:db8::1"}, {IP: "2001:0db8:0:0:0:0:0:1"}}},
	}
	fake.servers = []timeweb.Server{server}
	got, err := p.GetInstance(context.Background(), "9007199254740993")
	if err != nil {
		t.Fatal(err)
	}
	want := []params.Address{
		{Address: "10.0.0.4", Type: params.PrivateAddress},
		{Address: "198.51.100.8", Type: params.PublicAddress},
		{Address: "2001:db8::1", Type: params.PublicAddress},
	}
	if !reflect.DeepEqual(got.Addresses, want) {
		t.Fatalf("addresses = %#v", got.Addresses)
	}
	if got.ProviderID != "9007199254740993" || got.OSType != params.Linux || got.OSArch != params.Amd64 || got.OSName != "ubuntu" || got.OSVersion != "24.04" {
		t.Fatalf("instance identity/OS = %#v", got)
	}
}
