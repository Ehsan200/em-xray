package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehsan200/em-xray/core/xray"
	"github.com/ehsan200/em-xray/internal/xraybin"
)

// TestTemplatesValidateWithXray generates a config for every built-in template
// and runs `xray -test` on it. `-test` only validates — it binds no ports — so
// it is safe to run alongside any xray already on the host.
func TestTemplatesValidateWithXray(t *testing.T) {
	if xraybin.Version() == "none" {
		t.Skip("no embedded xray: run scripts/fetch-xray.sh")
	}
	dir := t.TempDir()
	bin, err := xraybin.Extract(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tmpl := range xray.TemplateList() {
		t.Run(tmpl.Name, func(t *testing.T) {
			in, err := xray.NewInboundFromTemplate("srv", tmpl.Name, "direct")
			if err != nil {
				t.Fatal(err)
			}
			in.Port = 23456
			cfg, err := xray.Generate(nil, []xray.Inbound{*in}, nil, xray.GenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tmpl.Name+".json")
			if err := writeFileAtomic(path, cfg); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, "-test", "-c", path)
			cmd.Env = append(cmd.Env, "XRAY_LOCATION_ASSET="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("xray rejected %s config: %v\n%s", tmpl.Name, err, out)
			}
			if !strings.Contains(string(out), "Configuration OK") {
				t.Errorf("no 'Configuration OK' for %s:\n%s", tmpl.Name, out)
			}
		})
	}
}
