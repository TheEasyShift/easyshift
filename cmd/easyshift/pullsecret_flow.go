package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TheEasyShift/easyshift/config"
	"github.com/TheEasyShift/easyshift/interfaces"
)

const pullSecretConsoleURL = "https://console.redhat.com/openshift/install/pull-secret"

// pullSecretExplanation is the guidance shown whenever no pull secret is
// configured: what it is and the two ways to set one up.
func pullSecretExplanation() string {
	return fmt.Sprintf(`No pull secret configured.

A pull secret is a free credential from your Red Hat account that lets the
installer download OpenShift container images. Two ways to set it up:

  1. Log in to your Red Hat account now (recommended) — you'll get a short
     code to enter at a Red Hat URL from any browser, e.g. your laptop.
  2. Download it yourself from
     %s
     and run: easyshift pull-secret set <file>
`, pullSecretConsoleURL)
}

// stdinIsTTY reports whether stdin is an interactive terminal.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ensurePullSecret guarantees a pull secret exists before create proceeds.
// Interactive terminals get an offer to fetch one via Red Hat SSO;
// non-interactive runs fail immediately with the manual instructions.
func ensurePullSecret(ctx context.Context, cfg *config.Config, fetcher interfaces.PullSecretFetcher, in io.Reader, out io.Writer, isTTY bool) error {
	if config.EnsurePullSecret(cfg.ConfigDir) == nil {
		return nil
	}
	if !isTTY {
		return fmt.Errorf("%s\nthen re-run this command (or run `easyshift pull-secret login` from an interactive terminal)", pullSecretExplanation())
	}
	fmt.Fprint(out, pullSecretExplanation())
	fmt.Fprint(out, "\nLog in and fetch it now? [Y/n] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "" && answer != "y" && answer != "yes" {
		return fmt.Errorf("pull secret not configured: download it from %s and run `easyshift pull-secret set <file>`", pullSecretConsoleURL)
	}
	return fetchAndStorePullSecret(ctx, cfg, fetcher, out)
}

// fetchAndStorePullSecret runs the device-code login, validates the fetched
// secret, and persists it. Every failure path names the manual fallback.
func fetchAndStorePullSecret(ctx context.Context, cfg *config.Config, fetcher interfaces.PullSecretFetcher, out io.Writer) error {
	manualFallback := fmt.Sprintf("fallback: download the pull secret from %s and run `easyshift pull-secret set <file>`", pullSecretConsoleURL)
	prompt, err := fetcher.StartDeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, manualFallback)
	}
	fmt.Fprintf(out, "\n  On any device, open:  %s\n  and enter the code:   %s\n\nWaiting for authorization...\n", prompt.VerificationURI, prompt.UserCode)
	data, err := fetcher.WaitAndFetch(ctx)
	if err != nil {
		return fmt.Errorf("%w\n\n%s", err, manualFallback)
	}
	if err := config.ValidatePullSecretBytes(data); err != nil {
		return fmt.Errorf("pull secret from Red Hat was unusable: %w\n\n%s", err, manualFallback)
	}
	if err := config.WritePullSecret(cfg.ConfigDir, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "Pull secret stored at %s\n", config.PullSecretPath(cfg.ConfigDir))
	return nil
}
