package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/jumpserver-dev/sdk-go/common"
	"github.com/jumpserver-dev/sdk-go/model"
	gossh "golang.org/x/crypto/ssh"

	"github.com/jumpserver/koko/pkg/auth"
	"github.com/jumpserver/koko/pkg/config"
	"github.com/jumpserver/koko/pkg/logger"
	"github.com/jumpserver/koko/pkg/session"
	"github.com/jumpserver/koko/pkg/srvconn"
)

type genericForwardContextKey struct{}

type genericForwardClient interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	Wait() error
	Close() error
}

// 状态保存在入站 Connection 的 Context 中，关闭后保留拒绝状态，防止重新授权复活。
type genericForward struct {
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	done   chan struct{}

	client  genericForwardClient
	expire  model.ExpireInfo
	userID  string
	assetID string
	finish  func()
	err     error
}

func newGenericForward(parent context.Context) *genericForward {
	ctx, cancel := context.WithCancel(parent)
	return &genericForward{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (f *genericForward) initialize(init func() error) error {
	f.once.Do(func() {
		f.err = f.ctx.Err()
		if f.err == nil {
			f.err = init()
		}
		if f.err != nil {
			f.cancel()
		}
		go f.run()
	})
	if f.err != nil {
		return f.err
	}
	if f.expire.IsExpired(time.Now()) {
		f.cancel()
	}
	return f.ctx.Err()
}

func (f *genericForward) run() {
	defer close(f.done)
	if f.client == nil {
		return
	}
	backendDone := make(chan struct{})
	go func() {
		_ = f.client.Wait()
		f.cancel()
		close(backendDone)
	}()
	timer := time.AfterFunc(time.Until(time.Unix(int64(f.expire), 0)), f.cancel)
	defer timer.Stop()
	<-f.ctx.Done()
	_ = f.client.Close()
	<-backendDone
	if f.finish != nil {
		f.finish()
	}
}

func (s *Server) genericTCPIPChannelHandler(ctx ssh.Context, newChan gossh.NewChannel, dest string) {
	cf := config.GetConf()
	host, port, targetErr := net.SplitHostPort(dest)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if !cf.EnableGenericTCPForward || targetErr != nil || host == "" || portErr != nil || portNumber == 0 {
		_ = newChan.Reject(gossh.Prohibited, "invalid TCP forwarding destination")
		return
	}
	user, req, err := genericForwardIdentity(ctx)
	if err != nil {
		_ = newChan.Reject(gossh.Prohibited, "TCP forwarding requires authenticated direct SSH login")
		return
	}
	ctx.Lock()
	f, ok := ctx.Value(genericForwardContextKey{}).(*genericForward)
	if !ok {
		f = newGenericForward(ctx)
		ctx.SetValue(genericForwardContextKey{}, f)
	}
	ctx.Unlock()
	if err = f.initialize(func() error { return s.initializeGenericForward(ctx, f, user, req) }); err != nil {
		logger.Infof("TCP forward user_id=%s result=authorization_or_connection_failed", user.ID)
		_ = newChan.Reject(gossh.Prohibited, "TCP forwarding authorization or asset connection failed")
		return
	}
	timeout := time.Duration(cf.SSHTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(f.ctx, timeout)
	dConn, err := f.client.DialContext(dialCtx, "tcp", dest)
	dialCanceled := dialCtx.Err() != nil
	cancel()
	if err != nil {
		// SSH Channel 打开请求不能单独撤销，超时后关闭独占 Client，回收底层等待者。
		if dialCanceled {
			f.cancel()
		}
		logger.Infof("TCP forward user_id=%s asset_id=%s destination=%q result=dial_failed", f.userID, f.assetID, dest)
		_ = newChan.Reject(gossh.ConnectionFailed, "asset TCP connection failed")
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = dConn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	logger.Infof("TCP forward user_id=%s asset_id=%s destination=%q result=started", f.userID, f.assetID, dest)
	bridgeGenericForward(f.ctx, ch, dConn)
	logger.Infof("TCP forward user_id=%s asset_id=%s destination=%q result=finished", f.userID, f.assetID, dest)
}

func genericForwardIdentity(ctx ssh.Context) (*model.User, *auth.DirectLoginAssetReq, error) {
	user, ok := ctx.Value(auth.ContextKeyUser).(*model.User)
	req, direct := ctx.Value(auth.ContextKeyDirectLoginFormat).(*auth.DirectLoginAssetReq)
	if !ok || user == nil || user.ID == "" || !direct || req == nil || req.Protocol != model.ProtocolSSH ||
		(!req.IsToken() && (req.AssetTarget == "" || !genericForwardAccount(req.AccountUsername))) {
		return nil, nil, errors.New("authenticated direct SSH login required")
	}
	return user, req, nil
}

func genericForwardAccount(username string) bool {
	switch username {
	case "", model.InputUser, model.DynamicUser, model.ANONUser, "null":
		return false
	default:
		return true
	}
}

func validateGenericForwardToken(token *model.ConnectToken, userID string) error {
	if token == nil || token.Id == "" || token.User.ID != userID || token.Asset.ID == "" ||
		token.Protocol != model.ProtocolSSH || !token.Asset.IsSupportProtocol(model.ProtocolSSH) ||
		!token.Actions.EnableConnect() || token.ExpireAt.IsExpired(time.Now()) ||
		token.Code != "" || token.Error != "" || token.Account.ID == "" ||
		!genericForwardAccount(token.Account.Username) || token.Account.Secret == "" || token.Account.SuFrom != nil {
		return errors.New("SSH asset or managed account permission denied")
	}
	return nil
}

func (s *Server) initializeGenericForward(ctx ssh.Context, f *genericForward, user *model.User, req *auth.DirectLoginAssetReq) error {
	token := req.ConnectToken
	if !req.IsToken() {
		var err error
		token, err = s.buildConnectToken(ctx, user, req)
		if err != nil {
			return errors.New("core asset authorization failed")
		}
	}
	if err := validateGenericForwardToken(token, user.ID); err != nil {
		return err
	}
	// Token 状态表示持续的资产权限，不以是否已消费一次性登录令牌代替它。
	status, err := s.jmsService.CheckTokenStatus(token.Id)
	if err != nil || status.Code != model.CodePermOk || f.ctx.Err() != nil {
		return errors.New("core token permission check failed")
	}
	var gateways []model.Gateway
	if token.Gateway != nil {
		gateways = []model.Gateway{*token.Gateway}
	}
	opts := buildSSHClientOptions(&token.Asset, &token.Account, gateways)
	timeout := time.Duration(config.GetConf().SSHTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(f.ctx, timeout)
	client, err := srvconn.NewSSHClientContext(connectCtx, opts...)
	cancel()
	if err != nil {
		return errors.New("asset SSH connection failed")
	}
	host, _, _ := net.SplitHostPort(ctx.RemoteAddr().String())
	apiSession := token.CreateSession(host, model.LoginFromSSH, model.TUNNELType)
	apiSession.ID = common.UUID()
	if _, err = s.jmsService.CreateSession(apiSession); err != nil {
		_ = client.Close()
		return errors.New("core tunnel session creation failed")
	}
	trace := session.NewSession(&apiSession, func(task *model.TerminalTask) error {
		switch task.Name {
		case model.TaskKillSession, model.TaskPermExpired, model.TaskLockSession:
			f.cancel()
			return nil
		case model.TaskPermValid:
			return nil
		default:
			return fmt.Errorf("TCP forwarding does not support task %s", task.Name)
		}
	})
	session.AddSession(trace)
	s.recordSessionLifecycle(apiSession.ID, model.AssetConnectSuccess, "")
	f.client, f.expire = client, token.ExpireAt
	f.userID, f.assetID = user.ID, token.Asset.ID
	f.finish = func() {
		session.RemoveSession(trace)
		if _, finishErr := s.jmsService.SessionFinished(apiSession.ID, common.NewNowUTCTime()); finishErr != nil {
			logger.Errorf("TCP forward session_id=%s result=finish_failed", apiSession.ID)
		}
		s.recordSessionLifecycle(apiSession.ID, model.AssetConnectFinished, "")
	}
	return nil
}

func bridgeGenericForward(ctx context.Context, channel gossh.Channel, conn net.Conn) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = channel.Close(); _ = conn.Close() }) }
	defer closeBoth()
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(conn, channel); err != nil {
			closeBoth()
			return
		}
		if writer, ok := conn.(interface{ CloseWrite() error }); ok {
			if err := writer.CloseWrite(); err != nil {
				closeBoth()
			}
		} else {
			closeBoth()
		}
	}()
	if _, err := io.Copy(channel, conn); err != nil {
		closeBoth()
	} else if err := channel.CloseWrite(); err != nil {
		closeBoth()
	}
	<-done
}
