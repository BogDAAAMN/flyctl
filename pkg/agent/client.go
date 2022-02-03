package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azazeal/pause"
	"github.com/blang/semver"
	"golang.org/x/sync/errgroup"

	"github.com/superfly/flyctl/pkg/agent/internal/proto"
	"github.com/superfly/flyctl/pkg/wg"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/internal/buildinfo"
	"github.com/superfly/flyctl/internal/logger"
	"github.com/superfly/flyctl/internal/wireguard"
)

// Establish starts the daemon, if necessary, and returns a client to it.
func Establish(ctx context.Context, apiClient *api.Client) (*Client, error) {
	if err := wireguard.PruneInvalidPeers(ctx, apiClient); err != nil {
		return nil, err
	}

	c := newClient("unix", PathToSocket())

	res, err := c.Ping(ctx)
	if err != nil {
		return StartDaemon(ctx)
	}

	if buildinfo.Version().EQ(res.Version) {
		return c, nil
	}

	// TOOD: log this instead
	msg := fmt.Sprintf("The running flyctl background agent (v%s) is older than the current flyctl (v%s).", buildinfo.Version(), res.Version)

	logger := logger.MaybeFromContext(ctx)
	if logger != nil {
		logger.Warn(msg)
	} else {
		fmt.Fprintln(os.Stderr, msg)
	}

	if !res.Background {
		return c, nil
	}

	const stopMessage = "The out-of-date agent will be shut down along with existing wireguard connections. The new agent will start automatically as needed."
	if logger != nil {
		logger.Warn(stopMessage)
	} else {
		fmt.Fprintln(os.Stderr, stopMessage)
	}

	if err := c.Kill(ctx); err != nil {
		err = fmt.Errorf("failed stopping agent: %w", err)

		if logger != nil {
			logger.Error(err)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}

		return nil, err
	}

	// this is gross, but we need to wait for the agent to exit
	pause.For(ctx, time.Second)

	return StartDaemon(ctx)
}

func newClient(network, addr string) *Client {
	return &Client{
		network: network,
		address: addr,
	}
}

func Dial(ctx context.Context, network, addr string) (client *Client, err error) {
	client = newClient(network, addr)

	if _, err = client.Ping(ctx); err != nil {
		client = nil
	}

	return
}

func DefaultClient(ctx context.Context) (*Client, error) {
	return Dial(ctx, "unix", PathToSocket())
}

const (
	timeout = 2 * time.Second
	cycle   = time.Second / 20
)

type Client struct {
	network string
	address string
	dialer  net.Dialer
}

func (c *Client) dial() (conn net.Conn, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return c.dialContext(ctx)
}

func (c *Client) dialContext(ctx context.Context) (conn net.Conn, err error) {
	return c.dialer.DialContext(ctx, c.network, c.address)
}

var errDone = errors.New("done")

func (c *Client) do(parent context.Context, fn func(net.Conn) error) (err error) {
	var conn net.Conn
	if conn, err = c.dialContext(parent); err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(parent)

	eg.Go(func() (err error) {
		<-ctx.Done()

		if err = conn.Close(); err == nil {
			err = net.ErrClosed
		}

		return
	})

	eg.Go(func() (err error) {
		if err = fn(conn); err == nil {
			err = errDone
		}

		return
	})

	if err = eg.Wait(); errors.Is(err, errDone) {
		err = nil
	}

	return
}

func (c *Client) Kill(ctx context.Context) error {
	return c.do(ctx, func(conn net.Conn) error {
		return proto.Write(conn, "kill")
	})
}

type PingResponse struct {
	PID        int
	Version    semver.Version
	Background bool
}

type errInvalidResponse []byte

func (err errInvalidResponse) Error() string {
	return fmt.Sprintf("invalid server response: %q", string(err))
}

func (c *Client) Ping(ctx context.Context) (res PingResponse, err error) {
	err = c.do(ctx, func(conn net.Conn) (err error) {
		if err = proto.Write(conn, "ping"); err != nil {
			return
		}

		var data []byte
		if data, err = proto.Read(conn); err != nil {
			return
		}

		if isOK(data) {
			err = unmarshal(&res, data)
		} else {
			err = errInvalidResponse(data)
		}

		return
	})

	return
}

const okPrefix = "ok "

func isOK(data []byte) bool {
	return isPrefixedWith(data, okPrefix)
}

func extractOK(data []byte) []byte {
	return data[len(okPrefix):]
}

const errorPrefix = "err "

func isError(data []byte) bool {
	return isPrefixedWith(data, errorPrefix)
}

func extractError(data []byte) error {
	msg := data[len(errorPrefix):]

	return errors.New(string(msg))
}

func isPrefixedWith(data []byte, prefix string) bool {
	return strings.HasPrefix(string(data), prefix)
}

type EstablishResponse struct {
	WireGuardState *wg.WireGuardState
	TunnelConfig   *wg.Config
}

func (c *Client) Establish(ctx context.Context, slug string) (res *EstablishResponse, err error) {
	err = c.do(ctx, func(conn net.Conn) (err error) {
		if err = proto.Write(conn, "establish", slug); err != nil {
			return
		}

		// this goes out to the API; don't time it out aggressively
		var data []byte
		if data, err = proto.Read(conn); err != nil {
			return
		}

		switch {
		default:
			err = errInvalidResponse(data)
		case isOK(data):
			res = &EstablishResponse{}
			if err = unmarshal(res, data); err != nil {
				res = nil
			}
		case isError(data):
			err = extractError(data)
		}

		return
	})

	return
}

func (c *Client) Probe(ctx context.Context, slug string) error {
	return c.do(ctx, func(conn net.Conn) (err error) {
		if err = proto.Write(conn, "probe", slug); err != nil {
			return
		}

		var data []byte
		if data, err = proto.Read(conn); err != nil {
			return
		}

		switch {
		default:
			err = errInvalidResponse(data)
		case string(data) == "ok":
			return // up and running
		case isError(data):
			err = extractError(data)
		}

		return
	})
}

func (c *Client) Resolve(ctx context.Context, slug, host string) (addr string, err error) {
	err = c.do(ctx, func(conn net.Conn) (err error) {
		if err = proto.Write(conn, "resolve", slug, host); err != nil {
			return
		}

		var data []byte
		if data, err = proto.Read(conn); err != nil {
			return
		}

		switch {
		default:
			err = errInvalidResponse(data)
		case string(data) == "ok":
			err = ErrNoSuchHost
		case isOK(data):
			addr = string(extractOK(data))
		case isError(data):
			err = extractError(data)
		}

		return
	})

	return
}

// WaitForTunnel waits for a tunnel to the given org slug to become available
// in the next four minutes.
func (c *Client) WaitForTunnel(parent context.Context, slug string) (err error) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()

	for {
		if err = c.Probe(ctx, slug); !errors.Is(err, ErrTunnelUnavailable) {
			break
		}

		pause.For(ctx, cycle)
	}

	if parent.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		err = ErrTunnelUnavailable
	}

	return
}

// WaitForHost waits for a tunnel to the given host of the given org slug to
// become available in the next four minutes.
func (c *Client) WaitForHost(parent context.Context, slug, host string) (err error) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	defer cancel()

	if err = c.WaitForTunnel(ctx, slug); err != nil {
		return
	}

	for {
		if _, err = c.Resolve(ctx, slug, host); !errors.Is(err, ErrNoSuchHost) {
			break
		}

		pause.For(ctx, cycle)
	}

	if parent.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		err = ErrNoSuchHost
	}

	return
}

func (c *Client) Instances(ctx context.Context, org *api.Organization, app string) (instances Instances, err error) {
	err = c.do(ctx, func(conn net.Conn) (err error) {
		if err = proto.Write(conn, "instances", org.Slug, app); err != nil {
			return
		}

		// this goes out to the network; don't time it out aggressively
		var data []byte
		if data, err = proto.Read(conn); err != nil {
			return
		}

		switch {
		default:
			err = errInvalidResponse(data)
		case isOK(data):
			err = unmarshal(&instances, data)
		case isError(data):
			err = extractError(data)
		}

		return
	})

	return
}

func unmarshal(dst interface{}, data []byte) (err error) {
	src := bytes.NewReader(extractOK(data))

	dec := json.NewDecoder(src)
	if err = dec.Decode(dst); err != nil {
		err = fmt.Errorf("failed decoding response: %w", err)
	}

	return
}

func (c *Client) DecafDialer(ctx context.Context, slug string) (d Dialer, err error) {
	return &dialer{
		slug:   slug,
		client: c,
	}, nil
}

func (c *Client) Dialer(ctx context.Context, slug string) (d Dialer, err error) {
	var er *EstablishResponse
	if er, err = c.Establish(ctx, slug); err == nil {
		d = &dialer{
			slug:   slug,
			client: c,
			state:  er.WireGuardState,
			config: er.TunnelConfig,
		}
	}

	return
}

// TODO: refactor to struct
type Dialer interface {
	State() *wg.WireGuardState
	Config() *wg.Config
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type dialer struct {
	slug    string
	timeout time.Duration

	state  *wg.WireGuardState
	config *wg.Config

	client *Client
}

func (d *dialer) State() *wg.WireGuardState {
	return d.state
}

func (d *dialer) Config() *wg.Config {
	return d.config
}

func (d *dialer) DialContext(ctx context.Context, network, addr string) (conn net.Conn, err error) {
	if conn, err = d.client.dialContext(ctx); err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	timeout := strconv.FormatInt(int64(d.timeout), 10)
	if err = proto.Write(conn, "connect", d.slug, addr, timeout); err != nil {
		return
	}

	var data []byte
	if data, err = proto.Read(conn); err != nil {
		return
	}

	switch {
	default:
		err = errInvalidResponse(data)
	case string(data) == "ok":
		break
	case isError(data):
		err = extractError(data)
	}

	return
}

type Resolver struct {
	c       net.Conn
	err     error
	timeout time.Duration
	nsip    string
	mu      sync.Mutex // probably overkill, whatever
}

func (c *Client) Resolver(ctx context.Context, slug string) (r *Resolver, err error) {
	if _, err = c.Establish(ctx, slug); err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}

	conn, err := c.dialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}

	if err = proto.Write(conn, "resolver", slug); err != nil {
		conn.Close()
		return nil, fmt.Errorf("resolver: %w", err)
	}

	var lenbuf [4]byte
	if _, err = io.ReadFull(conn, lenbuf[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("resolver: read dns ip length: %w", err)
	}

	dnsip := make([]byte, int(binary.BigEndian.Uint32(lenbuf[:])))
	if _, err = io.ReadFull(conn, dnsip); err != nil {
		conn.Close()
		return nil, fmt.Errorf("resolver: read dns ip: %w", err)
	}

	reps := strings.SplitN(string(dnsip), " ", 2)
	if len(reps) != 2 || reps[0] != "ok" {
		conn.Close()
		return nil, fmt.Errorf("resolver: parse dns response: bad response")
	}

	return &Resolver{c: conn, nsip: reps[1]}, nil
}

func (r *Resolver) NSAddr() string {
	return r.nsip
}

func (r *Resolver) SetTimeout(t time.Duration) {
	r.timeout = t
}

func (r *Resolver) LookupHost(ctx context.Context, name string) ([]string, error) {
	result, err := r.lookup(ctx, "host", name)
	if err != nil {
		return nil, err
	}

	return strings.Split(result, ","), nil
}

func (r *Resolver) LookupTXT(ctx context.Context, name string) ([]string /* but never really an array */, error) {
	result, err := r.lookup(ctx, "txt", name)
	if err != nil {
		return nil, err
	}

	return []string{result}, nil
}

func (r *Resolver) lookup(ctx context.Context, kind, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var lenbuf [4]byte
	req := fmt.Sprintf("%s %s", kind, name)

	binary.BigEndian.PutUint32(lenbuf[:], uint32(len(req)))

	if _, err := r.c.Write(lenbuf[:]); err != nil {
		r.err = fmt.Errorf("lookup: write req length: %w", err)
		return "", r.err
	}

	if _, err := r.c.Write([]byte(req)); err != nil {
		r.err = fmt.Errorf("lookup: write req: %w", err)
		return "", r.err
	}

	if r.timeout != 0 {
		r.c.SetDeadline(time.Now().Add(r.timeout))
	}

	zt := time.Time{}
	defer r.c.SetDeadline(zt)

	// this first read captures the time the DNS request on the
	// agent takes; subsequent reads should be fast

	if _, err := io.ReadFull(r.c, lenbuf[:]); err != nil {
		r.err = fmt.Errorf("lookup: read reply length: %w", err)
		return "", r.err
	}

	r.c.SetDeadline(zt)

	reply := make([]byte, int(binary.BigEndian.Uint32(lenbuf[:])))

	if _, err := io.ReadFull(r.c, reply); err != nil {
		r.err = fmt.Errorf("lookup: read reply: %w", err)
		return "", r.err
	}

	reps := strings.SplitN(string(reply), " ", 2)
	if len(reps) != 2 {
		r.err = fmt.Errorf("lookup: parse reply: malformed")
		return "", r.err
	}

	switch reps[0] {
	case "ok":
		return reps[1], nil
	case "err":
		fallthrough
	default:
		return "", fmt.Errorf("%s", reps[1])
	}
}

func (r *Resolver) Close() error {
	return r.c.Close()
}

// Err returns any non-recoverable error seen on this Resolver connection;
// lookups on a Resolver will not function if Err returns
// non-nil.
func (r *Resolver) Err() error {
	return r.err
}

// Pinger wraps a connection to the flyctl agent over which ICMP
// requests and replies are written. There's a simple protocol
// for encapsulating requests and responses; drive it with the Pinger
// member functions. Pinger implements most of net.PacketConn but is
// not really intended as such.
type Pinger struct {
	c   net.Conn
	err error
}

// Pinger creates a Pinger struct. It does this by first ensuring
// a WireGuard session exists for the specified org, and then
// opening an additional connection to the agent, which is upgraded
// to a Pinger connection by sending the "ping6" command. Call "Close"
// on a Pinger when you're done pinging things.
func (c *Client) Pinger(ctx context.Context, slug string) (p *Pinger, err error) {
	if _, err = c.Establish(ctx, slug); err != nil {
		return nil, fmt.Errorf("pinger: %w", err)
	}

	conn, err := c.dialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinger: %w", err)
	}

	if err = proto.Write(conn, "ping6", slug); err != nil {
		return nil, fmt.Errorf("pinger: %w", err)
	}

	return &Pinger{c: conn}, nil
}

func (p *Pinger) SetReadDeadline(t time.Time) error {
	return p.c.SetReadDeadline(t)
}

func (p *Pinger) Close() error {
	return p.c.Close()
}

// Err returns any non-recoverable error seen on this Pinger connection;
// WriteTo and ReadFrom on a Pinger will not function if Err returns
// non-nil.
func (p *Pinger) Err() error {
	return p.err
}

// WriteTo writes an ICMP message, including headers, to the specified
// address. `addr` should always be an IPv6 net.IPAddr beginning with
// `fdaa` --- you cannot ping random hosts on the Internet with this
// interface. See golang/x/net/icmp for message construction details;
// this interface uses gVisor netstack, which is fussy about ICMP,
// and will only allow icmp.Echo messages with a code of 0.
//
// Pinger runs a trivial protocol to encapsulate ICMP messages over
// agent connections: each message is a 16-byte IPv6 address, followed
// by an NBO u16 length, followed by the ICMP message bytes, which
// again must begin with an ICMP header. Checksums are performed by
// netstack; don't bother with them.
func (p *Pinger) WriteTo(buf []byte, addr net.Addr) (int64, error) {
	if p.err != nil {
		return 0, p.err
	}

	if len(buf) >= 1500 {
		return 0, fmt.Errorf("icmp write: too large (>=1500 bytes)")
	}

	var v6addr net.IP

	ipaddr, ok := addr.(*net.IPAddr)
	if ok {
		v6addr = ipaddr.IP.To16()
	}

	if !ok || v6addr == nil {
		return 0, fmt.Errorf("icmp write: bad address type")
	}

	_, err := p.c.Write([]byte(v6addr))
	if err != nil {
		p.err = fmt.Errorf("icmp write: address: %w", err)
		return 0, p.err
	}

	lbuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lbuf, uint16(len(buf)))

	_, err = p.c.Write([]byte(lbuf))
	if err != nil {
		p.err = fmt.Errorf("icmp write: length: %w", err)
		return 0, p.err
	}

	_, err = p.c.Write(buf)
	if err != nil {
		p.err = fmt.Errorf("icmp write: payload: %w", err)
		return 0, p.err
	}

	return int64(len(buf)), nil
}

// ReadFrom reads an ICMP message from a Pinger, using the same
// protocol as WriteTo. Call `SetReadDeadline` to poll this
// interface while watching channels or whatever.
func (p *Pinger) ReadFrom(buf []byte) (int64, net.Addr, error) {
	if p.err != nil {
		return 0, nil, p.err
	}

	lbuf := make([]byte, 2)
	v6buf := make([]byte, 16)

	_, err := io.ReadFull(p.c, v6buf)
	if err != nil {
		// common case: read deadline set, this is just
		// a timeout, we don't want to close the pinger

		if !errors.Is(err, os.ErrDeadlineExceeded) {
			p.err = fmt.Errorf("icmp read: addr: %w", err)
			return 0, nil, p.err
		}

		return 0, nil, err
	}

	_, err = io.ReadFull(p.c, lbuf)
	if err != nil {
		p.err = fmt.Errorf("icmp read: length: %w", err)
		return 0, nil, p.err
	}

	paylen := binary.BigEndian.Uint16(lbuf)
	inbuf := make([]byte, paylen)

	_, err = io.ReadFull(p.c, inbuf)
	if err != nil {
		p.err = fmt.Errorf("icmp read: payload: %w", err)
		return 0, nil, p.err
	}

	// burning a copy just so i don't have to think about what
	// happens if you try to read 1 byte of a 1000-byte ping

	copy(buf, inbuf)

	return int64(paylen), &net.IPAddr{IP: net.IP(v6buf)}, nil
}
