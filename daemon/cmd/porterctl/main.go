// porterctl talks to a running porterd over its local control socket. It
// speaks the same protocol as the phone app and is meant for debugging
// on the router itself.
//
//	porterctl status
//	porterctl config
//	porterctl call <method> [json-params]
//	porterctl watch
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ryanzhou/porter/daemon/internal/client"
)

func main() {
	runDir := flag.String("run", "/run/porter", "porterd runtime directory")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: porterctl [-run dir] status | config | watch | call <method> [json]")
		os.Exit(2)
	}
	c, err := client.DialUnix(filepath.Join(*runDir, "control.sock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "porterctl:", err)
		os.Exit(1)
	}
	defer c.Close()

	method := ""
	var params any
	switch args[0] {
	case "status":
		method = "status.get"
	case "config":
		method = "config.get"
	case "watch":
		enc := json.NewEncoder(os.Stdout)
		for ev := range c.Events {
			enc.Encode(ev)
		}
		return
	case "call":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "porterctl: call needs a method")
			os.Exit(2)
		}
		method = args[1]
		if len(args) > 2 {
			params = json.RawMessage(args[2])
		}
	default:
		fmt.Fprintln(os.Stderr, "porterctl: unknown command", args[0])
		os.Exit(2)
	}
	var out json.RawMessage
	if err := c.Call(method, params, &out); err != nil {
		fmt.Fprintln(os.Stderr, "porterctl:", err)
		os.Exit(1)
	}
	var pretty any
	json.Unmarshal(out, &pretty)
	b, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(b))
}
