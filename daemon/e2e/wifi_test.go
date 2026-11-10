//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ryanzhou/porter/daemon/internal/client"
	"github.com/ryanzhou/porter/daemon/internal/config"
)

// TestWifi runs porterd against simulated radios (mac80211_hwsim): a
// "hotel" access point upstream, two radios in the router, and a client
// device. It covers what the namespace test cannot: hostapd and
// wpa_supplicant driven for real, scanning, joining with a wrong and a
// right password, clients on the router's own access point, and one
// radio doing both jobs.
//
// Skipped when the kernel module or the Wi-Fi daemons are not available.
func TestWifi(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	for _, tool := range []string{"iw", "hostapd", "hostapd_cli", "wpa_supplicant", "wpa_cli", "dnsmasq", "dhcpcd", "modprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	const (
		nsHotel  = "wf-isp"
		nsRouter = "wf-rtr"
		nsClient = "wf-cl"
	)
	dir := t.TempDir()
	run, err := os.MkdirTemp("/tmp", "wf-run-")
	if err != nil {
		t.Fatal(err)
	}
	var procs []*exec.Cmd
	cleanup := func() {
		for _, p := range procs {
			if p.Process != nil {
				p.Process.Signal(os.Interrupt)
			}
		}
		time.Sleep(300 * time.Millisecond)
		for _, p := range procs {
			if p.Process != nil {
				p.Process.Kill()
				p.Wait()
			}
		}
		for _, ns := range []string{nsHotel, nsRouter, nsClient} {
			try("ip netns pids %s 2>/dev/null | xargs -r kill -9", ns)
			try("ip netns del %s", ns)
		}
		try("modprobe -r mac80211_hwsim")
		try("iw reg set 00")
		os.RemoveAll(run)
	}
	cleanup()
	if err := exec.Command("modprobe", "mac80211_hwsim", "radios=5").Run(); err != nil {
		t.Skip("mac80211_hwsim is not available on this kernel:", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"porterd.log", "hotel-hostapd.log", "hotel5-hostapd.log", "client-wpa.log"} {
				if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
					lines := strings.Split(string(b), "\n")
					if len(lines) > 150 {
						lines = lines[len(lines)-150:]
					}
					t.Logf("%s (tail):\n%s", name, strings.Join(lines, "\n"))
				}
			}
		}
		cleanup()
	})
	start := func(name, logTo string, argv ...string) {
		cmd := exec.Command(argv[0], argv[1:]...)
		f, _ := os.Create(filepath.Join(dir, logTo))
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		procs = append(procs, cmd)
	}

	// Five simulated radios: one for the hotel, two for the router, one
	// for the client, and a second one for the hotel, which later puts
	// the same network on 5 GHz as well.
	var phys []string
	entries, _ := os.ReadDir("/sys/class/ieee80211")
	for _, e := range entries {
		if l, err := os.Readlink(filepath.Join("/sys/class/ieee80211", e.Name(), "device", "driver")); err == nil && filepath.Base(l) == "mac80211_hwsim" {
			phys = append(phys, e.Name())
		}
	}
	sort.Strings(phys)
	if len(phys) < 5 {
		t.Skipf("expected 5 simulated radios, found %d", len(phys))
	}
	// The regulatory domain belongs to the machine, not to a network
	// namespace, and only the first namespace may set it. Without one,
	// nothing may transmit first on 5 GHz, so the hotel could not have an
	// access point there.
	sh(t, "iw reg set US")
	eventually(t, "regulatory domain set", 10*time.Second, func() bool {
		out, _ := exec.Command("iw", "reg", "get").Output()
		return strings.Contains(string(out), "country US")
	})
	for _, ns := range []string{nsHotel, nsRouter, nsClient} {
		sh(t, "ip netns add %s && ip -n %s link set lo up", ns, ns)
	}
	for i, ns := range []string{nsHotel, nsRouter, nsRouter, nsClient, nsHotel} {
		sh(t, "iw phy %s set netns name %s", phys[i], ns)
	}
	wlans := func(ns string) []string {
		out := sh(t, "ip netns exec %s sh -c 'ls /sys/class/net'", ns)
		var names []string
		for _, n := range strings.Fields(out) {
			if strings.HasPrefix(n, "wlan") {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		return names
	}
	hotelIf, hotel5If, clientIf := wlans(nsHotel)[0], wlans(nsHotel)[1], wlans(nsClient)[0]

	// --- the hotel ---------------------------------------------------------------
	const hotelSSID, hotelPass = "HotelNet", "hotelpass123"
	hotelConf := filepath.Join(dir, "hotel-hostapd.conf")
	// Both of the hotel's access points are ports of one bridge, as the
	// access points of one network are.
	const hotelBridge = "brhotel"
	sh(t, "ip -n %s link add %s type bridge && ip -n %s link set %s up", nsHotel, hotelBridge, nsHotel, hotelBridge)
	os.WriteFile(hotelConf, []byte(fmt.Sprintf(
		"interface=%s\nbridge=%s\ndriver=nl80211\nssid=%s\nhw_mode=g\nchannel=1\nwpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nwpa_passphrase=%s\n",
		hotelIf, hotelBridge, hotelSSID, hotelPass)), 0o600)
	start("hotel hostapd", "hotel-hostapd.log", "ip", "netns", "exec", nsHotel, "hostapd", hotelConf)
	eventually(t, "hotel access point", 20*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "hotel-hostapd.log"))
		return strings.Contains(string(b), "AP-ENABLED")
	})
	sh(t, "ip -n %s addr add %s/24 dev %s", nsHotel, ispIP, hotelBridge)
	sh(t, "ip -n %s addr add %s/32 dev lo", nsHotel, siteA)
	hotelDNS := filepath.Join(dir, "hotel-dnsmasq.conf")
	os.WriteFile(hotelDNS, []byte(strings.Join([]string{
		"interface=" + hotelBridge, "except-interface=lo", "bind-interfaces", "no-resolv", "no-hosts", "user=root", "keep-in-foreground",
		"dhcp-range=100.64.0.50,100.64.0.99,255.255.255.0,10m", "dhcp-option=option:router," + ispIP,
		"dhcp-option=option:dns-server," + ispIP, "dhcp-leasefile=" + filepath.Join(dir, "hotel.leases"),
		"address=/probe.test/" + siteA,
	}, "\n")+"\n"), 0o644)
	start("hotel dnsmasq", "hotel-dnsmasq.log", "ip", "netns", "exec", nsHotel, "dnsmasq", "--conf-file="+hotelDNS)
	go http.Serve(listen(t, nsHotel, siteA+":80"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			fmt.Fprint(w, "Success")
			return
		}
		fmt.Fprint(w, strings.Split(r.RemoteAddr, ":")[0])
	}))

	// --- the router -----------------------------------------------------------------
	bin := daemonBinary(t, dir)
	start("porterd", "porterd.log", "ip", "netns", "exec", nsRouter, bin, "-data", filepath.Join(dir, "data"), "-run", run, "-no-ble", "-debug")
	var c *client.Client
	eventually(t, "control socket", 10*time.Second, func() bool {
		c, err = client.DialUnix(filepath.Join(run, "control.sock"))
		return err == nil
	})
	defer c.Close()

	type radio struct {
		ID      string `json:"id"`
		Iface   string `json:"iface"`
		AP      bool   `json:"ap"`
		STA     bool   `json:"sta"`
		APSTA   bool   `json:"ap_sta"`
		Present bool   `json:"present"`
	}
	type wifiStatus struct {
		Internet string `json:"internet"`
		Active   string `json:"active"`
		WAN      []struct {
			If   string `json:"if"`
			IPv4 string `json:"ipv4"`
		} `json:"wan"`
		APs []struct {
			Radio   string `json:"radio"`
			Iface   string `json:"iface"`
			State   string `json:"state"`
			Channel int    `json:"channel"`
			Mode    string `json:"mode"`
			Error   string `json:"error"`
		} `json:"aps"`
		STAs []struct {
			Radio      string `json:"radio"`
			State      string `json:"state"`
			SSID       string `json:"ssid"`
			Freq       int    `json:"freq"`
			Failed     string `json:"failed"`
			FailReason string `json:"fail_reason"`
		} `json:"stas"`
	}
	getStatus := func() wifiStatus {
		var st wifiStatus
		if err := c.Call("status.get", nil, &st); err != nil {
			t.Fatalf("status.get: %v", err)
		}
		return st
	}
	editConfig := func(f func(cfg *config.Config)) {
		t.Helper()
		var cfg config.Config
		if err := c.Call("config.get", nil, &cfg); err != nil {
			t.Fatal(err)
		}
		f(&cfg)
		if err := c.Call("config.set", map[string]any{"config": &cfg}, nil); err != nil {
			t.Fatalf("config.set: %v", err)
		}
	}

	var radios []radio
	eventually(t, "both radios discovered", 15*time.Second, func() bool {
		radios = nil
		c.Call("radios.list", nil, &radios)
		n := 0
		for _, r := range radios {
			if r.Present {
				n++
			}
		}
		return n == 2
	})
	var hw []radio
	for _, r := range radios {
		if r.Present {
			hw = append(hw, r)
		}
	}
	for _, r := range hw {
		if !r.AP || !r.STA {
			t.Fatalf("radio capabilities misdetected: %+v", r)
		}
	}
	apRadio, wanRadio := hw[0], hw[1]
	t.Logf("radios: access point on %s (%s), uplink on %s (%s), simultaneous AP+station: %v",
		apRadio.ID, apRadio.Iface, wanRadio.ID, wanRadio.Iface, apRadio.APSTA)

	const ssid, pass = "PorterTest", "portertest123"
	editConfig(func(cfg *config.Config) {
		cfg.System.Country = "US"
		cfg.AP.SSID, cfg.AP.Passphrase = ssid, pass
		cfg.WAN.Probe = config.Probe{URL: "http://probe.test/ok", Expect: "Success"}
		for i := range cfg.Radios {
			switch cfg.Radios[i].ID {
			case apRadio.ID:
				cfg.Radios[i].Role, cfg.Radios[i].Band = config.RoleAP, "2.4"
			case wanRadio.ID:
				cfg.Radios[i].Role = config.RoleWAN
			}
		}
	})

	t.Run("AccessPoint", func(t *testing.T) {
		eventually(t, "router access point up", 60*time.Second, func() bool {
			s := getStatus()
			return len(s.APs) == 1 && s.APs[0].State == "up"
		})
		s := getStatus()
		t.Logf("access point: %+v", s.APs[0])
		if s.APs[0].Iface != apRadio.Iface || s.APs[0].Channel != 6 {
			t.Errorf("unexpected access point: %+v", s.APs[0])
		}
	})

	t.Run("JoinUpstream", func(t *testing.T) {
		type scanned struct {
			SSID     string `json:"ssid"`
			Security string `json:"security"`
			Known    bool   `json:"known"`
		}
		eventually(t, "hotel network in scan results", 60*time.Second, func() bool {
			var res []scanned
			if err := c.Call("wifi.scan", nil, &res); err != nil {
				return false
			}
			for _, r := range res {
				if r.SSID == hotelSSID {
					return r.Security == "psk" && !r.Known
				}
			}
			return false
		})
		// A wrong password is reported as such.
		if err := c.Call("wifi.connect", config.WifiNetwork{SSID: hotelSSID, PSK: "not-the-password"}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "wrong password reported", 90*time.Second, func() bool {
			s := getStatus()
			return len(s.STAs) == 1 && s.STAs[0].Failed == hotelSSID && s.STAs[0].FailReason == "wrong_password"
		})
		if err := c.Call("wifi.connect", config.WifiNetwork{SSID: hotelSSID, PSK: hotelPass}, nil); err != nil {
			t.Fatal(err)
		}
		eventually(t, "uplink online over Wi-Fi", 90*time.Second, func() bool {
			s := getStatus()
			return len(s.STAs) == 1 && s.STAs[0].State == "connected" && s.STAs[0].SSID == hotelSSID &&
				s.Internet == "online" && s.Active == wanRadio.Iface
		})
	})

	// --- a client on the router's Wi-Fi ---------------------------------------------
	clientWPA := filepath.Join(dir, "client-wpa.conf")
	os.WriteFile(clientWPA, []byte(fmt.Sprintf("ctrl_interface=%s\nnetwork={\n\tssid=\"%s\"\n\tpsk=\"%s\"\n}\n",
		filepath.Join(dir, "client-wpa"), ssid, pass)), 0o600)
	start("client wpa_supplicant", "client-wpa.log", "ip", "netns", "exec", nsClient, "wpa_supplicant", "-i", clientIf, "-D", "nl80211", "-c", clientWPA)
	clientDHCP := filepath.Join(dir, "client-dhcpcd.conf")
	os.WriteFile(clientDHCP, []byte("script /bin/true\nnoipv4ll\nipv4only\n"), 0o644)
	start("client dhcpcd", "client-dhcpcd.log", "ip", "netns", "exec", nsClient, "dhcpcd", "--nobackground", "--config", clientDHCP, clientIf)

	wanIP := func() string {
		for _, w := range getStatus().WAN {
			if w.IPv4 != "" {
				return strings.Split(w.IPv4, "/")[0]
			}
		}
		return ""
	}
	t.Run("ClientThroughRouter", func(t *testing.T) {
		eventually(t, "wireless client reaching the internet through the router", 90*time.Second, func() bool {
			got, err := httpGet(nsClient, "http://"+siteA+"/ip")
			return err == nil && got != "" && got == wanIP()
		})
		clientMAC := strings.TrimSpace(sh(t, "ip netns exec %s cat /sys/class/net/%s/address", nsClient, clientIf))
		eventually(t, "client listed as a wireless device", 20*time.Second, func() bool {
			var devs []struct {
				MAC    string `json:"mac"`
				Online bool   `json:"online"`
				Link   string `json:"link"`
				Signal int    `json:"signal"`
			}
			c.Call("devices.list", nil, &devs)
			for _, d := range devs {
				if d.MAC == clientMAC {
					return d.Online && d.Link == apRadio.Iface && d.Signal < 0
				}
			}
			return false
		})
	})

	t.Run("PrefersFiveGHz", func(t *testing.T) {
		if s := getStatus(); len(s.STAs) != 1 || s.STAs[0].Freq != 2412 {
			t.Fatalf("expected the uplink on the hotel's 2.4 GHz channel: %+v", s.STAs)
		}
		// The hotel turns on a second access point for the same network,
		// on 5 GHz.
		hotel5Conf := filepath.Join(dir, "hotel5-hostapd.conf")
		os.WriteFile(hotel5Conf, []byte(fmt.Sprintf(
			"interface=%s\nbridge=%s\ndriver=nl80211\nssid=%s\ncountry_code=US\nhw_mode=a\nchannel=36\nwpa=2\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\nwpa_passphrase=%s\n",
			hotel5If, hotelBridge, hotelSSID, hotelPass)), 0o600)
		start("hotel hostapd (5 GHz)", "hotel5-hostapd.log", "ip", "netns", "exec", nsHotel, "hostapd", hotel5Conf)
		eventually(t, "hotel access point on 5 GHz", 30*time.Second, func() bool {
			b, _ := os.ReadFile(filepath.Join(dir, "hotel5-hostapd.log"))
			return strings.Contains(string(b), "AP-ENABLED")
		})
		// Once the router has seen it (here: a scan from the app) it moves
		// the uplink over.
		eventually(t, "uplink moved to 5 GHz", 120*time.Second, func() bool {
			s := getStatus()
			if len(s.STAs) == 1 && s.STAs[0].State == "connected" && s.STAs[0].Freq == 5180 {
				return s.Internet == "online"
			}
			var res []struct{}
			c.Call("wifi.scan", nil, &res)
			return false
		})
		eventually(t, "client still online", 60*time.Second, func() bool {
			got, err := httpGet(nsClient, "http://"+siteA+"/ip")
			return err == nil && got != "" && got == wanIP()
		})
	})

	t.Run("OneRadioForBoth", func(t *testing.T) {
		if !apRadio.APSTA {
			t.Skip("the simulated radio does not report simultaneous AP and station support")
		}
		// Hand both jobs to the first radio and switch the second off.
		editConfig(func(cfg *config.Config) {
			for i := range cfg.Radios {
				switch cfg.Radios[i].ID {
				case apRadio.ID:
					cfg.Radios[i].Role = config.RoleAPWAN
				case wanRadio.ID:
					cfg.Radios[i].Role = config.RoleOff
				}
			}
		})
		eventually(t, "uplink and access point sharing one radio", 120*time.Second, func() bool {
			s := getStatus()
			if len(s.STAs) != 1 || s.STAs[0].Radio != apRadio.ID || s.STAs[0].State != "connected" {
				return false
			}
			// The uplink goes to the hotel's 5 GHz access point, and the
			// router's own access point has to follow it there.
			return s.STAs[0].Freq == 5180 &&
				len(s.APs) == 1 && s.APs[0].Iface == "ap-"+apRadio.Iface && s.APs[0].State == "up" && s.APs[0].Channel == 36 &&
				s.Internet == "online" && s.Active == apRadio.Iface
		})
		eventually(t, "client back online through the shared radio", 120*time.Second, func() bool {
			got, err := httpGet(nsClient, "http://"+siteA+"/ip")
			return err == nil && got != "" && got == wanIP()
		})
	})
}
