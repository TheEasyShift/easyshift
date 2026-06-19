package interfaces

// Deps is the wiring DTO: a bag of every side-effect implementation. app
// (production) and tests (fakes) populate it, then app.buildStages maps each
// field into the constructor of the stage that needs it. Stages do NOT
// receive Deps — they get exactly the interfaces they depend on. This struct
// exists only so construction has one well-known shape.
type Deps struct {
	Cmd        CommandRunner
	Download   Downloader
	VM         VMManager
	Net        NetworkProvisioner
	Installer  Installer
	ImageBaker ImageBaker
	Files      FileServer
	CSR        CSRApprover
	Hostname   HostnameInjector
	Host       HostInspector
	DNS        DNSResolver
	DNSManager DNSManager
	// PullSecret fetches the pull secret from the user's Red Hat account
	// (device-code login). Consumed only by cmd — never by stages.
	PullSecret PullSecretFetcher
	// TrustStore installs the local CA into host trust stores. Consumed only
	// by cmd (`easyshift trust`) — never by stages.
	TrustStore TrustStoreInstaller
	// NewCertIssuer constructs a CertIssuer for per-cluster ACME settings
	// (email + staging). Function-shaped because that state isn't known until
	// a specific cluster is being created.
	NewCertIssuer func(opts CertIssuerOpts) (CertIssuer, error)
	// NewLocalCertIssuer constructs a CertIssuer backed by the host-local
	// easyshift CA rooted at caDir (config.LocalCADir). Used for every
	// cluster without TLSEmail. Function-shaped to mirror NewCertIssuer.
	NewLocalCertIssuer func(caDir string) (CertIssuer, error)
}
