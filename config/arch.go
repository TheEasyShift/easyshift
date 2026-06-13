package config

// PayloadArch maps a Go arch (runtime.GOARCH) to the OpenShift mirror /
// RHCOS stream-json architecture key. easyshift runs a native-arch guest, so
// the cluster payload arch equals the host CPU arch.
func PayloadArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}
