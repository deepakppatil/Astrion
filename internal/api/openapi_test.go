package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/deepakppatil/astrion/internal/agent"
	"github.com/deepakppatil/astrion/internal/authz"
	"github.com/deepakppatil/astrion/internal/domain"
	"github.com/deepakppatil/astrion/internal/execapi"
	"github.com/deepakppatil/astrion/internal/registry"
	"github.com/deepakppatil/astrion/internal/store"
)

// These tests keep api/openapi.yaml and api/execution-api.yaml honest: every
// route the server registers must be documented (and vice versa), documented
// object schemas must match the JSON field names of the Go types the
// handlers actually serialize, and enums must match the domain constants.

type spec struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T, name string) *spec {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", name))
	if err != nil {
		t.Fatal(err)
	}
	var s spec
	if err := yaml.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return &s
}

var httpMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}

// normalize replaces path parameter names so /jobs/{id} matches /jobs/{jobId}.
var paramRE = regexp.MustCompile(`\{[^}]+\}`)

func normalize(method, path string) string {
	return strings.ToUpper(method) + " " + paramRE.ReplaceAllString(path, "{}")
}

func TestOpenAPIRoutesMatchServer(t *testing.T) {
	h := (&Server{}).Handler()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("Handler() is %T, not a chi router", h)
	}
	server := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasSuffix(route, "/*") {
			return nil
		}
		server[normalize(method, route)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	documented := map[string]bool{}
	for _, file := range []string{"openapi.yaml", "execution-api.yaml"} {
		s := loadSpec(t, file)
		for path, item := range s.Paths {
			isInternal := strings.HasPrefix(path, "/internal/")
			if isInternal != (file == "execution-api.yaml") {
				t.Errorf("%s documents %s, which belongs in the other spec", file, path)
			}
			for m := range item {
				if httpMethods[m] {
					documented[normalize(m, path)] = true
				}
			}
		}
	}
	for r := range server {
		if !documented[r] {
			t.Errorf("route %s is served but not documented in api/*.yaml", r)
		}
	}
	for r := range documented {
		if !server[r] {
			t.Errorf("route %s is documented but not served", r)
		}
	}
}

// jsonFields returns the JSON property names of a struct type, flattening
// embedded structs the way encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolve follows a $ref (directly or as the single entry of allOf).
func resolve(s *spec, node map[string]any) map[string]any {
	for i := 0; i < 5 && node != nil; i++ {
		if ref, ok := node["$ref"].(string); ok {
			node = s.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
			continue
		}
		if all, ok := node["allOf"].([]any); ok && len(all) == 1 {
			node, _ = all[0].(map[string]any)
			continue
		}
		break
	}
	return node
}

var opaque = map[reflect.Type]bool{
	reflect.TypeOf(time.Time{}): true,
}

// compareSchema checks that the documented properties equal the Go JSON
// fields, recursing into nested objects and arrays of objects. Schemas
// without `properties` (free-form objects) are not compared.
func compareSchema(t *testing.T, s *spec, where string, node map[string]any, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	node = resolve(s, node)
	if node == nil || opaque[typ] {
		return
	}
	switch typ.Kind() {
	case reflect.Slice:
		if items, ok := node["items"].(map[string]any); ok && typ.Elem().Kind() != reflect.Uint8 {
			compareSchema(t, s, where+"[]", items, typ.Elem())
		}
		return
	case reflect.Struct:
	default:
		return
	}
	props, ok := node["properties"].(map[string]any)
	if !ok {
		return
	}
	fields := jsonFields(typ)
	if a, b := keys(props), keys(fields); !reflect.DeepEqual(a, b) {
		t.Errorf("%s: schema properties differ from Go type %s\n  spec: %v\n  go:   %v", where, typ, a, b)
		return
	}
	for name, ft := range fields {
		child, _ := props[name].(map[string]any)
		compareSchema(t, s, where+"."+name, child, ft)
	}
}

func TestOpenAPISchemasMatchGoTypes(t *testing.T) {
	pub := loadSpec(t, "openapi.yaml")
	public := map[string]any{
		"Job":                        domain.Job{},
		"JobRepository":              domain.JobRepository{},
		"Workspace":                  domain.Workspace{},
		"WorkspaceRequest":           workspaceRequest{},
		"WorkspacePatch":             workspaceRequest{},
		"Repository":                 domain.Repository{},
		"RepositoryRequest":          repositoryRequest{},
		"FrameworkProfile":           domain.FrameworkProfile{},
		"Command":                    registry.Command{},
		"InputField":                 registry.InputField{},
		"AgentCapabilities":          agent.Capabilities{},
		"Interaction":                interactionView{},
		"InteractionOption":          domain.InteractionOption{},
		"InteractionResponse":        domain.InteractionResponse{},
		"InteractionResponseRequest": responseRequest{},
		"SubmitJobRequest":           submitJobRequest{},
		"OperatorActionRequest":      operatorRequest{},
		"Execution":                  domain.ExecutionEnvironment{},
		"Artifact":                   domain.Artifact{},
		"AuditEvent":                 domain.AuditEvent{},
		"JobEvent":                   domain.JobEvent{},
		"LogLine":                    logLineView{},
		"Dashboard":                  store.DashboardStats{},
	}
	for name, v := range public {
		node, ok := pub.Components.Schemas[name]
		if !ok {
			t.Errorf("openapi.yaml: schema %s missing", name)
			continue
		}
		compareSchema(t, pub, "openapi.yaml#"+name, node, reflect.TypeOf(v))
	}
	// AgentProfile is served as the domain type plus adapter capabilities.
	type agentView struct {
		domain.AgentProfile
		Capabilities agent.Capabilities `json:"capabilities"`
	}
	compareSchema(t, pub, "openapi.yaml#AgentProfile", pub.Components.Schemas["AgentProfile"], reflect.TypeOf(agentView{}))
	me := pub.Components.Schemas["Me"]["properties"].(map[string]any)["user"].(map[string]any)
	compareSchema(t, pub, "openapi.yaml#Me.user", me, reflect.TypeOf(authz.Principal{}))

	ex := loadSpec(t, "execution-api.yaml")
	internal := map[string]any{
		"Spec":               execapi.Spec{},
		"RepoSpec":           execapi.RepoSpec{},
		"ResumeRequest":      agent.ResumeRequest{},
		"StatusReport":       execapi.StatusReport{},
		"LogBatch":           execapi.LogBatch{},
		"RepositoryReport":   execapi.RepositoryReport{},
		"InteractionRequest": agent.InteractionRequest{},
		"InteractionCreated": execapi.InteractionCreated{},
		"InteractionState":   execapi.InteractionState{},
		"ArtifactCreated":    execapi.ArtifactCreated{},
		"Completion":         execapi.Completion{},
	}
	for name, v := range internal {
		node, ok := ex.Components.Schemas[name]
		if !ok {
			t.Errorf("execution-api.yaml: schema %s missing", name)
			continue
		}
		compareSchema(t, ex, "execution-api.yaml#"+name, node, reflect.TypeOf(v))
	}
}

// domainConstants parses internal/domain and groups typed string constants
// by their type name, e.g. "Permission" -> ["workspace:read", ...].
func domainConstants(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	dir := filepath.Join("..", "domain")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						// Empty values mean "none" (e.g. no failure) and are
						// represented by omitting the field.
						if s, _ := strconv.Unquote(lit.Value); s != "" {
							out[id.Name] = append(out[id.Name], s)
						}
					}
				}
			}
		}
	}
	return out
}

func TestOpenAPIEnumsMatchDomain(t *testing.T) {
	consts := domainConstants(t)
	pub := loadSpec(t, "openapi.yaml")
	for _, name := range []string{"Phase", "JobState", "FailureClass", "Role", "Permission", "DataClassification",
		"InteractionMode", "InteractionKind", "InteractionStatus", "Decision", "ExecutionStatus"} {
		want := append([]string(nil), consts[name]...)
		if len(want) == 0 {
			t.Errorf("no domain constants of type %s", name)
			continue
		}
		var got []string
		for _, v := range pub.Components.Schemas[name]["enum"].([]any) {
			got = append(got, v.(string))
		}
		sort.Strings(want)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("openapi.yaml enum %s differs from domain\n  spec:   %v\n  domain: %v", name, got, want)
		}
	}
}
