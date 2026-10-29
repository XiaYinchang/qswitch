package buildinfo

import "runtime/debug"

// Version can be set by -ldflags -X; normal builds use Go's embedded VCS info.
var Version string

func init() {
	if Version == "" {
		Version = sourceVersion()
	}
}

func sourceVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}
