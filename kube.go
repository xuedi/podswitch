package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// kube is a minimal client for the Kubernetes API: one server, one bearer token, JSON only.
type kube struct {
	api, token string
	http       *http.Client
}

func newKube(api, tokenFile, caFile string) (*kube, error) {
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("CA: no PEM certificate in %s", caFile)
	}
	return &kube{
		api:   strings.TrimRight(api, "/"),
		token: strings.TrimSpace(string(token)),
		http: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}, nil
}

func (k *kube) do(method, path, contentType string, body []byte, out any) error {
	req, err := http.NewRequest(method, k.api+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var st struct{ Message string }
		json.NewDecoder(resp.Body).Decode(&st)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, st.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (k *kube) get(path string, out any) error { return k.do("GET", path, "", nil, out) }

// scale sets the replica count through the scale subresource, the only write podswitch makes.
func (k *kube) scale(ns, kind, name string, replicas int) error {
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	return k.do("PATCH", fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s/scale", ns, kind, name),
		"application/merge-patch+json", body, nil)
}

// --- the bits of the Kubernetes objects podswitch reads ---

type meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

type workloadList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Replicas *int32 `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int32 `json:"readyReplicas"`
		} `json:"status"`
	} `json:"items"`
}

type podList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
	} `json:"items"`
}

type nodeList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Capacity   map[string]string `json:"capacity"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			NodeInfo struct {
				KubeletVersion string `json:"kubeletVersion"`
				OSImage        string `json:"osImage"`
				KernelVersion  string `json:"kernelVersion"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

type usage struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type nodeMetrics struct {
	Items []struct {
		Usage usage `json:"usage"`
	} `json:"items"`
}

type podMetrics struct {
	Items []struct {
		Metadata   meta `json:"metadata"`
		Containers []struct {
			Usage usage `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// --- what the page shows ---

// Cluster is the sum over all nodes; OS and Kernel are those of the first node.
type Cluster struct {
	Nodes, ReadyNodes      int
	Kubernetes, OS, Kernel string
	CPUMilli, CPUCapMilli  int64
	Mem, MemCap            int64
}

type App struct {
	Namespace, Kind, Name, Title, Address, State string
	Replicas                                     int32
	CPUMilli, Mem                                int64
}

type View struct {
	Name    string
	Version string
	Online  bool
	Error   string
	Cluster Cluster
	Apps    []App
	Time    string
}

// view reads the cluster once. Any failure to list nodes counts as "offline"; later failures
// are reported in Error with whatever was read so far.
func (k *kube) view(cfg config) View {
	v := View{Name: cfg.name, Version: version, Time: time.Now().Format("15:04:05")}
	var nodes nodeList
	if err := k.get("/api/v1/nodes", &nodes); err != nil {
		log.Printf("offline: %v", err)
		return v
	}
	v.Online = true
	for i, n := range nodes.Items {
		c := &v.Cluster
		c.Nodes++
		for _, cond := range n.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				c.ReadyNodes++
			}
		}
		c.CPUCapMilli += cpuMilli(n.Status.Capacity["cpu"])
		c.MemCap += bytesOf(n.Status.Capacity["memory"])
		if i == 0 {
			c.Kubernetes, c.OS, c.Kernel = n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage, n.Status.NodeInfo.KernelVersion
			if v.Name == "" {
				v.Name = n.Metadata.Name
			}
		}
	}
	var nm nodeMetrics
	if err := k.get("/apis/metrics.k8s.io/v1beta1/nodes", &nm); err == nil {
		for _, n := range nm.Items {
			v.Cluster.CPUMilli += cpuMilli(n.Usage.CPU)
			v.Cluster.Mem += bytesOf(n.Usage.Memory)
		}
	}

	var pods podList
	if err := k.get("/api/v1/pods", &pods); err != nil {
		v.Error = err.Error()
		return v
	}
	var pm podMetrics
	k.get("/apis/metrics.k8s.io/v1beta1/pods", &pm) // optional: missing right after a start
	type use struct{ cpu, mem int64 }
	podUse := map[string]use{}
	for _, p := range pm.Items {
		var u use
		for _, c := range p.Containers {
			u.cpu += cpuMilli(c.Usage.CPU)
			u.mem += bytesOf(c.Usage.Memory)
		}
		podUse[p.Metadata.Namespace+"/"+p.Metadata.Name] = u
	}

	for _, kind := range []string{"statefulsets", "deployments"} {
		var wl workloadList
		if err := k.get("/apis/apps/v1/"+kind, &wl); err != nil {
			v.Error = err.Error()
			continue
		}
		for _, w := range wl.Items {
			ann := w.Metadata.Annotations
			if cfg.hidden[w.Metadata.Namespace] || ann[annotationHide] == "true" {
				continue
			}
			a := App{Namespace: w.Metadata.Namespace, Kind: kind, Name: w.Metadata.Name, Title: ann[annotationTitle]}
			if a.Title == "" {
				a.Title = a.Name
			}
			if port := ann[annotationPort]; port != "" {
				a.Address = cfg.host + ":" + port
			}
			if w.Spec.Replicas != nil {
				a.Replicas = *w.Spec.Replicas
			}
			present := 0
			for _, p := range pods.Items {
				if p.Metadata.Namespace != a.Namespace || !matches(w.Spec.Selector.MatchLabels, p.Metadata.Labels) {
					continue
				}
				present++
				u := podUse[p.Metadata.Namespace+"/"+p.Metadata.Name]
				a.CPUMilli += u.cpu
				a.Mem += u.mem
			}
			a.State = state(a.Replicas, w.Status.ReadyReplicas, present)
			v.Apps = append(v.Apps, a)
		}
	}
	sort.Slice(v.Apps, func(i, j int) bool {
		if v.Apps[i].Namespace != v.Apps[j].Namespace {
			return v.Apps[i].Namespace < v.Apps[j].Namespace
		}
		return v.Apps[i].Title < v.Apps[j].Title
	})
	return v
}

func state(want, ready int32, pods int) string {
	switch {
	case want == 0 && pods == 0:
		return "stopped"
	case want == 0:
		return "stopping"
	case ready >= want:
		return "running"
	default:
		return "starting"
	}
}

// matches reports whether labels satisfy a matchLabels selector. An empty selector matches
// nothing here (it would select every pod of the namespace).
func matches(sel, labels map[string]string) bool {
	if len(sel) == 0 {
		return false
	}
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}
