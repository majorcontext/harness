package main

import (
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/process"
)

func TestBuildProcessManagerEmptyNotAlwaysOn(t *testing.T) {
	mgr := buildProcessManager(t.TempDir(), nil, false)
	if mgr != nil {
		t.Errorf("buildProcessManager(nil, alwaysOn=false) = %v, want nil", mgr)
	}
	if reg := processRegistry(mgr); reg != nil {
		t.Errorf("processRegistry(nil manager) = %v, want a true nil interface", reg)
	}
	closeProcessManager(mgr)
}

func TestBuildProcessManagerEmptyAlwaysOn(t *testing.T) {
	mgr := buildProcessManager(t.TempDir(), nil, true)
	if mgr == nil {
		t.Fatal("buildProcessManager(nil, alwaysOn=true) = nil, want a non-nil manager")
	}
	if reg := processRegistry(mgr); reg == nil {
		t.Fatal("processRegistry(non-nil manager) returned a nil interface")
	}
	if len(mgr.List()) != 0 {
		t.Errorf("List() = %+v, want empty", mgr.List())
	}
	closeProcessManager(mgr)
}

func TestBuildProcessManagerConvertsSpecs(t *testing.T) {
	mgr := buildProcessManager(t.TempDir(), map[string]config.ProcessSpec{
		"dev": {Command: []string{"pnpm", "dev"}, Dir: "apps/app", Env: []string{"K=V"}, ReadyRegex: "Ready in .*ms", ReadyTimeoutS: 5},
	}, false)
	if mgr == nil {
		t.Fatal("buildProcessManager returned nil for a non-empty processes map")
	}
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("List() = %+v, want 1 entry", infos)
	}
	info := infos[0]
	if info.Name != "dev" || len(info.Command) != 2 || info.Command[0] != "pnpm" {
		t.Errorf("info = %+v", info)
	}
	if info.Origin != process.OriginConfig {
		t.Errorf("Origin = %q, want config", info.Origin)
	}
	if info.ReadyTimeout != (5 * time.Second).String() {
		t.Errorf("ReadyTimeout = %q, want 5s", info.ReadyTimeout)
	}
	closeProcessManager(mgr)
}
