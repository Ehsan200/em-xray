package daemon

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// xrayAPITimeout bounds each `xray api` call.
const xrayAPITimeout = 5 * time.Second

// apiCmd builds a `xray api <sub> --server=127.0.0.1:<apiPort> -t=5 [args...]`
// command against the running child's gRPC api inbound.
func (s *Supervisor) apiCmd(sub string, args ...string) (*exec.Cmd, error) {
	bin, err := xraybin.Extract(s.paths.Cache) // cached; hash-gated no-op if present
	if err != nil {
		return nil, err
	}
	full := append([]string{
		"api", sub,
		"--server=127.0.0.1:" + strconv.Itoa(xray.ApiPort),
		"-t=" + strconv.Itoa(int(xrayAPITimeout.Seconds())),
	}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+s.paths.AssetDir())
	return cmd, nil
}

// apiAddOutbounds live-adds outbounds from one-outbound JSON files (`ado`).
func (s *Supervisor) apiAddOutbounds(files ...string) error {
	if len(files) == 0 {
		return nil
	}
	cmd, err := s.apiCmd("ado", files...)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("api ado: %v: %s", err, out)
	}
	return nil
}

// apiRemoveOutbounds live-removes outbounds by tag (`rmo`). "Already gone"
// errors are tolerated by the caller.
func (s *Supervisor) apiRemoveOutbounds(tags ...string) error {
	if len(tags) == 0 {
		return nil
	}
	cmd, err := s.apiCmd("rmo", tags...)
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("api rmo: %v: %s", err, out)
	}
	return nil
}

// StatsQuery reads all xray traffic counters (`statsquery`, no reset) and returns
// them parsed. Counters are cumulative since the xray process started.
func (s *Supervisor) StatsQuery() ([]xray.StatCounter, error) {
	cmd, err := s.apiCmd("statsquery")
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("api statsquery: %w", err)
	}
	var payload struct {
		Stat []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse statsquery: %w", err)
	}
	res := make([]xray.StatCounter, 0, len(payload.Stat))
	for _, e := range payload.Stat {
		if c, ok := xray.ParseStatName(e.Name, xray.StatValueToInt(e.Value)); ok {
			res = append(res, c)
		}
	}
	return res, nil
}

// BalancerInfoRaw returns the raw `xray api bi` text for the given balancer
// tags (BalancerInfo requires explicit tags). Used to read the current winner.
func (s *Supervisor) BalancerInfoRaw(tags ...string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	cmd, err := s.apiCmd("bi", tags...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("api bi: %v: %s", err, out)
	}
	return string(out), nil
}
