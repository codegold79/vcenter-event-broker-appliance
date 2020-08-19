package function

import (
	"context"
	"fmt"
	"net/url"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/types"
)

// vsClient is a client for vSphere.
type vsClient struct {
	govmomi *govmomi.Client
	rest    *rest.Client
	tagMgr  *tags.Manager
}

func newClient(ctx context.Context, u url.URL, insecure bool) (*vsClient, error) {
	var clt vsClient

	gc, err := govmomi.NewClient(ctx, &u, insecure)
	if err != nil {
		return nil, fmt.Errorf("connecting to govmomi api failed: %w", err)
	}
	clt.govmomi = gc

	clt.rest = rest.NewClient(clt.govmomi.Client)
	err = clt.rest.Login(ctx, u.User)
	if err != nil {
		return nil, fmt.Errorf("log in to rest api failed: %w", err)
	}

	err, tm := tags.NewManager(clt.rest)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize tag manager: %w", err)
	}

	clt.tagMgr = tm

	return &clt, nil
}

func (clt *vsClient) logout(ctx context.Context) error {
	err := clt.govmomi.Logout(ctx)
	if err != nil {
		return fmt.Errorf("govmomi api logout failed: %w", err)
	}

	err = clt.rest.Logout(ctx)
	if err != nil {
		return fmt.Errorf("rest api logout failed: %w", err)
	}

	return nil
}
