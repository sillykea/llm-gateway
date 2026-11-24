package cache

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// selfSignedTLS returns a server config for 127.0.0.1 and a client config
// that trusts it, so the test exercises real certificate verification.
func selfSignedTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-redis"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	client = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return server, client
}

// secureRedis behaves like ElastiCache with in-transit encryption and an
// auth token: TLS only, and every command fails until AUTH succeeds.
func secureRedis(t *testing.T, cfg *tls.Config, password string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	store := map[string]string{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				authed := false
				for {
					args, err := readCommand(rd)
					if err != nil {
						return
					}
					switch cmd := strings.ToUpper(args[0]); {
					case cmd == "AUTH":
						if len(args) == 2 && args[1] == password {
							authed = true
							fmt.Fprint(c, "+OK\r\n")
						} else {
							fmt.Fprint(c, "-WRONGPASS invalid password\r\n")
						}
					case !authed:
						fmt.Fprint(c, "-NOAUTH Authentication required.\r\n")
					case cmd == "PING":
						fmt.Fprint(c, "+PONG\r\n")
					case cmd == "SET":
						mu.Lock()
						store[args[1]] = args[2]
						mu.Unlock()
						fmt.Fprint(c, "+OK\r\n")
					case cmd == "GET":
						mu.Lock()
						v, ok := store[args[1]]
						mu.Unlock()
						if ok {
							fmt.Fprintf(c, "$%d\r\n%s\r\n", len(v), v)
						} else {
							fmt.Fprint(c, "$-1\r\n")
						}
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestRedisTLSAndAuth(t *testing.T) {
	ctx := context.Background()
	serverCfg, clientCfg := selfSignedTLS(t)
	addr := secureRedis(t, serverCfg, "s3cret-token")

	r := NewRedis(addr, RedisOptions{TLS: clientCfg, Password: "s3cret-token"})
	if err := r.Ping(ctx); err != nil {
		t.Fatalf("ping over TLS with auth: %v", err)
	}
	r.Set(ctx, "k", []byte("v"), time.Minute)
	if v, ok := r.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestRedisWrongPassword(t *testing.T) {
	serverCfg, clientCfg := selfSignedTLS(t)
	addr := secureRedis(t, serverCfg, "right")
	r := NewRedis(addr, RedisOptions{TLS: clientCfg, Password: "wrong"})
	if err := r.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expected an auth error, got %v", err)
	}
}

func TestRedisRejectsUntrustedCert(t *testing.T) {
	serverCfg, _ := selfSignedTLS(t)
	addr := secureRedis(t, serverCfg, "pw")
	// System roots don't trust the self-signed cert: the client must refuse.
	r := NewRedis(addr, RedisOptions{TLS: &tls.Config{}, Password: "pw"})
	if err := r.Ping(context.Background()); err == nil {
		t.Fatal("connected to a server with an untrusted certificate")
	}
}

func TestPlainClientCannotTalkToTLSServer(t *testing.T) {
	serverCfg, _ := selfSignedTLS(t)
	addr := secureRedis(t, serverCfg, "pw")
	r := NewRedis(addr, RedisOptions{})
	if _, ok := r.Get(context.Background(), "k"); ok {
		t.Fatal("plain client should not get a hit from a TLS-only server")
	}
}
