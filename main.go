// podswitch: a tiny web page with on/off switches for the apps in a Kubernetes cluster.
//
// It lists every Deployment and StatefulSet outside the system namespaces with its state and
// usage, plus the cluster's CPU and memory, and turns apps on and off by setting their replicas
// to 1 or 0. It runs outside the cluster, so it can say "offline" when the cluster is switched
// off. It talks to the API with a token of its own that may only read and scale (deploy/rbac.yaml).
// Standard library only. All links are relative, so it works behind any reverse-proxy prefix.
package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // container images often have no zoneinfo; TZ still works
)

const version = "0.2.0"

// Annotations on a Deployment or StatefulSet that change how podswitch shows it.
const (
	annotationTitle = "podswitch/title" // display name instead of the object's name
	annotationPort  = "podswitch/port"  // shown as <host>:<port>, where to reach the app
	annotationHide  = "podswitch/hide"  // "true": not listed
)

//go:embed web/page.html
var page []byte

type config struct {
	api, token, ca, listen string
	name                   string          // shown in the header; default: the first node's name
	host                   string          // shown with an app's port; default: the API's host
	hidden                 map[string]bool // namespaces never listed nor scaled
}

const defaultHidden = "kube-system,kube-public,kube-node-lease,local-path-storage,podswitch"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	c := config{
		api:    env("PODSWITCH_API", ""),
		token:  env("PODSWITCH_TOKEN", "/data/token"),
		ca:     env("PODSWITCH_CA", "/data/ca.crt"),
		listen: env("PODSWITCH_LISTEN", ":8080"),
		name:   env("PODSWITCH_NAME", ""),
		host:   env("PODSWITCH_HOST", ""),
		hidden: map[string]bool{},
	}
	if c.api == "" {
		log.Fatal("PODSWITCH_API is not set, e.g. https://192.168.1.10:6443")
	}
	if c.host == "" {
		if u, err := url.Parse(c.api); err == nil {
			c.host = u.Hostname()
		}
	}
	for _, ns := range strings.Split(env("PODSWITCH_HIDE_NAMESPACES", defaultHidden), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			c.hidden[ns] = true
		}
	}
	return c
}

// withPower adds the power-on state to a view, for the offline page.
func withPower(v View, power *powerOn) View {
	if power != nil {
		v.PowerOn = true
		if t, ok := power.since(); ok {
			v.PowerOnSent = t.Format("15:04:05")
		}
	}
	return v
}

// routes wires the endpoints. scale is called by POST /scale, power (nil when not configured)
// by POST /power-on.
func routes(cfg config, snap *snapshot, scale func(ns, kind, name string, replicas int) error, power *powerOn) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(snap.get())
	})
	// Called by the page with fetch(): 204 on success, an error text otherwise.
	mux.HandleFunc("POST /scale", func(w http.ResponseWriter, r *http.Request) {
		ns, kind, name := r.FormValue("ns"), r.FormValue("kind"), r.FormValue("name")
		replicas, err := strconv.Atoi(r.FormValue("replicas"))
		switch {
		case err != nil || (replicas != 0 && replicas != 1):
			http.Error(w, "invalid replica count", http.StatusBadRequest)
		case kind != "statefulsets" && kind != "deployments":
			http.Error(w, "invalid kind", http.StatusBadRequest)
		case cfg.hidden[ns] || ns == "" || name == "":
			http.Error(w, "not an app", http.StatusBadRequest)
		default:
			if err := scale(ns, kind, name, replicas); err != nil {
				log.Print(err)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			log.Printf("%s %s/%s/%s", map[int]string{0: "off", 1: "on"}[replicas], ns, kind, name)
			snap.refresh() // the next poll already shows the change
			w.WriteHeader(http.StatusNoContent)
		}
	})
	// Only while the cluster is offline: the button is on the offline page, and a power-on
	// sent to a running machine could switch it off, depending on what the action does.
	mux.HandleFunc("POST /power-on", func(w http.ResponseWriter, r *http.Request) {
		var v View
		switch {
		case power == nil:
			http.Error(w, "no power-on action configured", http.StatusNotFound)
		case json.Unmarshal(snap.get(), &v) != nil || v.Online:
			http.Error(w, "the cluster is online", http.StatusConflict)
		default:
			if err := power.run(); err != nil {
				log.Print(err)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			log.Print("power-on sent")
			snap.refresh()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok " + version + "\n"))
	})
	return mux
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" { // for container HEALTHCHECK, no curl in the image
		_, port, _ := net.SplitHostPort(env("PODSWITCH_LISTEN", ":8080"))
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	cfg := loadConfig()
	k, err := newKube(cfg.api, cfg.token, cfg.ca)
	if err != nil {
		log.Fatal(err)
	}
	power, err := loadPowerOn()
	if err != nil {
		log.Fatal(err)
	}
	snap := newSnapshot(func() View { return withPower(k.view(cfg), power) }, 500*time.Millisecond, 15*time.Second)
	go snap.loop()

	log.Printf("podswitch %s listening on %s, cluster %s, power-on %v", version, cfg.listen, cfg.api, power != nil)
	log.Fatal(http.ListenAndServe(cfg.listen, routes(cfg, snap, k.scale, power)))
}
