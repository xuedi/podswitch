package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuantities(t *testing.T) {
	cpu := map[string]int64{"": 0, "4": 4000, "0.5": 500, "250m": 250, "123456789n": 123, "1500u": 1}
	for q, want := range cpu {
		if got := cpuMilli(q); got != want {
			t.Errorf("cpuMilli(%q) = %d, want %d", q, got, want)
		}
	}
	mem := map[string]int64{"": 0, "1234": 1234, "2Ki": 2048, "512Mi": 512 << 20, "1Gi": 1 << 30, "1G": 1e9, "3k": 3000}
	for q, want := range mem {
		if got := bytesOf(q); got != want {
			t.Errorf("bytesOf(%q) = %d, want %d", q, got, want)
		}
	}
}

func TestState(t *testing.T) {
	cases := []struct {
		want, ready int32
		pods        int
		state       string
	}{{0, 0, 0, "stopped"}, {0, 0, 1, "stopping"}, {1, 0, 1, "starting"}, {1, 0, 0, "starting"}, {1, 1, 1, "running"}}
	for _, c := range cases {
		if got := state(c.want, c.ready, c.pods); got != c.state {
			t.Errorf("state(%d, %d, %d) = %s, want %s", c.want, c.ready, c.pods, got, c.state)
		}
	}
}

func TestMatches(t *testing.T) {
	if matches(nil, map[string]string{"a": "b"}) {
		t.Error("an empty selector must match nothing")
	}
	if !matches(map[string]string{"app": "x"}, map[string]string{"app": "x", "other": "y"}) {
		t.Error("subset should match")
	}
	if matches(map[string]string{"app": "x"}, map[string]string{"app": "z"}) {
		t.Error("different value should not match")
	}
}

// fakeAPI serves canned answers for the paths podswitch reads, and records scale calls.
func fakeAPI(t *testing.T, scaled *[]string) *httptest.Server {
	t.Helper()
	answers := map[string]string{
		"/api/v1/nodes": `{"items":[{"metadata":{"name":"box"},"status":{
			"capacity":{"cpu":"4","memory":"16Gi"},
			"conditions":[{"type":"Ready","status":"True"}],
			"nodeInfo":{"kubeletVersion":"v1.36.4","osImage":"Debian","kernelVersion":"6.12"}}}]}`,
		"/apis/metrics.k8s.io/v1beta1/nodes": `{"items":[{"usage":{"cpu":"500000000n","memory":"2Gi"}}]}`,
		"/api/v1/pods": `{"items":[
			{"metadata":{"name":"web-0","namespace":"games","labels":{"app":"web"}}},
			{"metadata":{"name":"dns-1","namespace":"kube-system","labels":{"k8s-app":"dns"}}}]}`,
		"/apis/metrics.k8s.io/v1beta1/pods": `{"items":[
			{"metadata":{"name":"web-0","namespace":"games"},"containers":[{"usage":{"cpu":"250m","memory":"1Gi"}},{"usage":{"cpu":"50m","memory":"512Mi"}}]}]}`,
		"/apis/apps/v1/statefulsets": `{"items":[
			{"metadata":{"name":"web","namespace":"games","annotations":{"podswitch/title":"Web World","podswitch/port":"25565"}},
			 "spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}}},"status":{"readyReplicas":1}},
			{"metadata":{"name":"secret","namespace":"games","annotations":{"podswitch/hide":"true"}},
			 "spec":{"replicas":0,"selector":{"matchLabels":{"app":"secret"}}}}]}`,
		"/apis/apps/v1/deployments": `{"items":[
			{"metadata":{"name":"coredns","namespace":"kube-system"},"spec":{"replicas":1,"selector":{"matchLabels":{"k8s-app":"dns"}}},"status":{"readyReplicas":1}},
			{"metadata":{"name":"blog","namespace":"web"},"spec":{"replicas":0,"selector":{"matchLabels":{"app":"blog"}}}}]}`,
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"message":"no token"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == "PATCH" && strings.HasSuffix(r.URL.Path, "/scale") {
			var body struct{ Spec struct{ Replicas int } }
			json.NewDecoder(r.Body).Decode(&body)
			*scaled = append(*scaled, r.URL.Path+"="+string(rune('0'+body.Spec.Replicas)))
			w.Write([]byte(`{}`))
			return
		}
		a, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(a))
	}))
}

func testConfig(api string) config {
	c := config{api: api, host: "10.0.0.5", hidden: map[string]bool{}}
	for _, ns := range strings.Split(defaultHidden, ",") {
		c.hidden[ns] = true
	}
	return c
}

func TestView(t *testing.T) {
	var scaled []string
	srv := fakeAPI(t, &scaled)
	defer srv.Close()
	k := &kube{api: srv.URL, token: "test-token", http: srv.Client()}

	v := k.view(testConfig(srv.URL))
	if !v.Online || v.Error != "" {
		t.Fatalf("online=%v error=%q", v.Online, v.Error)
	}
	if v.Name != "box" {
		t.Errorf("name defaults to the first node: got %q", v.Name)
	}
	c := v.Cluster
	if c.Nodes != 1 || c.ReadyNodes != 1 || c.CPUCapMilli != 4000 || c.MemCap != 16<<30 || c.CPUMilli != 500 || c.Mem != 2<<30 {
		t.Errorf("cluster = %+v", c)
	}
	if len(v.Apps) != 2 {
		t.Fatalf("want 2 apps (system namespace and hidden one left out), got %+v", v.Apps)
	}
	web := v.Apps[0]
	if web.Title != "Web World" || web.Address != "10.0.0.5:25565" || web.State != "running" ||
		web.CPUMilli != 300 || web.Mem != 1<<30+512<<20 || web.Kind != "statefulsets" {
		t.Errorf("web = %+v", web)
	}
	blog := v.Apps[1]
	if blog.Title != "blog" || blog.State != "stopped" || blog.Address != "" || blog.Kind != "deployments" {
		t.Errorf("blog = %+v", blog)
	}
}

func TestOffline(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	client := srv.Client()
	srv.Close() // nothing listens any more
	k := &kube{api: srv.URL, token: "x", http: client}
	if v := k.view(testConfig(srv.URL)); v.Online {
		t.Error("unreachable API must read as offline")
	}
}

func TestScaleEndpoint(t *testing.T) {
	var scaled []string
	srv := fakeAPI(t, &scaled)
	defer srv.Close()
	k := &kube{api: srv.URL, token: "test-token", http: srv.Client()}
	cfg := testConfig(srv.URL)
	snap := newSnapshot(func() View { return k.view(cfg) }, time.Hour, time.Hour)
	h := routes(cfg, snap, k.scale, nil)

	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/scale", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ok := url.Values{"ns": {"games"}, "kind": {"statefulsets"}, "name": {"web"}, "replicas": {"0"}}
	if w := post(ok); w.Code != http.StatusNoContent {
		t.Errorf("valid request: %d %s", w.Code, w.Body)
	}
	if len(scaled) != 1 || scaled[0] != "/apis/apps/v1/namespaces/games/statefulsets/web/scale=0" {
		t.Errorf("scale calls = %v", scaled)
	}
	for name, form := range map[string]url.Values{
		"system namespace": {"ns": {"kube-system"}, "kind": {"deployments"}, "name": {"coredns"}, "replicas": {"0"}},
		"replicas 5":       {"ns": {"games"}, "kind": {"statefulsets"}, "name": {"web"}, "replicas": {"5"}},
		"other kind":       {"ns": {"games"}, "kind": {"daemonsets"}, "name": {"web"}, "replicas": {"1"}},
	} {
		if w := post(form); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, w.Code)
		}
	}
	if len(scaled) != 1 {
		t.Errorf("refused requests must not reach the API: %v", scaled)
	}

	failing := routes(cfg, snap, func(string, string, string, int) error { return errors.New("forbidden") }, nil)
	r := httptest.NewRequest("POST", "/scale", strings.NewReader(ok.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	failing.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "forbidden") {
		t.Errorf("API error: %d %q", w.Code, w.Body)
	}
}

func TestPageAndState(t *testing.T) {
	var scaled []string
	srv := fakeAPI(t, &scaled)
	defer srv.Close()
	k := &kube{api: srv.URL, token: "test-token", http: srv.Client()}
	cfg := testConfig(srv.URL)
	h := routes(cfg, newSnapshot(func() View { return k.view(cfg) }, time.Hour, time.Hour), k.scale, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `fetch("state"`) {
		t.Errorf("page: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"/state"`) || strings.Contains(w.Body.String(), `"/scale"`) ||
		strings.Contains(w.Body.String(), `"/power-on"`) {
		t.Error("page must use relative URLs only, it runs behind proxy prefixes")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/state", nil))
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || !v.Online || len(v.Apps) != 2 || v.Version != version {
		t.Errorf("state: %v %+v", err, v)
	}
}

// offlineKube is a kube whose API server is gone.
func offlineKube(t *testing.T) *kube {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	client := srv.Client()
	srv.Close()
	return &kube{api: srv.URL, token: "x", http: client}
}

func postPowerOn(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/power-on", nil))
	return w
}

func TestPowerOnRequest(t *testing.T) {
	var got []string
	bmc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+user+":"+pass+" "+r.Header.Get("Content-Type")+" "+b.String())
	}))
	defer bmc.Close()
	auth := filepath.Join(t.TempDir(), "auth")
	os.WriteFile(auth, []byte("podswitch:s3cret\n"), 0o600)
	power := &powerOn{url: bmc.URL + "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset/", method: "POST",
		body: `{"ResetType":"On"}`, contentType: "application/json", authFile: auth, insecure: true}

	k := offlineKube(t)
	cfg := testConfig(k.api)
	snap := newSnapshot(func() View { return withPower(k.view(cfg), power) }, time.Hour, time.Hour)
	h := routes(cfg, snap, k.scale, power)

	var v View
	json.Unmarshal(snap.get(), &v)
	if !v.PowerOn || v.PowerOnSent != "" {
		t.Errorf("before: %+v", v)
	}
	if w := postPowerOn(h); w.Code != http.StatusNoContent {
		t.Fatalf("power-on: %d %s", w.Code, w.Body)
	}
	want := `POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset/ podswitch:s3cret application/json {"ResetType":"On"}`
	if len(got) != 1 || got[0] != want {
		t.Errorf("BMC got %q", got)
	}
	json.Unmarshal(snap.get(), &v)
	if v.PowerOnSent == "" {
		t.Error("the view must show that a power-on was sent")
	}
	if w := postPowerOn(h); w.Code != http.StatusBadGateway || len(got) != 1 {
		t.Errorf("second press within the cooldown: %d, BMC calls %d", w.Code, len(got))
	}
}

func TestPowerOnRefused(t *testing.T) {
	var scaled []string
	srv := fakeAPI(t, &scaled)
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "ran")
	power := &powerOn{command: "touch " + marker}

	online := &kube{api: srv.URL, token: "test-token", http: srv.Client()}
	cfg := testConfig(srv.URL)
	h := routes(cfg, newSnapshot(func() View { return online.view(cfg) }, time.Hour, time.Hour), online.scale, power)
	if w := postPowerOn(h); w.Code != http.StatusConflict {
		t.Errorf("cluster online: want 409, got %d", w.Code)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command must not run while the cluster is online")
	}

	k := offlineKube(t)
	h = routes(cfg, newSnapshot(func() View { return k.view(cfg) }, time.Hour, time.Hour), k.scale, nil)
	if w := postPowerOn(h); w.Code != http.StatusNotFound {
		t.Errorf("not configured: want 404, got %d", w.Code)
	}
}

func TestPowerOnCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	if err := (&powerOn{command: "touch " + marker}).run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("command did not run")
	}
	err := (&powerOn{command: "echo no route >&2; exit 3"}).run()
	if err == nil || !strings.Contains(err.Error(), "no route") {
		t.Errorf("a failing command must report its output: %v", err)
	}
}

func TestLoadPowerOn(t *testing.T) {
	if p, err := loadPowerOn(); p != nil || err != nil {
		t.Errorf("unset: %v %v", p, err)
	}
	t.Setenv("PODSWITCH_POWER_ON_URL", "https://bmc/")
	t.Setenv("PODSWITCH_POWER_ON_CMD", "wakeonlan aa:bb")
	if _, err := loadPowerOn(); err == nil {
		t.Error("both set must be an error")
	}
}
