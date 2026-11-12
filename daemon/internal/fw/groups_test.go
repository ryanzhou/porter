package fw

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ryanzhou/porter/daemon/internal/config"
)

// Groups in the ruleset: a member without a route of its own takes the
// group's, a rule can name a group as its source, and a device that is
// cut off through its group is treated exactly as one that is paused
// itself, over IPv4 and IPv6 alike, since both are matched by hardware
// address.
func TestRenderGroups(t *testing.T) {
	const (
		tablet  = "02:00:00:00:00:21"
		console = "02:00:00:00:00:22"
		laptop  = "02:00:00:00:00:23"
		tv      = "02:00:00:00:00:24"
		nas     = "02:00:00:00:00:10"
	)
	c := inboundConfig()
	c.System.Timezone = "Europe/Berlin"
	c.Devices = append(c.Devices, config.Device{MAC: laptop, Route: "direct"})
	c.Groups = []config.Group{
		{ID: "kids", Name: "Kids", Members: []string{tablet, console, laptop, nas}, Route: "vpn:work",
			Schedules: []config.Schedule{{ID: "bed", Enabled: true, Days: []int{0, 1, 2, 3, 4}, From: "21:00", To: "07:00"}}},
		{ID: "media", Name: "Media", Members: []string{tv}},
		{ID: "empty", Name: "Nobody yet"},
	}
	c.Policy.Rules = append(c.Policy.Rules,
		config.Rule{ID: "g1", Name: "Kids and one more", Enabled: true, Src: config.Src{Groups: []string{"kids", "media"}, Devices: []string{"02:00:00:00:00:01"}}, Dst: config.Dst{Ports: []string{"6881"}}, Action: "block"},
		config.Rule{ID: "g2", Name: "Nobody", Enabled: true, Src: config.Src{Groups: []string{"empty"}}, Dst: config.Dst{Domains: []string{"example.org"}}, Action: "block"})
	c.Normalize()
	if err := c.Validate(); err != nil && !strings.Contains(err.Error(), "private key") {
		t.Fatal(err)
	}

	// During the day nobody is cut off through a group.
	day := BuildPlan(c, Runtime{WANIfs: []string{"eth0"}})
	out := Render(day)
	for _, want := range []string{
		// The group's route for its members, the device's own where it
		// has one.
		tablet + ` : 0x11`, console + ` : 0x11`, nas + ` : 0x11`,
		laptop + ` : 0x1`,
		// The rule's source: the device named and every member of the
		// two groups.
		`ether saddr { 02:00:00:00:00:01, ` + nas + `, ` + tablet + `, ` + console + `, ` + laptop + `, ` + tv + ` } jump r_g1`,
		// Only the two devices that are paused themselves are cut off.
		`set paused { type ether_addr; elements = { 02:00:00:00:00:02, 02:00:00:00:00:12 } }`,
		// The port forwarded to a member of the group works.
		`dnat ip to 10.83.0.10:443`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("by day, the ruleset lacks %q", want)
		}
	}
	// A rule for a group without members is a rule for nobody: it must
	// not turn into one for everybody.
	if strings.Contains(out, "r_g2") {
		t.Errorf("the rule for an empty group is in the ruleset:\n%s", out)
	}

	// At night the group is offline: the control loop hands over its
	// members, except the laptop, which was let through.
	night := BuildPlan(c, Runtime{WANIfs: []string{"eth0"}, Held: []string{tablet, console, nas}})
	out = Render(night)
	for _, want := range []string{
		`set paused { type ether_addr; elements = { 02:00:00:00:00:02, ` + nas + `, 02:00:00:00:00:12, ` + tablet + `, ` + console + ` } }`,
		// The same two rules that cut a paused device off, which match
		// by hardware address whatever the address family.
		`ether saddr @paused meta mark set 0x2 ct mark set 0x2 accept`,
		"chain lan_lan {\n\t\tether saddr @paused jump refuse",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("by night, the ruleset lacks %q", want)
		}
	}
	// Like a paused device's, its forwarded ports are closed.
	if strings.Contains(out, "10.83.0.10") {
		t.Error("a port is still forwarded to a device that is offline with its group")
	}
	for _, f := range night.Forwards {
		if f.To4.String() == "10.83.0.10" {
			t.Errorf("forward %s still leads to the device that is offline", f.ID)
		}
	}
	// Going offline, and coming back, is a change to where connections
	// go: those under way have to be ended.
	if ClassKey(day) == ClassKey(night) {
		t.Error("ClassKey is the same with a group offline and online")
	}

	if os.Getuid() != 0 {
		t.Skip("not root: skipping kernel check")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	cmd := exec.Command("unshare", "-n", "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kernel rejected ruleset: %v\n%s\n%s", err, b, out)
	}
}
