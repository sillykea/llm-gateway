package cache

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Redis is a small Redis client that speaks just enough of the RESP protocol
// for GET, SET with expiry, PING and AUTH. It keeps a pool of connections so
// concurrent requests don't queue behind each other, and can use TLS and a
// password (as managed Redis services such as ElastiCache require).
//
// A cache failure never fails a request: errors are treated as a miss and
// the request goes to the model as normal.
type Redis struct {
	addr     string
	timeout  time.Duration
	pool     chan *redisConn
	tls      *tls.Config
	password string
}

// RedisOptions configures the client. Zero values mean plain TCP, no auth.
type RedisOptions struct {
	PoolSize int
	// TLS enables encryption in transit. Use &tls.Config{} for a server whose
	// certificate is signed by a public CA (ElastiCache is).
	TLS *tls.Config
	// Password is sent with AUTH on every new connection.
	Password string
}

type redisConn struct {
	net.Conn
	rd *bufio.Reader
}

// NewRedis creates a client. Connections are opened lazily, on first use.
func NewRedis(addr string, opts RedisOptions) *Redis {
	if opts.PoolSize <= 0 {
		opts.PoolSize = 10
	}
	return &Redis{
		addr:     addr,
		timeout:  2 * time.Second,
		pool:     make(chan *redisConn, opts.PoolSize),
		tls:      opts.TLS,
		password: opts.Password,
	}
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool) {
	v, err := r.do(ctx, "GET", key)
	if err != nil || v == nil {
		return nil, false
	}
	return v, true
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	_, _ = r.do(ctx, "SET", key, string(value), "EX", strconv.Itoa(int(ttl.Seconds())))
}

// Ping is used by the readiness probe.
func (r *Redis) Ping(ctx context.Context) error {
	_, err := r.do(ctx, "PING")
	return err
}

// get takes an idle connection from the pool, or dials a new one.
func (r *Redis) get(ctx context.Context) (*redisConn, error) {
	select {
	case c := <-r.pool:
		return c, nil
	default:
	}
	d := net.Dialer{Timeout: r.timeout}
	var c net.Conn
	var err error
	if r.tls != nil {
		td := tls.Dialer{NetDialer: &d, Config: r.tls}
		c, err = td.DialContext(ctx, "tcp", r.addr)
	} else {
		c, err = d.DialContext(ctx, "tcp", r.addr)
	}
	if err != nil {
		return nil, err
	}
	rc := &redisConn{Conn: c, rd: bufio.NewReader(c)}

	if r.password != "" {
		_ = c.SetDeadline(time.Now().Add(r.timeout))
		if err := writeCommand(c, "AUTH", r.password); err != nil {
			c.Close()
			return nil, err
		}
		if _, err := readReply(rc.rd); err != nil {
			c.Close()
			return nil, fmt.Errorf("redis auth: %w", err)
		}
	}
	return rc, nil
}

func writeCommand(w io.Writer, args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// put returns a healthy connection to the pool, or closes it if the pool is full.
func (r *Redis) put(c *redisConn) {
	select {
	case r.pool <- c:
	default:
		c.Close()
	}
}

func (r *Redis) do(ctx context.Context, args ...string) ([]byte, error) {
	c, err := r.get(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(r.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)

	// Commands are sent as a RESP array of bulk strings.
	if err := writeCommand(c, args...); err != nil {
		c.Close()
		return nil, err
	}

	v, err := readReply(c.rd)
	var redisErr redisError
	if err != nil && !errors.As(err, &redisErr) {
		c.Close() // the connection is in an unknown state, don't reuse it
		return nil, err
	}
	r.put(c)
	return v, err
}

// redisError is an error reply from the server ("-ERR ..."). The connection
// is still fine after one of these.
type redisError string

func (e redisError) Error() string { return "redis: " + string(e) }

func readReply(rd *bufio.Reader) ([]byte, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, errors.New("redis: short reply")
	}
	body := line[1 : len(line)-2]
	switch line[0] {
	case '+', ':': // simple string or integer
		return []byte(body), nil
	case '-':
		return nil, redisError(body)
	case '$': // bulk string
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil // key not found
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(rd, buf); err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line[0])
}
