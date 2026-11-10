// porter-release prepares the files of a release that routers update
// themselves from. The image build runs it; it is not part of the image.
//
//	porter-release describe <version> <unix-time> <boot.vfat> <root.ext4> <out-dir>
//	    compresses the two partition images next to themselves and writes
//	    update.json, the manifest, into out-dir. With a signing key in the
//	    environment variable PORTER_UPDATE_KEY it also writes
//	    update.json.sig. With PORTER_UPDATE_PUB naming a public key file,
//	    the signature is checked against it, so that a release signed with
//	    the wrong key is never published.
//
//	porter-release keygen
//	    prints a new signing key (keep it secret) and the public key that
//	    goes into the repository as image/keys/update.pub.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ryanzhou/porter/daemon/internal/update"
)

func fail(err error) {
	fmt.Fprintln(os.Stderr, "porter-release:", err)
	os.Exit(1)
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "keygen":
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			fail(err)
		}
		fmt.Println("Signing key, for the PORTER_UPDATE_KEY secret. Keep it out of the repository:")
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println()
		fmt.Println("Public key, for image/keys/update.pub:")
		fmt.Println(base64.StdEncoding.EncodeToString(pub))

	case len(args) == 6 && args[0] == "describe":
		built, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			fail(fmt.Errorf("build time: %w", err))
		}
		m, err := update.Describe(args[1], built, args[3], args[4])
		if err != nil {
			fail(err)
		}
		raw := m.Encode()
		out := filepath.Join(args[5], "update.json")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("%s: %s, download %d MB\n", out, m.Version, m.Download()>>20)
		if key := os.Getenv("PORTER_UPDATE_KEY"); key != "" {
			sig, err := update.Sign(raw, key)
			if err != nil {
				fail(err)
			}
			if pub := os.Getenv("PORTER_UPDATE_PUB"); pub != "" {
				key, err := os.ReadFile(pub)
				if err != nil {
					fail(err)
				}
				if update.Verify(raw, []byte(sig), string(key)) != nil {
					fail(fmt.Errorf("PORTER_UPDATE_KEY is not the key that belongs to %s; routers would refuse this release", pub))
				}
			}
			if err := os.WriteFile(out+".sig", []byte(sig+"\n"), 0o644); err != nil {
				fail(err)
			}
			fmt.Println(out + ".sig: signed")
		} else {
			fmt.Println("not signed: PORTER_UPDATE_KEY is not set")
		}

	default:
		fmt.Fprintln(os.Stderr, "usage: porter-release describe <version> <unix-time> <boot.vfat> <root.ext4> <out-dir> | keygen")
		os.Exit(2)
	}
}
