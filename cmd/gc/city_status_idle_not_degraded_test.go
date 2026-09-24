package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// idleCity is a city with the given agents and nothing else configured.
func idleCity(agents ...config.Agent) *config.City {
	return &config.City{Workspace: config.Workspace{Name: "city"}, Agents: agents}
}

// idleCityStatus builds the JSON status for cfg with every agent observed
// stopped and no work in the store. The controller is forced to running so the
// only signals that can appear are the ones derived from the agent fleet and
// from city suspension.
func idleCityStatus(t *testing.T, cfg *config.City) StatusJSON {
	t.Helper()
	snapshot := collectCityStatusSnapshot(runtime.NewFake(), cfg, t.TempDir(), beads.NewMemStore(), io.Discard)
	snapshot.Controller.Running = true
	return cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)
}

// TestIdleMinZeroCityIsNotDegraded reproduces gastownhall/gascity#6437: a city
// whose pools are all min=0 and which has no routed work is idle by design,
// yet gc status --json reported no_agents_running and degraded=true.
func TestIdleMinZeroCityIsNotDegraded(t *testing.T) {
	status := idleCityStatus(t, idleCity(
		config.Agent{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		config.Agent{Name: "control-dispatcher", MaxActiveSessions: intPtr(1)},
	))

	if status.Summary.TotalAgents != 3 || status.Summary.RunningAgents != 0 {
		t.Fatalf("summary = %d/%d running, want 0/3", status.Summary.RunningAgents, status.Summary.TotalAgents)
	}
	if slices.Contains(status.Health.Signals, "no_agents_running") {
		t.Errorf("health signals = %v, want no no_agents_running for an idle min=0 city", status.Health.Signals)
	}
	if status.Health.Degraded {
		t.Errorf("health.degraded = true (signals %v), want false for an idle min=0 city", status.Health.Signals)
	}
}

// TestStoppedMinOnePoolStillSignalsNoAgentsRunning is the positive control for
// the pool floor: a pool that must keep a session up and has none running is
// still degraded.
func TestStoppedMinOnePoolStillSignalsNoAgentsRunning(t *testing.T) {
	status := idleCityStatus(t, idleCity(
		config.Agent{Name: "dog", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
	))

	if !slices.Contains(status.Health.Signals, "no_agents_running") {
		t.Fatalf("health signals = %v, want no_agents_running when a min=1 pool has nothing running", status.Health.Signals)
	}
	if !status.Health.Degraded {
		t.Fatal("health.degraded = false, want true when a min=1 pool has nothing running")
	}
}

// TestStoppedAlwaysOnNamedSessionStillSignalsNoAgentsRunning is the positive
// control for named sessions: the controller keeps a mode="always" session
// running whenever its template is not suspended, so a city with nothing
// running is still degraded even though every pool is min=0.
func TestStoppedAlwaysOnNamedSessionStillSignalsNoAgentsRunning(t *testing.T) {
	cfg := idleCity(
		config.Agent{Name: "lead"},
		config.Agent{Name: "dog", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
	)
	cfg.NamedSessions = []config.NamedSession{{Template: "lead", Mode: "always"}}

	status := idleCityStatus(t, cfg)

	if !slices.Contains(status.Health.Signals, "no_agents_running") {
		t.Fatalf("health signals = %v, want no_agents_running when an always-on named session is down", status.Health.Signals)
	}
}

// TestSuspendedCityIsDegradedOnlyBySuspension pins the part of #6437 this fix
// leaves alone. A suspended city keeps nothing running, so no_agents_running is
// quiet even with a min=1 pool and an always-on named session configured, but
// city_suspended still marks the city degraded.
func TestSuspendedCityIsDegradedOnlyBySuspension(t *testing.T) {
	cfg := idleCity(
		config.Agent{Name: "lead"},
		config.Agent{Name: "dog", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
	)
	cfg.Workspace.SuspendedOnStart = true
	cfg.NamedSessions = []config.NamedSession{{Template: "lead", Mode: "always"}}

	status := idleCityStatus(t, cfg)

	if !slices.Equal(status.Health.Signals, []string{"city_suspended"}) {
		t.Fatalf("health signals = %v, want [city_suspended]", status.Health.Signals)
	}
	if !status.Health.Degraded {
		t.Fatal("health.degraded = false, want true for a suspended city")
	}
}

// TestCityIdleByConfig covers the floor rule: config keeps a session running
// when an agent that is not effectively suspended has min_active_sessions > 0,
// or when a mode="always" named session has a template that is not
// effectively suspended. City, rig, and agent suspension each remove the floor,
// whether set in config or at runtime.
func TestCityIdleByConfig(t *testing.T) {
	suspended := true
	pool := func(minSessions int) config.Agent {
		return config.Agent{Name: "dog", MinActiveSessions: intPtr(minSessions), MaxActiveSessions: intPtr(2)}
	}
	rigPool := func(rigSuspendedOnStart bool) *config.City {
		a := pool(1)
		a.Dir = "r"
		return &config.City{
			Rigs:   []config.Rig{{Name: "r", Path: "/rigs/r", SuspendedOnStart: rigSuspendedOnStart}},
			Agents: []config.Agent{a},
		}
	}
	alwaysOn := func(template config.Agent) *config.City {
		return &config.City{
			Agents:        []config.Agent{template, pool(0)},
			NamedSessions: []config.NamedSession{{Template: "lead", Dir: template.Dir, Mode: "always"}},
		}
	}
	suspendedCityState := suspensionstate.State{City: suspensionstate.Override{Suspended: &suspended}}
	suspendedRigState := suspensionstate.State{Rigs: map[string]suspensionstate.Override{"r": {Suspended: &suspended}}}

	tests := []struct {
		name string
		cfg  *config.City
		st   suspensionstate.State
		want bool
	}{
		{name: "nil config is not known to be idle", cfg: nil, want: false},
		{name: "min=0 pool", cfg: &config.City{Agents: []config.Agent{pool(0)}}, want: true},
		{name: "min=1 pool", cfg: &config.City{Agents: []config.Agent{pool(1)}}, want: false},
		{name: "min=1 pool suspended in config", cfg: &config.City{Agents: []config.Agent{{Name: "dog", MinActiveSessions: intPtr(1), Suspended: true}}}, want: true},
		{name: "min=1 pool in rig suspended on start", cfg: rigPool(true), want: true},
		{name: "min=1 pool in rig suspended at runtime", cfg: rigPool(false), st: suspendedRigState, want: true},
		{name: "min=1 pool in city suspended on start", cfg: &config.City{Workspace: config.Workspace{SuspendedOnStart: true}, Agents: []config.Agent{pool(1)}}, want: true},
		{name: "min=1 pool in city suspended at runtime", cfg: &config.City{Agents: []config.Agent{pool(1)}}, st: suspendedCityState, want: true},
		{name: "always-on named session", cfg: alwaysOn(config.Agent{Name: "lead"}), want: false},
		{name: "always-on named session with suspended template", cfg: alwaysOn(config.Agent{Name: "lead", Suspended: true}), want: true},
		{name: "always-on named session in city suspended at runtime", cfg: alwaysOn(config.Agent{Name: "lead"}), st: suspendedCityState, want: true},
		{
			name: "always-on named session with template in live rig",
			cfg: func() *config.City {
				cfg := alwaysOn(config.Agent{Name: "lead", Dir: "r"})
				cfg.Rigs = []config.Rig{{Name: "r", Path: "/rigs/r"}}
				return cfg
			}(),
			want: false,
		},
		{
			name: "always-on named session with template in suspended rig",
			cfg: func() *config.City {
				cfg := alwaysOn(config.Agent{Name: "lead", Dir: "r"})
				cfg.Rigs = []config.Rig{{Name: "r", Path: "/rigs/r"}}
				return cfg
			}(),
			st:   suspendedRigState,
			want: true,
		},
		{
			name: "on_demand named session",
			cfg: &config.City{
				Agents:        []config.Agent{{Name: "lead"}, pool(0)},
				NamedSessions: []config.NamedSession{{Template: "lead"}},
			},
			want: true,
		},
		{
			name: "always-on named session without a template",
			cfg: &config.City{
				Agents:        []config.Agent{pool(0)},
				NamedSessions: []config.NamedSession{{Template: "lead", Mode: "always"}},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cityIdleByConfig(tt.cfg, t.TempDir(), tt.st); got != tt.want {
				t.Errorf("cityIdleByConfig = %v, want %v", got, tt.want)
			}
		})
	}
}

// writeIdlePoolStatusCity writes a city with one bounded pool of the given
// minimum and returns its path and loaded config.
func writeIdlePoolStatusCity(t *testing.T, minSessions int) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "dog"
min_active_sessions = %d
max_active_sessions = 2
`, minSessions)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	return cityPath, cfg
}

// idlePoolStatusView is the supervisor's view of that city: both pool slots
// stopped, and the city suspended when suspended is set.
func idlePoolStatusView(suspended bool) api.StatusView {
	return api.StatusView{
		CityName:  "test-city",
		CityPath:  "/tmp/test-city",
		Suspended: suspended,
		Agents: []api.StatusAgentView{
			{Name: "dog-1", QualifiedName: "dog-1", Scope: "city", SessionName: "dog-1", GroupName: "dog", Expanded: true},
			{Name: "dog-2", QualifiedName: "dog-2", Scope: "city", SessionName: "dog-2", GroupName: "dog", Expanded: true},
		},
		Summary: api.StatusSummaryView{TotalAgents: 2, RunningAgents: 0},
	}
}

// TestCityStatusAppliesConfigFloorOnBothRoutes covers the wiring. The
// StatusBody carries no pool floor, so the API route must apply the same config
// rule as the local fallback, including runtime suspension state. The fallback
// half runs routeCityStatus with no client. The API half skips routeCityStatus
// and hands the supervisor's view to renderCityStatusFromAPI: routing to it
// takes a test HTTP server, and the repo's resource ledger
// (test/test-resources.toml) freezes new ones.
func TestCityStatusAppliesConfigFloorOnBothRoutes(t *testing.T) {
	tests := []struct {
		name             string
		minSessions      int
		runtimeSuspended bool
		want             bool
	}{
		{name: "min=0 pool is idle by design", minSessions: 0, want: false},
		{name: "min=1 pool with nothing running", minSessions: 1, want: true},
		{name: "min=1 pool in city suspended at runtime", minSessions: 1, runtimeSuspended: true, want: false},
	}
	for _, tt := range tests {
		for _, route := range []string{"api", "fallback"} {
			t.Run(tt.name+"/"+route, func(t *testing.T) {
				cityPath, cfg := writeIdlePoolStatusCity(t, tt.minSessions)
				if tt.runtimeSuspended {
					suspended := true
					if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
						t.Fatal(err)
					}
				}
				var stdout, stderr bytes.Buffer
				var code int
				if route == "api" {
					cr := api.CachedRead[api.StatusView]{Body: idlePoolStatusView(tt.runtimeSuspended)}
					code = renderCityStatusFromAPI(cityPath, cfg, cr, newFakeDrainOps(), true, &stdout)
				} else {
					code = routeCityStatus(cityPath, cfg, runtime.NewFake(), newFakeDrainOps(), nil, "controller-down", true, &stdout, &stderr)
				}
				if code != 0 {
					t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
				}
				var status StatusJSON
				if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
					t.Fatalf("unmarshal: %v; output: %s", err, stdout.String())
				}
				if status.Summary.TotalAgents != 2 || status.Summary.RunningAgents != 0 {
					t.Fatalf("summary = %d/%d running, want 0/2", status.Summary.RunningAgents, status.Summary.TotalAgents)
				}
				if got := slices.Contains(status.Health.Signals, "no_agents_running"); got != tt.want {
					t.Errorf("no_agents_running signaled = %v, want %v (signals %v)", got, tt.want, status.Health.Signals)
				}
			})
		}
	}
}

// TestPartialStatusOfIdleCityStillSignalsAgentStateUnknown pins that the config
// floor never hides an unobserved fleet: agent_state_unknown wins even when the
// city is idle by config.
func TestPartialStatusOfIdleCityStillSignalsAgentStateUnknown(t *testing.T) {
	snapshot := fleetSnapshot(0, 15, true, []string{"runtime status probe incomplete"})
	snapshot.IdleByConfig = true

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)

	if !slices.Contains(status.Health.Signals, "agent_state_unknown") {
		t.Fatalf("health signals = %v, want agent_state_unknown during partial status even when idle by config", status.Health.Signals)
	}
}
