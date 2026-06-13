package openshift

import "fmt"

// HostClientPlatform returns the openshift-install/oc tarball platform suffix
// for the host easyshift runs on (where those binaries execute).
func HostClientPlatform(goos, goarch string) string {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "mac-arm64"
	case goos == "darwin":
		return "mac"
	default:
		return "linux"
	}
}

// InstallClientTarball / OCClientTarball name the client tarballs for a host
// platform suffix (from HostClientPlatform).
func InstallClientTarball(platform string) string {
	return fmt.Sprintf("openshift-install-%s.tar.gz", platform)
}

func OCClientTarball(platform string) string {
	return fmt.Sprintf("openshift-client-%s.tar.gz", platform)
}

// OCPMirrorURLForArch is the mirror root for a payload architecture.
func OCPMirrorURLForArch(arch string) string {
	return fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/%s", arch)
}
