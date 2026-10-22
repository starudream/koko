package handler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/jumpserver-dev/sdk-go/model"
	"github.com/jumpserver-dev/sdk-go/service"
	gossh "golang.org/x/crypto/ssh"

	"github.com/jumpserver/koko/pkg/auth"
	"github.com/jumpserver/koko/pkg/config"
	"github.com/jumpserver/koko/pkg/session"
)

func genericTestConfig(t *testing.T) {
	t.Helper()
	previous := config.GlobalConfig
	config.GlobalConfig = &config.Config{
		EnableGenericTCPForward: true, SSHTimeout: 2,
	}
	t.Cleanup(func() { config.GlobalConfig = previous })
}

func genericTestToken() model.ConnectToken {
	return model.ConnectToken{
		Id: "test-token", User: model.User{ID: "test-user"}, Protocol: model.ProtocolSSH,
		Asset:   model.Asset{ID: "test-asset", Address: "127.0.0.1", Protocols: []model.Protocol{{Name: "ssh", Port: 22}}},
		Account: model.Account{BaseAccount: model.BaseAccount{ID: "test-account", Username: "root", Secret: "test-only"}},
		Actions: model.Actions{{Value: model.ActionConnect}}, ExpireAt: model.ExpireInfo(time.Now().Add(time.Hour).Unix()),
	}
}

type genericTestCore struct {
	token    model.ConnectToken
	deny     string
	checks   atomic.Int32
	builds   atomic.Int32
	sessions atomic.Int32
	finishes atomic.Int32
	service  *service.JMService
}

func newGenericTestCore(t *testing.T, token model.ConnectToken, deny string) *genericTestCore {
	t.Helper()
	c := &genericTestCore{token: token, deny: deny}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response any = map[string]any{}
		switch {
		case r.URL.Path == fmt.Sprintf(service.UserPermsAssetsURL, token.User.ID):
			assets := []model.PermAsset{{ID: token.Asset.ID}}
			if deny == "asset" {
				assets = nil
			}
			if deny == "ambiguous" {
				assets = append(assets, model.PermAsset{ID: "other"})
			}
			response = model.PaginationResponse{Data: assets}
		case r.URL.Path == fmt.Sprintf(service.UserPermsAssetAccountsURL, token.User.ID, token.Asset.ID):
			accounts := []model.PermAccount{{Username: "root", Alias: "test-account", HasSecret: true}}
			if deny == "account" {
				accounts = nil
			}
			response = model.PermAssetDetail{ID: token.Asset.ID, PermedAccounts: accounts, PermedProtocols: token.Asset.Protocols}
		case r.URL.Path == service.SuperConnectTokenInfoURL:
			c.builds.Add(1)
			var req service.SuperConnectTokenReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserId != token.User.ID || req.AssetId != token.Asset.ID || req.Account != "test-account" {
				t.Error("incorrect Core asset/account binding")
			}
			response = model.ConnectTokenInfo{ID: token.Id}
			if deny == "review" {
				w.WriteHeader(http.StatusForbidden)
				response = model.ConnectTokenInfo{Code: model.ACLReview}
			}
		case r.URL.Path == service.SuperConnectTokenSecretURL:
			response = token
		case r.URL.Path == fmt.Sprintf(service.SuperConnectTokenCheckURL, token.Id):
			c.checks.Add(1)
			response = model.TokenCheckStatus{Code: model.CodePermOk, Expired: true}
			if deny == "permission" {
				w.WriteHeader(http.StatusForbidden)
				response = model.TokenCheckStatus{Code: model.CodePermAccountInvalid}
			}
		case r.URL.Path == service.SessionListURL:
			c.sessions.Add(1)
			var sess model.Session
			if err := json.NewDecoder(r.Body).Decode(&sess); err != nil || sess.TokenId != token.Id || sess.UserID != token.User.ID || sess.AssetID != token.Asset.ID {
				t.Error("incorrect Core tunnel session binding")
			}
			response = sess
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, service.SessionListURL):
			c.finishes.Add(1)
		case strings.HasSuffix(r.URL.Path, "/lifecycle_log/"):
		default:
			t.Errorf("unexpected Core request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	var err error
	c.service, err = service.NewAuthJMService(service.JMSCoreHost(server.URL), service.JMSAccessKey("test-key", "01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type genericTestContext struct {
	context.Context
	sync.Mutex
	values sync.Map
}

func (c *genericTestContext) Value(key any) any       { value, _ := c.values.Load(key); return value }
func (c *genericTestContext) SetValue(key, value any) { c.values.Store(key, value) }
func (c *genericTestContext) User() string            { return "test-user@root@127.0.0.1" }
func (c *genericTestContext) SessionID() string       { return "test-connection" }
func (c *genericTestContext) ClientVersion() string   { return "SSH-2.0-test" }
func (c *genericTestContext) ServerVersion() string   { return "SSH-2.0-test" }
func (c *genericTestContext) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10000}
}
func (c *genericTestContext) LocalAddr() net.Addr           { return c.RemoteAddr() }
func (c *genericTestContext) Permissions() *ssh.Permissions { return nil }

type genericRejectedChannel struct {
	reason   gossh.RejectionReason
	accepted bool
}

func (c *genericRejectedChannel) ChannelType() string { return "direct-tcpip" }
func (c *genericRejectedChannel) ExtraData() []byte   { return nil }
func (c *genericRejectedChannel) Reject(reason gossh.RejectionReason, _ string) error {
	c.reason = reason
	return nil
}
func (c *genericRejectedChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	c.accepted = true
	return nil, nil, fmt.Errorf("unexpected accept")
}

func TestGenericForwardReject(t *testing.T) {
	for _, test := range []string{"disabled", "invalid_port", "unauthenticated", "interactive", "asset", "ambiguous", "account", "review", "permission", "expired", "unmanaged", "protocol", "identity", "action"} {
		t.Run(test, func(t *testing.T) {
			genericTestConfig(t)
			token := genericTestToken()
			switch test {
			case "expired":
				token.ExpireAt = model.ExpireInfo(time.Now().Add(-time.Minute).Unix())
			case "unmanaged":
				token.Account.Username = model.InputUser
			case "protocol":
				token.Protocol = "mysql"
			case "identity":
				token.User.ID = "other-user"
			case "action":
				token.Actions = nil
			}
			core := newGenericTestCore(t, token, test)
			s := &Server{jmsService: core.service}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sshCtx := &genericTestContext{Context: ctx}
			sshCtx.SetValue(auth.ContextKeyUser, &model.User{ID: "test-user"})
			req := &auth.DirectLoginAssetReq{Protocol: "ssh", AssetTarget: "127.0.0.1", AccountUsername: "root"}
			if test == "expired" || test == "unmanaged" || test == "protocol" || test == "identity" || test == "action" {
				req.ConnectToken = &token
			}
			sshCtx.SetValue(auth.ContextKeyDirectLoginFormat, req)
			dest := "127.0.0.1:3306"
			switch test {
			case "disabled":
				config.GlobalConfig.EnableGenericTCPForward = false
			case "invalid_port":
				dest = "127.0.0.1:65536"
			case "unauthenticated":
				sshCtx.SetValue(auth.ContextKeyUser, (*model.User)(nil))
			case "interactive":
				sshCtx.SetValue(auth.ContextKeyDirectLoginFormat, (*auth.DirectLoginAssetReq)(nil))
			}
			ch := &genericRejectedChannel{}
			s.DirectTCPIPChannelHandler(sshCtx, ch, dest)
			if ch.accepted || ch.reason != gossh.Prohibited || core.sessions.Load() != 0 {
				t.Fatalf("request not safely rejected: accepted=%v reason=%v", ch.accepted, ch.reason)
			}
			if f, ok := sshCtx.Value(genericForwardContextKey{}).(*genericForward); ok {
				waitGenericDone(t, f.done)
			} else if core.builds.Load() != 0 || core.checks.Load() != 0 {
				t.Fatal("denied destination or identity reached Core")
			}
		})
	}
}

func waitGenericDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward cleanup timed out")
	}
}

type genericLifetimeClient struct {
	done   chan struct{}
	closed sync.Once
}

func (c *genericLifetimeClient) Wait() error { <-c.done; return nil }
func (c *genericLifetimeClient) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *genericLifetimeClient) Close() error {
	c.closed.Do(func() { close(c.done) })
	return nil
}

func TestGenericForwardPermissionDeadline(t *testing.T) {
	f := newGenericForward(context.Background())
	defer f.cancel()
	client := &genericLifetimeClient{done: make(chan struct{})}
	if err := f.initialize(func() error {
		f.client = client
		f.expire = model.ExpireInfo(time.Now().Unix() + 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitGenericDone(t, f.done)
	if err := f.initialize(func() error { t.Fatal("expired connection initialized again"); return nil }); err == nil {
		t.Fatal("expired permission accepted a new channel")
	}
}

func TestGenericForwardDialTimeoutClosesClient(t *testing.T) {
	genericTestConfig(t)
	config.GlobalConfig.SSHTimeout = 1
	ctx := &genericTestContext{Context: context.Background()}
	token := genericTestToken()
	ctx.SetValue(auth.ContextKeyUser, &token.User)
	ctx.SetValue(auth.ContextKeyDirectLoginFormat, &auth.DirectLoginAssetReq{Protocol: "ssh", ConnectToken: &token})
	f := newGenericForward(ctx)
	defer f.cancel()
	if err := f.initialize(func() error {
		f.client = &genericLifetimeClient{done: make(chan struct{})}
		f.expire = token.ExpireAt
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx.SetValue(genericForwardContextKey{}, f)
	ch := &genericRejectedChannel{}
	(&Server{}).DirectTCPIPChannelHandler(ctx, ch, "database.internal:3306")
	if ch.accepted || ch.reason != gossh.ConnectionFailed {
		t.Fatal("failed destination must reject the channel")
	}
	waitGenericDone(t, f.done)
}

func startGenericTestSSH(t *testing.T, server *ssh.Server) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server.HostSigners = []ssh.Signer{signer}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); waitGenericDone(t, done) })
	return listener.Addr().String()
}

func TestGenericForwardWithoutSession(t *testing.T) {
	for _, end := range []string{"connection", "backend", "permission", "token"} {
		t.Run(end, func(t *testing.T) {
			genericTestConfig(t)
			var sessions, backendConnections, backendChannels atomic.Int32
			backend := &ssh.Server{
				PasswordHandler: func(_ ssh.Context, password string) error {
					backendConnections.Add(1)
					if password != "test-only" {
						return fmt.Errorf("invalid test credential")
					}
					return nil
				},
				Handler: func(ssh.Session) { sessions.Add(1) },
				ChannelHandlers: map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler,
					"direct-tcpip": func(_ *ssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, _ ssh.Context) {
						backendChannels.Add(1)
						var payload struct {
							Host       string
							Port       uint32
							Origin     string
							OriginPort uint32
						}
						if err := gossh.Unmarshal(newChan.ExtraData(), &payload); err != nil || payload.Host != "127.0.0.1" || payload.Port != 3307 {
							t.Error("wrong asset-side TCP destination")
						}
						ch, reqs, err := newChan.Accept()
						if err != nil {
							return
						}
						defer func() { _ = ch.Close() }()
						go gossh.DiscardRequests(reqs)
						data, err := io.ReadAll(ch)
						if err == nil {
							_, _ = ch.Write(data)
						}
					},
				},
			}
			backendAddr := startGenericTestSSH(t, backend)
			host, port, _ := net.SplitHostPort(backendAddr)
			portNumber, _ := strconv.Atoi(port)
			token := genericTestToken()
			token.Asset.Address, token.Asset.Protocols[0].Port = host, portNumber
			core := newGenericTestCore(t, token, "")
			s := &Server{jmsService: core.service}
			contexts := make(chan ssh.Context, 1)
			inbound := &ssh.Server{
				MaxSessions: 2,
				PasswordHandler: func(ctx ssh.Context, _ string) error {
					ctx.SetValue(auth.ContextKeyUser, &token.User)
					req := &auth.DirectLoginAssetReq{Protocol: "ssh", AssetTarget: "127.0.0.1", AccountUsername: "root"}
					if end == "token" {
						req.ConnectToken = &token
					}
					ctx.SetValue(auth.ContextKeyDirectLoginFormat, req)
					contexts <- ctx
					return nil
				},
				Handler: func(ssh.Session) { sessions.Add(1) },
				ChannelHandlers: map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler,
					"direct-tcpip": func(_ *ssh.Server, _ *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
						var payload struct {
							Host       string
							Port       uint32
							Origin     string
							OriginPort uint32
						}
						if err := gossh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
							return
						}
						if !s.LocalPortForwardingPermission(ctx, payload.Host, payload.Port) {
							_ = newChan.Reject(gossh.Prohibited, "disabled")
							return
						}
						s.DirectTCPIPChannelHandler(ctx, newChan, net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
					},
				},
			}
			addr := startGenericTestSSH(t, inbound)
			client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{User: "test-user@root@127.0.0.1", Auth: []gossh.AuthMethod{gossh.Password("test-only")}, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			sshCtx := <-contexts
			connections := make([]net.Conn, 2)
			var wg sync.WaitGroup
			for i := range connections {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					conn, dialErr := client.Dial("tcp", "127.0.0.1:3307")
					if dialErr != nil {
						t.Error(dialErr)
						return
					}
					connections[i] = conn
				}(i)
			}
			wg.Wait()
			for _, conn := range connections {
				if conn == nil {
					t.Fatal("channel open failed")
				}
				defer func() { _ = conn.Close() }()
			}
			f := sshCtx.Value(genericForwardContextKey{}).(*genericForward)
			if extra, err := client.Dial("tcp", "127.0.0.1:3307"); err == nil {
				_ = extra.Close()
				t.Fatal("channel limit ignored")
			}
			if sessions.Load() != 0 || backendConnections.Load() != 1 || backendChannels.Load() != 2 || core.checks.Load() != 1 || core.sessions.Load() != 1 {
				t.Fatalf("wrong initialization: sessions=%d clients=%d channels=%d checks=%d Core_sessions=%d", sessions.Load(), backendConnections.Load(), backendChannels.Load(), core.checks.Load(), core.sessions.Load())
			}
			wantBuilds := int32(1)
			if end == "token" {
				wantBuilds = 0
			}
			if core.builds.Load() != wantBuilds {
				t.Fatal("connect token built more than once")
			}
			if _, err := connections[0].Write([]byte("half-close payload")); err != nil {
				t.Fatal(err)
			}
			if err := connections[0].(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(connections[0])
			if err != nil || string(data) != "half-close payload" {
				t.Fatalf("bidirectional half-close forwarding failed: %q %v", data, err)
			}
			switch end {
			case "backend":
				_ = backend.Close()
			case "permission":
				var trace *session.Session
				for _, candidate := range session.GetSessions() {
					if candidate.TokenId == token.Id {
						trace = candidate
					}
				}
				if trace == nil {
					t.Fatal("tunnel missing from session manager")
				}
				if err := trace.HandleTask(&model.TerminalTask{Name: model.TaskPermExpired}); err != nil {
					t.Fatal(err)
				}
			default:
				_ = client.Close()
			}
			waitGenericDone(t, f.done)
			if retry, err := client.Dial("tcp", "127.0.0.1:3307"); err == nil {
				_ = retry.Close()
				t.Fatal("closed forward revived")
			}
			if core.finishes.Load() != 1 || len(session.GetSessions()) != 0 {
				t.Fatal("forward resources or Core session leaked")
			}
		})
	}
}
