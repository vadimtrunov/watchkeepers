// Package deploy contains contract tests pinning the shape of the
// Phase 1 compose stack (M10.3) and its observability provisioning.
//
// The tests parse the source-of-truth YAML / JSON files at repo root +
// deploy/observability/ and assert structural invariants that a future
// change MUST preserve:
//
//   - Every service named in the M10.3 roadmap entry is present in
//     docker-compose.yml. Adding a service is fine; renaming or
//     dropping one is a contract break.
//   - The Keep service waits for both Postgres health AND the
//     completion of the migrate sidecar before booting. Dropping
//     either gate is a regression that would surface as random
//     5xx during the first request after `compose up`.
//   - Prometheus scrapes the Keep `/metrics` endpoint on port 8080.
//   - Every Prometheus metric family pinned by the wkmetrics package
//     ([wkmetrics.expectedFamilies] mirrored here verbatim) appears
//     in at least one PromQL expression in the starter Grafana
//     dashboard. A renamed or removed metric without a corresponding
//     dashboard update would silently blank the panel.
//
// Tests live in the same module as the Keep service so the contract
// pins move with the codebase (a future rename of a wkmetrics family
// MUST update this list in the same PR; the test will fail loudly
// otherwise — the M10.1 lesson on contract-pin-now applies).
package deploy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot returns the absolute path to the watchkeepers repo root
// regardless of where `go test` is invoked from. Tests in this package
// read source-of-truth files at the root + deploy/, so anchoring on
// the test file's own location avoids the brittle relative-path trap.
// Iter-1 #15: the `WK_REPO_ROOT` env override lets a sandboxed test
// runner (bazel-style, `go test -C`, in-memory FS) point at the
// canonical repo root without depending on `runtime.Caller`'s file
// path layout.
func repoRoot(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("WK_REPO_ROOT"); env != "" {
		return env
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// .../core/internal/deploy/compose_test.go -> .../
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// composeFile is the minimal subset of docker-compose.yml the tests
// care about. yaml.v3 silently ignores extra fields so the schema
// stays additive-safe.
type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
	Networks map[string]any            `yaml:"networks"`
	Secrets  map[string]any            `yaml:"secrets"`
}

type composeService struct {
	Image       string            `yaml:"image"`
	Build       any               `yaml:"build"`
	DependsOn   map[string]any    `yaml:"depends_on"`
	Ports       []string          `yaml:"ports"`
	Profiles    []string          `yaml:"profiles"`
	Environment map[string]string `yaml:"environment"`
	Secrets     []string          `yaml:"secrets"`
	Healthcheck map[string]any    `yaml:"healthcheck"`
}

func loadCompose(t *testing.T) composeFile {
	t.Helper()
	path := filepath.Join(repoRoot(t), "docker-compose.yml")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got composeFile
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("yaml unmarshal %s: %v", path, err)
	}
	return got
}

// TestComposeServicesPresent pins the M10.3 service set. The list
// matches the docs/ROADMAP-phase1.md §M10.3 entry verbatim plus the
// observability + migrate sidecar services the same milestone adds.
func TestComposeServicesPresent(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	want := []string{
		// Data plane (fully wired).
		"postgres",
		"migrate",
		"keep",
		// Observability plane (fully wired).
		"prometheus",
		"grafana",
		// Agent plane (contract stubs — M10.3.b follow-up, gated by `stubs` profile).
		"core",
		"watchmaster",
		"coordinator",
		// Dev Slack socket bridge (contract stub — M10.3.c follow-up).
		"slack-bridge",
	}
	for _, name := range want {
		if _, ok := cf.Services[name]; !ok {
			t.Errorf("docker-compose.yml: service %q missing", name)
		}
	}
}

// TestIter1_AgentStubsGatedByProfile pins the iter-1 #3 fix: the four
// agent-stub services (core / watchmaster / coordinator /
// slack-bridge) are gated behind the `stubs` profile so default
// `docker compose up` does not start them. Operators probing the
// future shape run `docker compose --profile stubs up <name>`.
func TestIter1_AgentStubsGatedByProfile(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	stubs := []string{"core", "watchmaster", "coordinator", "slack-bridge"}
	for _, name := range stubs {
		svc, ok := cf.Services[name]
		if !ok {
			t.Errorf("service %q missing", name)
			continue
		}
		var found bool
		for _, p := range svc.Profiles {
			if p == "stubs" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("service %q profiles = %v, want to include %q", name, svc.Profiles, "stubs")
		}
	}
}

// TestIter1_DataPlaneServicesNotGatedByProfile asserts the inverse:
// postgres / migrate / keep / prometheus / grafana have NO profile
// constraint and therefore start under default `compose up`. The DoD
// ("docker compose up brings Phase 1 online with no manual steps
// beyond secret provisioning") fails if any of these are gated.
func TestIter1_DataPlaneServicesNotGatedByProfile(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	dataPlane := []string{"postgres", "migrate", "keep", "prometheus", "grafana"}
	for _, name := range dataPlane {
		svc, ok := cf.Services[name]
		if !ok {
			t.Errorf("service %q missing", name)
			continue
		}
		if len(svc.Profiles) != 0 {
			t.Errorf("service %q profiles = %v, want empty (data plane must boot under default up)", name, svc.Profiles)
		}
	}
}

// TestKeepDependsOnPostgresHealthyAndMigrateCompleted pins the
// dependency ordering. Both gates are required: postgres health
// alone is not enough because the schema is empty until the migrate
// sidecar has run.
func TestKeepDependsOnPostgresHealthyAndMigrateCompleted(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	keep, ok := cf.Services["keep"]
	if !ok {
		t.Fatalf("service keep missing")
	}
	pg, ok := keep.DependsOn["postgres"].(map[string]any)
	if !ok {
		t.Fatalf("keep.depends_on.postgres is not a map, got %T", keep.DependsOn["postgres"])
	}
	if got := pg["condition"]; got != "service_healthy" {
		t.Errorf("keep.depends_on.postgres.condition = %v, want service_healthy", got)
	}
	mig, ok := keep.DependsOn["migrate"].(map[string]any)
	if !ok {
		t.Fatalf("keep.depends_on.migrate is not a map, got %T", keep.DependsOn["migrate"])
	}
	if got := mig["condition"]; got != "service_completed_successfully" {
		t.Errorf("keep.depends_on.migrate.condition = %v, want service_completed_successfully", got)
	}
}

// TestMigrateDependsOnPostgresHealthy pins the migration sidecar's
// gate. A migrate run against a still-booting postgres process would
// fail with a transient connection error and crash-loop the stack.
func TestMigrateDependsOnPostgresHealthy(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	mig, ok := cf.Services["migrate"]
	if !ok {
		t.Fatalf("service migrate missing")
	}
	pg, ok := mig.DependsOn["postgres"].(map[string]any)
	if !ok {
		t.Fatalf("migrate.depends_on.postgres is not a map, got %T", mig.DependsOn["postgres"])
	}
	if got := pg["condition"]; got != "service_healthy" {
		t.Errorf("migrate.depends_on.postgres.condition = %v, want service_healthy", got)
	}
}

// TestIter1_GrafanaDependsOnPrometheusHealthy pins the iter-1 #6
// fix: grafana must wait for prometheus's healthcheck to pass, not
// just for the container to start, otherwise the datasource
// provisioning may probe a not-yet-ready Prometheus and flag it
// "not working" on first load.
func TestIter1_GrafanaDependsOnPrometheusHealthy(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	gf, ok := cf.Services["grafana"]
	if !ok {
		t.Fatalf("service grafana missing")
	}
	prom, ok := gf.DependsOn["prometheus"].(map[string]any)
	if !ok {
		t.Fatalf("grafana.depends_on.prometheus is not a map, got %T", gf.DependsOn["prometheus"])
	}
	if got := prom["condition"]; got != "service_healthy" {
		t.Errorf("grafana.depends_on.prometheus.condition = %v, want service_healthy", got)
	}

	// Prometheus must declare a healthcheck for the dependency to be
	// resolvable; verify the field is non-empty.
	pSvc, ok := cf.Services["prometheus"]
	if !ok {
		t.Fatalf("service prometheus missing")
	}
	if len(pSvc.Healthcheck) == 0 {
		t.Errorf("prometheus has no healthcheck; grafana's service_healthy gate cannot resolve")
	}
}

// TestIter1_PrometheusAndGrafanaBoundToLocalhost pins the iter-1 #5
// fix: the un-authenticated Prometheus PromQL API and Grafana UI
// must NOT be reachable from outside the local host by default.
// Operators with a deliberate reverse proxy can override per-host;
// the compose default stays safe.
func TestIter1_PrometheusAndGrafanaBoundToLocalhost(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	cases := []struct {
		service string
		prefix  string
	}{
		{"prometheus", "127.0.0.1:"},
		{"grafana", "127.0.0.1:"},
		{"keep", "127.0.0.1:"},
	}
	for _, tc := range cases {
		svc, ok := cf.Services[tc.service]
		if !ok {
			t.Errorf("service %q missing", tc.service)
			continue
		}
		for _, p := range svc.Ports {
			if !strings.HasPrefix(p, tc.prefix) {
				t.Errorf("service %q port %q must bind to %s (iter-1 #5: un-authenticated surfaces stay on loopback by default)", tc.service, p, tc.prefix)
			}
		}
	}
}

// TestIter1_PostgresPasswordViaDockerSecret pins the iter-1 #1 fix:
// the Postgres password reaches the postgres container via a docker
// `secrets:` mount (POSTGRES_PASSWORD_FILE), NOT as a plain env value
// visible to `docker inspect` or `/proc/<pid>/environ`. Same for the
// migrate sidecar.
func TestIter1_PostgresPasswordViaDockerSecret(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	if _, ok := cf.Secrets["postgres_password"]; !ok {
		t.Fatalf("compose secrets must declare postgres_password")
	}
	for _, svcName := range []string{"postgres", "migrate"} {
		svc, ok := cf.Services[svcName]
		if !ok {
			t.Errorf("service %q missing", svcName)
			continue
		}
		var hasMount bool
		for _, s := range svc.Secrets {
			if s == "postgres_password" {
				hasMount = true
				break
			}
		}
		if !hasMount {
			t.Errorf("service %q must mount the postgres_password secret", svcName)
		}
		if got, ok := svc.Environment["POSTGRES_PASSWORD"]; ok && got != "" {
			t.Errorf("service %q must NOT pass POSTGRES_PASSWORD as a plain env var (iter-1 #1), got %q", svcName, got)
		}
	}
}

// TestIter1_KeepTokenSigningKeyViaDockerSecret pins the iter-1 #2
// fix: KEEP_TOKEN_SIGNING_KEY reaches the keep container via docker
// secret mount, NOT a plain env var. The Keep config code accepts
// `KEEP_TOKEN_SIGNING_KEY_FILE` as the file-fallback variant.
func TestIter1_KeepTokenSigningKeyViaDockerSecret(t *testing.T) {
	t.Parallel()

	cf := loadCompose(t)
	if _, ok := cf.Secrets["keep_token_signing_key"]; !ok {
		t.Fatalf("compose secrets must declare keep_token_signing_key")
	}
	keep, ok := cf.Services["keep"]
	if !ok {
		t.Fatalf("service keep missing")
	}
	var hasMount bool
	for _, s := range keep.Secrets {
		if s == "keep_token_signing_key" {
			hasMount = true
			break
		}
	}
	if !hasMount {
		t.Errorf("service keep must mount the keep_token_signing_key secret")
	}
	if got, ok := keep.Environment["KEEP_TOKEN_SIGNING_KEY"]; ok && got != "" {
		t.Errorf("service keep must NOT pass KEEP_TOKEN_SIGNING_KEY as a plain env var (iter-1 #2), got %q", got)
	}
	if got := keep.Environment["KEEP_TOKEN_SIGNING_KEY_FILE"]; got == "" {
		t.Errorf("service keep must set KEEP_TOKEN_SIGNING_KEY_FILE to the mounted secret path")
	}
}

// prometheusConfig is the minimal subset of the scrape config the
// tests need. The full Prometheus schema is large; we pin only the
// fields the M10.3 contract relies on.
type prometheusConfig struct {
	Global struct {
		ScrapeInterval string `yaml:"scrape_interval"`
	} `yaml:"global"`
	ScrapeConfigs []struct {
		JobName       string `yaml:"job_name"`
		MetricsPath   string `yaml:"metrics_path"`
		StaticConfigs []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

// TestPrometheusScrapesKeepMetricsPath pins the scrape target. A
// rename of either the `keep` service hostname or the `/metrics`
// path would silently break the dashboards.
func TestPrometheusScrapesKeepMetricsPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "deploy", "observability", "prometheus.yml")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got prometheusConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("yaml unmarshal %s: %v", path, err)
	}

	var keepJob *struct {
		JobName       string `yaml:"job_name"`
		MetricsPath   string `yaml:"metrics_path"`
		StaticConfigs []struct {
			Targets []string `yaml:"targets"`
		} `yaml:"static_configs"`
	}
	for i := range got.ScrapeConfigs {
		if got.ScrapeConfigs[i].JobName == "keep" {
			keepJob = &got.ScrapeConfigs[i]
			break
		}
	}
	if keepJob == nil {
		t.Fatalf("prometheus.yml: scrape job %q missing", "keep")
	}
	if keepJob.MetricsPath != "/metrics" {
		t.Errorf("keep scrape job metrics_path = %q, want /metrics", keepJob.MetricsPath)
	}
	var foundTarget bool
	for _, sc := range keepJob.StaticConfigs {
		for _, tgt := range sc.Targets {
			if tgt == "keep:8080" {
				foundTarget = true
			}
		}
	}
	if !foundTarget {
		t.Errorf("keep scrape job static_configs targets missing %q", "keep:8080")
	}
}

// dashboardJSON captures just enough of the Grafana dashboard schema
// to walk every panel's PromQL expressions. Real Grafana panels can
// nest rows arbitrarily deep; we walk one level (the starter
// dashboard's actual shape) for simplicity. Extending to nested rows
// is a one-line recursion change if a future dashboard needs it.
type dashboardJSON struct {
	UID           string `json:"uid"`
	Title         string `json:"title"`
	SchemaVersion int    `json:"schemaVersion"`
	Templating    struct {
		List []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Query any    `json:"query"`
		} `json:"list"`
	} `json:"templating"`
	Panels []struct {
		Type    string `json:"type"`
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func loadDashboard(t *testing.T) dashboardJSON {
	t.Helper()
	path := filepath.Join(repoRoot(t), "deploy", "observability", "grafana", "dashboards", "watchkeeper-phase1.json")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got dashboardJSON
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json unmarshal %s: %v", path, err)
	}
	return got
}

// TestDashboardCoversExpectedFamilies pins the contract between the
// wkmetrics name set and the starter Grafana dashboard. A renamed
// family without a dashboard update would silently blank a panel; the
// test fails loudly instead. expectedFamilies is intentionally
// duplicated from core/pkg/wkmetrics/wkmetrics_test.go — a
// "shared constants" extraction would couple test-only state from
// two packages, and the dupe is a deliberate forcing function: any
// metric rename has to touch BOTH lists, with the test name making
// the intent explicit.
func TestDashboardCoversExpectedFamilies(t *testing.T) {
	t.Parallel()

	expectedFamilies := []string{
		"watchkeeper_llm_tokens_total",
		"watchkeeper_llm_request_duration_seconds",
		"watchkeeper_tool_invocations_total",
		"watchkeeper_eventbus_queue_depth",
		"watchkeeper_messenger_rate_limit_remaining",
		"watchkeeper_http_request_duration_seconds",
		"watchkeeper_outbox_published_total",
	}

	dash := loadDashboard(t)
	allExprs := make([]string, 0, len(dash.Panels))
	for _, p := range dash.Panels {
		for _, t := range p.Targets {
			allExprs = append(allExprs, t.Expr)
		}
	}
	joined := strings.Join(allExprs, "\n")

	var missing []string
	for _, fam := range expectedFamilies {
		// Histograms expose three series suffixes (_bucket / _sum /
		// _count); any one of them references the family. We match
		// the family stem so a panel using `_bucket` for a
		// histogram_quantile still counts.
		if !strings.Contains(joined, fam) {
			missing = append(missing, fam)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("Grafana dashboard missing PromQL coverage for %d wkmetrics families: %v\nAll PromQL exprs:\n%s",
			len(missing), missing, joined)
	}
}

// TestDashboardUIDStable pins the dashboard's `uid` so a future
// rename does not orphan operator-saved permalinks pointing at
// /d/watchkeeper-phase1/...
func TestDashboardUIDStable(t *testing.T) {
	t.Parallel()

	dash := loadDashboard(t)
	if dash.UID != "watchkeeper-phase1" {
		t.Errorf("dashboard uid = %q, want watchkeeper-phase1", dash.UID)
	}
}

// TestIter1_DashboardSchemaVersionGrafana11 pins iter-1 #9: the
// starter dashboard targets Grafana 11+, so the on-disk
// schemaVersion must match (40 as of Grafana 11.5.x). Older
// versions trigger an auto-migration log line on every grafana
// restart and drift the in-memory representation from the file.
func TestIter1_DashboardSchemaVersionGrafana11(t *testing.T) {
	t.Parallel()

	dash := loadDashboard(t)
	if dash.SchemaVersion < 40 {
		t.Errorf("dashboard schemaVersion = %d, want >= 40 (Grafana 11.x baseline; iter-1 #9)", dash.SchemaVersion)
	}
}

// TestIter1_DashboardLLMPanelBoundsCardinality pins iter-1 #7: the
// LLM token-spend panel must NOT group by agent_id in its sum-by
// clause by default. agent_id is unbounded as agents spawn; a
// raw `sum by (agent_id, …)` balloons cardinality on dashboards
// with many spawned agents. The dashboard uses the $agent_id
// template variable for opt-in drill-down instead.
func TestIter1_DashboardLLMPanelBoundsCardinality(t *testing.T) {
	t.Parallel()

	dash := loadDashboard(t)
	var llmPanel *struct {
		Type    string `json:"type"`
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	}
	for i := range dash.Panels {
		if strings.Contains(dash.Panels[i].Title, "LLM token spend") {
			llmPanel = &dash.Panels[i]
			break
		}
	}
	if llmPanel == nil {
		t.Fatalf("LLM token-spend panel missing")
	}
	for _, tgt := range llmPanel.Targets {
		if strings.Contains(tgt.Expr, "sum by (agent_id") {
			t.Errorf("LLM panel target groups by agent_id in default sum-by: %q\nuse $agent_id template variable for drill-down instead", tgt.Expr)
		}
	}
	// Templating must include an `agent_id` variable so drill-down
	// works.
	var hasAgentVar bool
	for _, v := range dash.Templating.List {
		if v.Name == "agent_id" {
			hasAgentVar = true
			break
		}
	}
	if !hasAgentVar {
		t.Errorf("templating.list missing `agent_id` variable for cardinality-bounded drill-down (iter-1 #7)")
	}
}

// TestIter1_DashboardDatasourceTemplating pins iter-1 #19: the
// dashboard uses a `datasource` template variable instead of
// duplicating the uid in every panel. Each panel's datasource.uid
// must reference `${datasource}`.
func TestIter1_DashboardDatasourceTemplating(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "deploy", "observability", "grafana", "dashboards", "watchkeeper-phase1.json")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "\"name\": \"datasource\"") {
		t.Errorf("dashboard templating must define a `datasource` variable (iter-1 #19)")
	}
	// Every datasource ref in panels uses ${datasource}.
	if strings.Contains(src, "\"uid\": \"prometheus\"") {
		// Allowed only in templating defaults (current/value), not
		// in panel datasource refs. Easier: assert the dashboard does
		// not hard-code the uid in panel-level datasource blocks by
		// scanning for the literal panel-shape sequence.
		// The dashboard uses {"type": "prometheus", "uid": "${datasource}"}
		// for every panel; presence of {"type": "prometheus", "uid": "prometheus"}
		// would be the regression.
		if strings.Contains(src, "\"uid\": \"prometheus\"\n      }") || strings.Contains(src, "\"uid\":\"prometheus\"}") {
			t.Errorf("a panel datasource hard-codes uid `prometheus` -- use ${datasource} (iter-1 #19)")
		}
	}
}

// grafanaDatasource is the minimal subset of the provisioned
// datasource the tests pin. The `uid` MUST match every panel's
// `datasource.uid`; a rename here without updating the dashboard
// would blank every panel.
type grafanaDatasourceConfig struct {
	APIVersion  int `yaml:"apiVersion"`
	Datasources []struct {
		Name      string `yaml:"name"`
		UID       string `yaml:"uid"`
		Type      string `yaml:"type"`
		URL       string `yaml:"url"`
		IsDefault bool   `yaml:"isDefault"`
	} `yaml:"datasources"`
}

// TestGrafanaDatasourceUIDStable pins the datasource uid so it stays
// in lockstep with the dashboard panels' datasource.uid references.
func TestGrafanaDatasourceUIDStable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "deploy", "observability", "grafana", "provisioning", "datasources", "prometheus.yml")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got grafanaDatasourceConfig
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("yaml unmarshal %s: %v", path, err)
	}
	if len(got.Datasources) == 0 {
		t.Fatalf("no datasources provisioned")
	}
	first := got.Datasources[0]
	if first.UID != "prometheus" {
		t.Errorf("datasource uid = %q, want prometheus", first.UID)
	}
	if first.URL != "http://prometheus:9090" {
		t.Errorf("datasource url = %q, want http://prometheus:9090", first.URL)
	}
	if first.Type != "prometheus" {
		t.Errorf("datasource type = %q, want prometheus", first.Type)
	}
}
