package agent

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/pkg/errors"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/internal/wireguard"
)

/// Establish starts the daemon if necessary and returns a client
func Establish(ctx context.Context, apiClient *api.Client) (*Client, error) {
	if err := wireguard.PruneInvalidPeers(apiClient); err != nil {
		return nil, err
	}

	c, err := DefaultClient(apiClient)
	if err == nil {
		_, err := c.Ping(ctx)
		if err == nil {
			return c, nil
		}
	}

	return StartDaemon(ctx, apiClient, os.Args[0])
}

func NewClient(path string, apiClient *api.Client) (*Client, error) {
	provider, err := newClientProvider(path, apiClient)
	if err != nil {
		return nil, err
	}

	return &Client{provider: provider}, nil
}

func DefaultClient(apiClient *api.Client) (*Client, error) {
	path := fmt.Sprintf("%s/.fly/fly-agent.sock", os.Getenv("HOME"))
	return NewClient(path, apiClient)
}

type Client struct {
	provider clientProvider
}

func (c *Client) Kill(ctx context.Context) error {
	if err := c.provider.Kill(ctx); err != nil {
		return errors.Wrap(err, "kill failed")
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) (int, error) {
	n, err := c.provider.Ping(ctx)
	if err != nil {
		return n, errors.Wrap(err, "ping failed")
	}
	return n, nil
}

func (c *Client) Establish(ctx context.Context, slug string) error {
	if err := c.provider.Establish(ctx, slug); err != nil {
		return errors.Wrap(err, "establish failed")
	}
	return nil
}

func (c *Client) WaitForTunnel(ctx context.Context, o *api.Organization) error {
	for {
		err := c.Probe(ctx, o)
		switch {
		case err == nil:
			return nil
		case IsTunnelError(err):
			continue
		default:
			return err
		}
	}
}

func (c *Client) WaitForHost(ctx context.Context, o *api.Organization, host string) error {
	for {
		_, err := c.Resolve(ctx, o, host)
		switch {
		case err == nil:
			return nil
		case IsHostNotFoundError(err), IsTunnelError(err):
			continue
		default:
			return err
		}
	}
}

func (c *Client) Probe(ctx context.Context, o *api.Organization) error {
	if err := c.provider.Probe(ctx, o); err != nil {
		err = mapResolveError(err, o.Slug, "")
		return errors.Wrap(err, "probe failed")
	}
	return nil
}

func (c *Client) Resolve(ctx context.Context, o *api.Organization, host string) (string, error) {
	addr, err := c.provider.Resolve(ctx, o, host)
	if err != nil {
		err = mapResolveError(err, o.Slug, host)
		return "", errors.Wrap(err, "resolve failed")
	}
	return addr, nil
}

func (c *Client) Instances(ctx context.Context, o *api.Organization, app string) (*Instances, error) {
	instances, err := c.provider.Instances(ctx, o, app)
	if err != nil {
		return nil, errors.Wrap(err, "list instances failed")
	}
	return instances, nil
}

func (c *Client) Dialer(ctx context.Context, o *api.Organization) (Dialer, error) {
	dialer, err := c.provider.Dialer(ctx, o)
	if err != nil {
		err = mapResolveError(err, o.Slug, "")
		return nil, errors.Wrap(err, "error fetching dialer")
	}
	return dialer, nil
}

// clientProvider is an interface for client functions backed by either the agent or in-process on Windows
type clientProvider interface {
	Dialer(ctx context.Context, o *api.Organization) (Dialer, error)
	Establish(ctx context.Context, slug string) error
	Instances(ctx context.Context, o *api.Organization, app string) (*Instances, error)
	Kill(ctx context.Context) error
	Ping(ctx context.Context) (int, error)
	Probe(ctx context.Context, o *api.Organization) error
	Resolve(ctx context.Context, o *api.Organization, name string) (string, error)
}

type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}
