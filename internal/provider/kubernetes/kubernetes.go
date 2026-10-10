// Package kubernetes implements the execution provider on any conformant
// Kubernetes API: Rancher-managed clusters (directly or through Rancher's
// cluster proxy endpoint) and Amazon EKS. It uses only stable core and
// batch/v1 APIs over plain HTTPS so that it carries no distribution-specific
// assumptions and no heavyweight client dependencies.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/provider"
)

// Config for one execution pool.
type Config struct {
	Name      string `json:"name"`
	Server    string `json:"server"` // e.g. https://rancher.example.com/k8s/clusters/c-m-xxxx or EKS endpoint
	Namespace string `json:"namespace"`
	// TokenFile is re-read on every request so rotated credentials (projected
	// service-account tokens, EKS token refreshers) are picked up.
	TokenFile          string            `json:"tokenFile"`
	Token              string            `json:"-"`
	CAFile             string            `json:"caFile"`
	InsecureSkipVerify bool              `json:"insecureSkipVerify"` // tests only; refused in production config
	ServiceAccount     string            `json:"serviceAccount"`     // dedicated SA with no RBAC bindings
	RuntimeClassName   string            `json:"runtimeClassName"`   // e.g. gvisor/kata for high-risk workloads
	PriorityClassName  string            `json:"priorityClassName"`
	NodeSelector       map[string]string `json:"nodeSelector"`
	Tolerations        []map[string]any  `json:"tolerations"`
	ImagePullSecrets   []string          `json:"imagePullSecrets"`
	WorkspaceSizeLimit string            `json:"workspaceSizeLimit"`
	// TTLAfterFinished is a backstop only; Astrion deletes Jobs explicitly (LIFE-005).
	TTLAfterFinished time.Duration `json:"ttlAfterFinished"`
	RunAsUser        int64         `json:"runAsUser"`
}

// InClusterConfig builds a config from the pod's service account.
func InClusterConfig(name, namespace string) Config {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	return Config{
		Name:      name,
		Server:    "https://" + host + ":" + port,
		Namespace: namespace,
		TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		CAFile:    "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
	}
}

type Provider struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Provider, error) {
	if cfg.Server == "" || cfg.Namespace == "" {
		return nil, errors.New("kubernetes provider: server and namespace are required")
	}
	if cfg.WorkspaceSizeLimit == "" {
		cfg.WorkspaceSizeLimit = "10Gi"
	}
	if cfg.TTLAfterFinished == 0 {
		cfg.TTLAfterFinished = time.Hour
	}
	if cfg.RunAsUser == 0 {
		cfg.RunAsUser = 10001
	}
	if cfg.ServiceAccount == "" {
		cfg.ServiceAccount = "astrion-execution"
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // explicit opt-in for tests
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		tlsCfg.RootCAs = pool
	}
	return &Provider{cfg: cfg, client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
	}}, nil
}

func (p *Provider) Name() string { return p.cfg.Name }

var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// ResourceName derives a DNS-1123 compliant, deterministic name from the execution ID.
func ResourceName(executionID string) string {
	n := nameRe.ReplaceAllString(strings.ToLower(strings.ReplaceAll(executionID, "_", "-")), "-")
	n = "astrion-" + strings.Trim(n, "-")
	if len(n) > 63 {
		n = n[:63]
	}
	return strings.TrimRight(n, "-")
}

func (p *Provider) Validate(ctx context.Context) error {
	var out map[string]any
	if err := p.do(ctx, http.MethodGet, p.jobsPath("")+"?limit=1", nil, &out, ""); err != nil {
		return fmt.Errorf("kubernetes pool %s: cannot list jobs in %s: %w", p.cfg.Name, p.cfg.Namespace, err)
	}
	return nil
}

// CheckCapacity consults namespace ResourceQuotas for a Job count limit.
func (p *Provider) CheckCapacity(ctx context.Context, _ provider.Spec) (provider.Capacity, error) {
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Status   struct {
				Hard map[string]string `json:"hard"`
				Used map[string]string `json:"used"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, "/api/v1/namespaces/"+p.cfg.Namespace+"/resourcequotas", nil, &list, ""); err != nil {
		return provider.Capacity{}, err
	}
	for _, q := range list.Items {
		for _, key := range []string{"count/jobs.batch", "pods", "count/pods"} {
			hard, ok1 := q.Status.Hard[key]
			used, ok2 := q.Status.Used[key]
			if !ok1 || !ok2 {
				continue
			}
			h, err1 := strconv.Atoi(hard)
			u, err2 := strconv.Atoi(used)
			if err1 == nil && err2 == nil && u >= h {
				return provider.Capacity{Available: false, Reason: fmt.Sprintf("quota %s: %s %d/%d", q.Metadata.Name, key, u, h)}, nil
			}
		}
	}
	return provider.Capacity{Available: true}, nil
}

func (p *Provider) jobsPath(name string) string {
	path := "/apis/batch/v1/namespaces/" + p.cfg.Namespace + "/jobs"
	if name != "" {
		path += "/" + name
	}
	return path
}

func (p *Provider) secretsPath(name string) string {
	path := "/api/v1/namespaces/" + p.cfg.Namespace + "/secrets"
	if name != "" {
		path += "/" + name
	}
	return path
}

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty"`
	OwnerReferences   []map[string]any  `json:"ownerReferences,omitempty"`
}

type jobObject struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMeta     `json:"metadata"`
	Spec       map[string]any `json:"spec"`
	Status     jobStatus      `json:"status"`
}

type jobStatus struct {
	Active     int  `json:"active"`
	Ready      *int `json:"ready"`
	Succeeded  int  `json:"succeeded"`
	Failed     int  `json:"failed"`
	Conditions []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

const secretsMountPath = "/var/run/astrion/secrets"

// BuildJob renders the hardened Job manifest. Exported for tests and for
// policy review (docs/security/threat-model.md).
func (p *Provider) BuildJob(spec provider.Spec) jobObject {
	name := ResourceName(spec.Owner.ExecutionID)
	labels := spec.Owner.Labels()
	podLabels := map[string]string{}
	for k, v := range labels {
		podLabels[k] = v
	}
	if spec.NetworkProfile != "" {
		podLabels["astrion.io/network-profile"] = spec.NetworkProfile
	}
	env := []map[string]any{{"name": "ASTRION_SECRETS_DIR", "value": secretsMountPath}}
	for _, k := range sortedKeys(spec.Env) {
		env = append(env, map[string]any{"name": k, "value": spec.Env[k]})
	}
	res := map[string]any{"requests": map[string]string{}, "limits": map[string]string{}}
	setQ := func(kind, key, v string) {
		if v != "" {
			res[kind].(map[string]string)[key] = v
		}
	}
	setQ("requests", "cpu", spec.Resources.CPURequest)
	setQ("requests", "memory", spec.Resources.MemoryRequest)
	setQ("limits", "cpu", spec.Resources.CPULimit)
	setQ("limits", "memory", spec.Resources.MemoryLimit)
	setQ("requests", "ephemeral-storage", spec.Resources.EphemeralStorage)
	setQ("limits", "ephemeral-storage", spec.Resources.EphemeralStorage)

	podSpec := map[string]any{
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"serviceAccountName":           p.cfg.ServiceAccount,
		"enableServiceLinks":           false,
		"hostNetwork":                  false,
		"hostPID":                      false,
		"hostIPC":                      false,
		"securityContext": map[string]any{
			"runAsNonRoot":   true,
			"runAsUser":      p.cfg.RunAsUser,
			"runAsGroup":     p.cfg.RunAsUser,
			"fsGroup":        p.cfg.RunAsUser,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
		"containers": []map[string]any{{
			"name":            "agent",
			"image":           spec.Image,
			"imagePullPolicy": "IfNotPresent",
			"command":         spec.Command,
			"env":             env,
			"resources":       res,
			"workingDir":      "/workspace",
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"privileged":               false,
				"readOnlyRootFilesystem":   true,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
			},
			"volumeMounts": []map[string]any{
				{"name": "workspace", "mountPath": "/workspace"},
				{"name": "tmp", "mountPath": "/tmp"},
				{"name": "home", "mountPath": "/home/agent"},
				{"name": "secrets", "mountPath": secretsMountPath, "readOnly": true},
			},
		}},
		"volumes": []map[string]any{
			{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": p.cfg.WorkspaceSizeLimit}},
			{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
			{"name": "home", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
			{"name": "secrets", "secret": map[string]any{"secretName": name, "defaultMode": 0o440}},
		},
	}
	if p.cfg.RuntimeClassName != "" {
		podSpec["runtimeClassName"] = p.cfg.RuntimeClassName
	}
	if p.cfg.PriorityClassName != "" {
		podSpec["priorityClassName"] = p.cfg.PriorityClassName
	}
	if len(p.cfg.NodeSelector) > 0 {
		podSpec["nodeSelector"] = p.cfg.NodeSelector
	}
	if len(p.cfg.Tolerations) > 0 {
		podSpec["tolerations"] = p.cfg.Tolerations
	}
	if len(p.cfg.ImagePullSecrets) > 0 {
		var ips []map[string]string
		for _, s := range p.cfg.ImagePullSecrets {
			ips = append(ips, map[string]string{"name": s})
		}
		podSpec["imagePullSecrets"] = ips
	}
	deadline := int64(spec.ActiveDeadline / time.Second)
	if deadline <= 0 {
		deadline = 3600
	}
	return jobObject{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: objectMeta{
			Name: name, Namespace: p.cfg.Namespace, Labels: labels, Annotations: spec.Owner.Annotations(),
		},
		Spec: map[string]any{
			// Suspended until the per-execution Secret exists (created with an
			// ownerReference to this Job so Kubernetes GC also removes it).
			"suspend":                 true,
			"backoffLimit":            0, // retries are a workflow decision, never implicit
			"activeDeadlineSeconds":   deadline,
			"ttlSecondsAfterFinished": int64(p.cfg.TTLAfterFinished / time.Second),
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec":     podSpec,
			},
		},
	}
}

// Allocate is idempotent by execution ID.
func (p *Provider) Allocate(ctx context.Context, spec provider.Spec) (provider.Ref, error) {
	if spec.Image == "" {
		return provider.Ref{}, errors.New("image is required")
	}
	name := ResourceName(spec.Owner.ExecutionID)
	ref := provider.Ref{Provider: p.cfg.Name, ID: p.cfg.Namespace + "/" + name}

	var job jobObject
	err := p.do(ctx, http.MethodGet, p.jobsPath(name), nil, &job, "")
	switch {
	case isNotFound(err):
		desired := p.BuildJob(spec)
		if err := p.do(ctx, http.MethodPost, p.jobsPath(""), desired, &job, ""); err != nil && !isConflict(err) {
			return provider.Ref{}, fmt.Errorf("create job: %w", err)
		}
		if job.Metadata.UID == "" {
			if err := p.do(ctx, http.MethodGet, p.jobsPath(name), nil, &job, ""); err != nil {
				return provider.Ref{}, err
			}
		}
	case err != nil:
		return provider.Ref{}, err
	default:
		if job.Metadata.Labels[provider.LabelTenant] != spec.Owner.TenantID {
			return provider.Ref{}, fmt.Errorf("job %s exists with different ownership", name)
		}
	}

	secretData := map[string]string{}
	for k, v := range spec.Secrets {
		secretData[k] = encodeB64(v)
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": objectMeta{
			Name: name, Namespace: p.cfg.Namespace, Labels: spec.Owner.Labels(),
			OwnerReferences: []map[string]any{{
				"apiVersion": "batch/v1", "kind": "Job", "name": name, "uid": job.Metadata.UID,
				"blockOwnerDeletion": true,
			}},
		},
		"data": secretData,
	}
	if err := p.do(ctx, http.MethodPost, p.secretsPath(""), secret, nil, ""); err != nil && !isConflict(err) {
		return provider.Ref{}, fmt.Errorf("create secret: %w", err)
	}
	patch := map[string]any{"spec": map[string]any{"suspend": false}}
	if err := p.do(ctx, http.MethodPatch, p.jobsPath(name), patch, nil, "application/merge-patch+json"); err != nil {
		return provider.Ref{}, fmt.Errorf("unsuspend job: %w", err)
	}
	return ref, nil
}

func (p *Provider) splitRef(ref provider.Ref) (string, error) {
	ns, name, ok := strings.Cut(ref.ID, "/")
	if !ok || ns != p.cfg.Namespace || name == "" {
		return "", fmt.Errorf("ref %q does not belong to pool %s", ref.ID, p.cfg.Name)
	}
	return name, nil
}

func (p *Provider) Status(ctx context.Context, ref provider.Ref) (provider.Status, error) {
	name, err := p.splitRef(ref)
	if err != nil {
		return provider.Status{}, err
	}
	var job jobObject
	if err := p.do(ctx, http.MethodGet, p.jobsPath(name), nil, &job, ""); err != nil {
		if isNotFound(err) {
			return provider.Status{Ref: ref, Phase: provider.PhaseNotFound}, nil
		}
		return provider.Status{}, err
	}
	return toStatus(p.cfg.Name, p.cfg.Namespace, job), nil
}

func toStatus(pool, ns string, job jobObject) provider.Status {
	st := provider.Status{
		Ref:   provider.Ref{Provider: pool, ID: ns + "/" + job.Metadata.Name},
		Phase: provider.PhasePending,
		Owner: ownerFromMeta(job.Metadata),
	}
	if job.Metadata.CreationTimestamp != nil {
		st.Created = *job.Metadata.CreationTimestamp
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			st.Phase = provider.PhaseSucceeded
			return st
		case "Failed":
			st.Phase = provider.PhaseFailed
			st.Message = strings.TrimSpace(c.Reason + ": " + c.Message)
			return st
		}
	}
	if job.Status.Active > 0 && (job.Status.Ready == nil || *job.Status.Ready > 0) {
		st.Phase = provider.PhaseRunning
	}
	return st
}

func ownerFromMeta(m objectMeta) provider.Ownership {
	o := provider.Ownership{
		TenantID:    m.Labels[provider.LabelTenant],
		JobID:       m.Labels[provider.LabelJob],
		ExecutionID: m.Labels[provider.LabelExecution],
		WorkflowID:  m.Annotations[provider.AnnotWorkflow],
	}
	o.Attempt, _ = strconv.Atoi(m.Labels[provider.LabelAttempt])
	o.ExpiresAt, _ = time.Parse(time.RFC3339, m.Annotations[provider.AnnotExpiresAt])
	return o
}

// Terminate deletes the Job (and its pods) and the Secret. Idempotent.
func (p *Provider) Terminate(ctx context.Context, ref provider.Ref) error {
	name, err := p.splitRef(ref)
	if err != nil {
		return err
	}
	body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background", "gracePeriodSeconds": 10}
	if err := p.do(ctx, http.MethodDelete, p.jobsPath(name), body, nil, ""); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete job: %w", err)
	}
	// Secret is garbage-collected via ownerReference, but delete explicitly so
	// credentials never outlive the job even if GC is delayed (DEPLOY-008).
	if err := p.do(ctx, http.MethodDelete, p.secretsPath(name), nil, nil, ""); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}

func (p *Provider) ListOwned(ctx context.Context, tenantID string) ([]provider.Status, error) {
	sel := provider.LabelManagedBy + "=" + provider.ManagedByValue + "," + provider.LabelComponent + "=" + provider.ComponentValue
	if tenantID != "" {
		sel += "," + provider.LabelTenant + "=" + tenantID
	}
	var list struct {
		Items []jobObject `json:"items"`
	}
	if err := p.do(ctx, http.MethodGet, p.jobsPath("")+"?labelSelector="+url.QueryEscape(sel), nil, &list, ""); err != nil {
		return nil, err
	}
	out := make([]provider.Status, 0, len(list.Items))
	for _, j := range list.Items {
		out = append(out, toStatus(p.cfg.Name, p.cfg.Namespace, j))
	}
	return out, nil
}

type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("kubernetes API %d: %s", e.Code, e.Message) }

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == http.StatusNotFound
}

func isConflict(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == http.StatusConflict
}

func (p *Provider) token() (string, error) {
	if p.cfg.TokenFile != "" {
		b, err := os.ReadFile(p.cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return p.cfg.Token, nil
}

func (p *Provider) do(ctx context.Context, method, path string, body, out any, contentType string) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.cfg.Server, "/")+path, rdr)
	if err != nil {
		return err
	}
	tok, err := p.token()
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = http.StatusText(resp.StatusCode)
		}
		return &apiError{Code: resp.StatusCode, Message: st.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Ensure interface compliance.
var _ provider.Provider = (*Provider)(nil)

// Errors surfaced to callers should not leak tokens; apiError never includes headers.
var _ = domain.ErrNotFound
