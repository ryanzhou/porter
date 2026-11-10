//go:build !linux

package update

import "os"

// dropCache has nothing to do away from the router. The package still
// builds on other systems for porter-release, which describes and signs
// a release and never writes a partition.
func dropCache(*os.File) {}
