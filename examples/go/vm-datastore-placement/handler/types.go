package function

import (
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vim25/types"
)

// vcConfig represents the toml vcconfig file.
type vcConfig struct {
	VCenter struct {
		Server   string
		User     string
		Password string
		Insecure bool
	}
}

// cloudEvent contains event data.
type cloudEvent struct {
	Data types.AlarmStatusChangedEvent
}

type vsClient struct {
	govmomi *govmomi.Client
}
