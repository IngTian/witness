//go:build opencodecontract

package opencode

import (
	"encoding/json"
	"os"
	"testing"
)

func TestContractVersionInRange(t *testing.T) {
	oc := requireContract(t)
	if os.Getenv("WITNESS_CONTRACT_CHANNEL") != "latest" && AboveTested(oc.Version) {
		t.Fatalf("%v is above NewestTestedMinor %v: bump it with the pins (scripts/opencode-contract/pins.txt)", oc.Version, NewestTestedMinor)
	}
}

func TestContractAPIInfoAuth(t *testing.T) { // T10
	oc := requireContract(t)
	noLeaks(t)
	s := scratchEnv(t)
	srv := startRawServe(t, oc.Bin, s)
	if code, _ := srv.Do("GET", "/api/info", nil, false); code != 401 {
		t.Fatalf("no auth: %d, want 401", code)
	}
	wrong := *srv
	wrong.Password = "wrong"
	if code, _ := wrong.Do("GET", "/api/info", nil, true); code != 401 {
		t.Fatalf("wrong password: %d, want 401", code)
	}
	code, body := srv.Do("GET", "/api/info", nil, true)
	var info struct {
		Version string
		Pid     int
	}
	if code != 200 || json.Unmarshal(body, &info) != nil {
		t.Fatalf("auth: %d %s", code, body)
	}
	if v, err := ParseOpenCodeVersion(info.Version); err != nil || v != oc.Version {
		t.Fatalf("/api/info version %q, --version %v", info.Version, oc.Version)
	}
	if info.Pid != srv.Cmd.Process.Pid {
		t.Fatalf("/api/info pid %d, spawned pid %d (ResolveExe must hand back the real exe, not a .cmd)", info.Pid, srv.Cmd.Process.Pid)
	}
}
