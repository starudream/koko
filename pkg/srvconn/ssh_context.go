package srvconn

import (
	"context"
	"net"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/jumpserver/koko/pkg/common"
)

// NewSSHClientContext 用于连接独占的隧道，取消同时终止 TCP、网关和 SSH 握手。
func NewSSHClientContext(ctx context.Context, opts ...SSHClientOption) (*SSHClient, error) {
	cfg := &SSHClientOptions{Host: "127.0.0.1", Port: "22"}
	for _, setter := range opts {
		setter(cfg)
	}
	return newSSHClientContext(ctx, cfg)
}

func newSSHClientContext(ctx context.Context, cfg *SSHClientOptions) (*SSHClient, error) {
	defer func() {
		// 独占 Client 的配置仅保留连接元数据，认证完成后不再持有凭据。
		cfg.Password, cfg.PrivateKey, cfg.Passphrase = "", "", ""
		cfg.PrivateAuth, cfg.keyboardAuth = nil, nil
		cfg.proxySSHClientOptions = nil
	}()
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	var conn net.Conn
	var proxyClient *SSHClient
	var err error
	if len(cfg.proxySSHClientOptions) == 0 {
		dialer := net.Dialer{Timeout: time.Duration(cfg.Timeout) * time.Second}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	} else {
		for i := range cfg.proxySSHClientOptions {
			proxyClient, err = newSSHClientContext(ctx, &cfg.proxySSHClientOptions[i])
			if err == nil || ctx.Err() != nil {
				break
			}
		}
		if err == nil {
			conn, err = proxyClient.DialContext(ctx, "tcp", addr)
		}
	}
	if err != nil {
		if proxyClient != nil {
			_ = proxyClient.Close()
		}
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	clientConfig := &gossh.ClientConfig{
		User: cfg.Username, Auth: cfg.AuthMethods(),
		HostKeyCallback: common.NewTrustHostKeyCallback(),
		Config:          createSSHConfig(), HostKeyAlgorithms: allHostKeyAlgorithms(),
	}
	sshConn, chans, reqs, err := gossh.NewClientConn(conn, addr, clientConfig)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		if proxyClient != nil {
			_ = proxyClient.Close()
		}
		return nil, err
	}
	return &SSHClient{
		Client: gossh.NewClient(sshConn, chans, reqs), Cfg: cfg, ProxyClient: proxyClient,
		traceSessionMap: make(map[*gossh.Session]time.Time),
	}, nil
}
