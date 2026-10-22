module github.com/kivali-ai/kivali

go 1.27

require (
	github.com/Code-Hex/vz/v3 v3.7.1
	github.com/Microsoft/go-winio v0.6.2
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/go-git/go-git/v5 v5.19.3
	github.com/klauspost/compress v1.18.0
	github.com/mdlayher/vsock v1.2.1
	golang.org/x/net v0.57.0
	golang.org/x/sys v0.47.0
	golang.org/x/term v0.45.0
	golang.org/x/text v0.41.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	dario.cat/mergo v1.0.0 // indirect
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	github.com/ProtonMail/go-crypto v1.1.6 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/fatih/structtag v1.2.0 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.2 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/gzuidhof/tygo v0.2.21 // indirect
	github.com/inconshreveable/mousetrap v1.0.0 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/kevinburke/ssh_config v1.2.0 // indirect
	github.com/mdlayher/socket v0.4.1 // indirect
	github.com/pjbgf/sha1cd v0.7.0 // indirect
	github.com/sergi/go-diff v1.3.2-0.20230802210424-5b0b94c5c0d3 // indirect
	github.com/skeema/knownhosts v1.3.1 // indirect
	github.com/spf13/cobra v1.3.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/xanzy/ssh-agent v0.3.3 // indirect
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

tool github.com/gzuidhof/tygo

// Code-Hex/vz v3 closes the vsock descriptor that Virtualization.framework
// owns, so every vsock connection's descriptor is closed twice
// (https://github.com/Code-Hex/vz/issues/268). The fork is v3.7.1 plus one
// commit that wraps a duplicate of the descriptor instead.
replace github.com/Code-Hex/vz/v3 => github.com/kivali-ai/vz/v3 v3.7.2-0.20261022023143-5e450bbf9bf9
