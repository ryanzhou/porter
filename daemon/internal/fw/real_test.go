package fw

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/config"
	"github.com/ryanzhou/porter/daemon/internal/geoip"
)

// TestRealCountrySet loads a ruleset with a large country's full address
// ranges into the kernel when PORTER_GEOIP_FILE names a DB-IP file.
func TestRealCountrySet(t *testing.T) {
	path := os.Getenv("PORTER_GEOIP_FILE")
	if path == "" || os.Getuid() != 0 {
		t.Skip("needs root and PORTER_GEOIP_FILE")
	}
	db, err := geoip.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Policy.Rules = []config.Rule{{ID: "us", Enabled: true, Dst: config.Dst{Countries: []string{"US", "CN", "DE"}}, Action: config.ActBlock}}
	p := BuildPlan(c, Runtime{WANIfs: []string{"eth0"}, Geo: db.Ranges})
	start := time.Now()
	script := Render(p)
	rendered := time.Since(start)
	if fp := Fingerprint(p); len(fp) > 20000 {
		t.Errorf("fingerprint is %d bytes; it should not carry the ranges", len(fp))
	}
	cmd := exec.Command("unshare", "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	start = time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected the ruleset: %v\n%.2000s", err, out)
	}
	t.Logf("three large countries: %d kB script rendered in %v, loaded by the kernel in %v",
		len(script)/1024, rendered.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
}
