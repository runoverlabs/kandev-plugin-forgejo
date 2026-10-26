package sourcecontrol

// ConnectionInfo is the configured instance as far as the plugin config says,
// without asking the instance anything.
type ConnectionInfo struct {
	// URL is the normalized instance URL: the connection scope every
	// association and repository carries.
	URL  string
	Host string
}

// ConnectionProbe is what a live check learned from the instance.
type ConnectionProbe struct {
	// Account is the login the token acts as.
	Account string
	// Version and Flavor are empty when the instance does not report them.
	Version string
	Flavor  string
}
