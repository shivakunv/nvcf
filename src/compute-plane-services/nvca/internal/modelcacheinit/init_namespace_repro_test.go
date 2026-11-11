package modelcacheinit

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// nvbugs#7055457: on NVCA 3.7.x staging clusters the model cache controller
// loops on `namespaces "nvca-modelcache-init" not found`, so every helm task
// or function carrying a model or resource stays QUEUED / DEPLOYING. The
// controller process cannot be run here, so this test drives the nearest
// thing this repository controls: the namespaces the nvca component's own
// rendered manifests create, against a fake Kubernetes API server.

const (
	initNamespace  = "nvca-modelcache-init"
	initLeaseName  = "nvca-modelcache-init"
	storageRequest = "nvmesh-model-cache"
)

// apiServer stands in for the Kubernetes API server: it knows only the
// namespaces the nvca component renders, and answers a lease create in any
// other namespace with the NotFound Status the clusters returned.
type apiServer struct {
	mu         sync.Mutex
	namespaces map[string]bool
	leases     map[string]string
}

func newAPIServer(namespaces map[string]bool) *apiServer {
	return &apiServer{namespaces: namespaces, leases: map[string]string{}}
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/apis/coordination.k8s.io/v1/namespaces/"
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "leases" {
		http.NotFound(w, r)
		return
	}
	ns := parts[0]
	var body struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			HolderIdentity string `json:"holderIdentity"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.namespaces[ns] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind":    "Status",
			"status":  "Failure",
			"reason":  "NotFound",
			"code":    404,
			"message": fmt.Sprintf("namespaces %q not found", ns),
		})
		return
	}
	a.leases[ns+"/"+body.Metadata.Name] = body.Spec.HolderIdentity
	w.WriteHeader(http.StatusCreated)
}

func (a *apiServer) holder(ns, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.leases[ns+"/"+name]
}

// createLease performs the model cache controller's lease create.
func createLease(base, ns, name, holder string) error {
	payload := fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q},"spec":{"holderIdentity":%q}}`, name, ns, holder)
	url := base + "/apis/coordination.k8s.io/v1/namespaces/" + ns + "/leases"
	resp, err := http.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	var status struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&status)
	if status.Message == "" {
		return fmt.Errorf("lease create failed with status %d", resp.StatusCode)
	}
	return fmt.Errorf("%s", status.Message)
}

func nvcaRoot(t *testing.T) string {
	t.Helper()
	if ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); ws != "" {
		return filepath.Join(ws, "src", "compute-plane-services", "nvca")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := wd; dir != string(filepath.Separator) && dir != "."; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "nvca" {
			return dir
		}
	}
	t.Fatalf("could not locate the nvca component root from %s", wd)
	return ""
}

// renderedManifests returns the Kubernetes objects the nvca component ships:
// every chart under the component rendered with helm, plus the plain
// manifests it carries.
func renderedManifests(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	var charts []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "Chart.yaml" {
			charts = append(charts, filepath.Dir(p))
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		docs = append(docs, strings.Split(string(b), "\n---")...)
		return nil
	})
	if err != nil {
		t.Fatalf("reading nvca deployment artifacts under %s: %v", root, err)
	}
	for _, chart := range charts {
		out, rerr := exec.Command("helm", "template", "nvca", chart, "--namespace", "nvca-system").CombinedOutput()
		if rerr != nil {
			t.Logf("helm template %s: %v (using the chart's raw templates instead)", chart, rerr)
			continue
		}
		docs = append(docs, strings.Split(string(out), "\n---")...)
	}
	t.Logf("nvca ships %d chart(s) and %d manifest document(s)", len(charts), len(docs))
	return docs
}

// declaredNamespaces is the namespace set those objects create on a cluster.
func declaredNamespaces(docs []string) map[string]bool {
	out := map[string]bool{}
	for _, doc := range docs {
		if !strings.Contains(doc, "kind: Namespace") {
			continue
		}
		for _, line := range strings.Split(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "name:") {
				continue
			}
			name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "name:")), "\"'")
			if name != "" {
				out[name] = true
			}
		}
	}
	return out
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestModelCacheInitLeaseAgainstRenderedManifests(t *testing.T) {
	root := nvcaRoot(t)
	api := newAPIServer(declaredNamespaces(renderedManifests(t, root)))
	srv := httptest.NewServer(api)
	defer srv.Close()

	if err := createLease(srv.URL, initNamespace, initLeaseName, storageRequest); err != nil {
		t.Fatalf("model cache controller cannot create its lease for StorageRequest %s: %v; "+
			"the nvca component renders namespaces %v, none of them %q, so the reconcile "+
			"loop never progresses and helm tasks/functions carrying a model or resource "+
			"stay QUEUED/DEPLOYING (nvbugs#7055457)",
			storageRequest, err, sortedNames(api.namespaces), initNamespace)
	}
	if got := api.holder(initNamespace, initLeaseName); got != storageRequest {
		t.Fatalf("lease %s/%s held by %q, want %q", initNamespace, initLeaseName, got, storageRequest)
	}
}
