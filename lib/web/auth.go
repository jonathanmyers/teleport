/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package web

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"
	"github.com/mailgun/ttlmap"
	"golang.org/x/crypto/ssh"
)

type Context interface {
	io.Closer
	ConnectUpstream(addr string, user string) (*sshutils.Upstream, error)
	GetAuthMethods() ([]ssh.AuthMethod, error)
	GetWebSID() string
	GetUser() string
	GetClient() (auth.ClientI, error)
}

type LocalContext struct {
	sid  string
	user string
	clt  *auth.TunClient
}

func (c *LocalContext) GetClient() (auth.ClientI, error) {
	return c.clt, nil
}

func (c *LocalContext) GetUser() string {
	return c.user
}

func (c *LocalContext) GetWebSID() string {
	return c.sid
}

func (c *LocalContext) GetAuthMethods() ([]ssh.AuthMethod, error) {
	a, err := c.clt.GetAgent()
	if err != nil {
		log.Errorf("failed to get agent: %v", err)
		return nil, err
	}
	signers, err := a.Signers()
	if err != nil {
		log.Errorf("failed to get signers: %v", err)
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

func (c *LocalContext) Close() error {
	if c.clt != nil {
		return c.clt.Close()
	}
	return nil
}

func (c *LocalContext) ConnectUpstream(addr string, user string) (*sshutils.Upstream, error) {
	agent, err := c.clt.GetAgent()
	if err != nil {
		return nil, fmt.Errorf("failed to get agent: %v", err)
	}
	signers, err := agent.Signers()
	if err != nil {
		return nil, fmt.Errorf("no signers: %v", err)
	}
	return sshutils.DialUpstream(user, addr, signers)
}

type RequestHandler func(http.ResponseWriter, *http.Request, httprouter.Params, Context)

type AuthHandler interface {
	GetHost() string
	Auth(user, pass string, hotpToken string) (string, error)
	GetCertificate(c SSHLoginCredentials) (SSHLoginResponse, error)
	NewUserForm(token string) (user string, QRImg []byte, hotpFirstValues []string, e error)
	NewUserFinish(token, password, hotpToken string) error
	ValidateSession(user, sid string) (Context, error)
	SetSession(w http.ResponseWriter, user, sid string) error
	ClearSession(w http.ResponseWriter)
}

func NewLocalAuth(host string, servers []utils.NetAddr) (*LocalAuth, error) {
	m, err := ttlmap.NewMap(1024, ttlmap.CallOnExpire(CloseContext))
	if err != nil {
		return nil, err
	}
	return &LocalAuth{
		host:        host,
		sessions:    m,
		authServers: servers,
	}, nil
}

type LocalAuth struct {
	sessions    *ttlmap.TtlMap
	authServers []utils.NetAddr
	host        string
}

func (s *LocalAuth) GetHost() string {
	return s.host
}

func CloseContext(key string, val interface{}) {
	log.Infof("closing context %v", key)
	ctx := val.(Context)
	err := ctx.Close()
	if err != nil {
		log.Errorf("failed closing context: %v", err)
	}
}

func (s *LocalAuth) Auth(user, pass string, hotpToken string) (string, error) {
	method, err := auth.NewWebPasswordAuth(user, []byte(pass), hotpToken)
	if err != nil {
		return "", err
	}
	clt, err := auth.NewTunClient(s.authServers[0], user, method)
	if err != nil {
		return "", err
	}
	return clt.SignIn(user, []byte(pass))
}

func (s *LocalAuth) GetCertificate(c SSHLoginCredentials) (SSHLoginResponse, error) {
	method, err := auth.NewWebPasswordAuth(c.User, []byte(c.Password),
		c.HOTPToken)
	if err != nil {
		return SSHLoginResponse{}, trace.Wrap(err)
	}

	clt, err := auth.NewTunClient(s.authServers[0], c.User, method)
	if err != nil {
		return SSHLoginResponse{}, trace.Wrap(err)
	}
	cert, err := clt.GenerateUserCert(c.PubKey, c.User, c.TTL)
	if err != nil {
		return SSHLoginResponse{}, trace.Wrap(err)
	}
	hostSigners, err := clt.GetCertAuthorities(services.HostCA)
	if err != nil {
		return SSHLoginResponse{}, trace.Wrap(err)
	}

	signers := []services.CertAuthority{}
	for _, hs := range hostSigners {
		signers = append(signers, *hs)
	}

	return SSHLoginResponse{
		Cert:        cert,
		HostSigners: signers,
	}, nil
}

func (s *LocalAuth) NewUserForm(token string) (user string,
	QRImg []byte, hotpFirstValues []string, e error) {

	method, err := auth.NewSignupTokenAuth(token)
	if err != nil {
		return "", nil, nil, trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], "tokenAuth", method)
	if err != nil {
		return "", nil, nil, trace.Wrap(err)
	}

	return clt.GetSignupTokenData(token)
}

func (s *LocalAuth) NewUserFinish(token, password, hotpToken string) error {
	method, err := auth.NewSignupTokenAuth(token)
	if err != nil {
		return trace.Wrap(err)
	}
	clt, err := auth.NewTunClient(s.authServers[0], "tokenAuth", method)
	if err != nil {
		return trace.Wrap(err)
	}

	return clt.CreateUserWithToken(token, password, hotpToken)
}

func (s *LocalAuth) ValidateSession(user, sid string) (Context, error) {
	val, ok := s.sessions.Get(user + sid)
	if ok {
		return val.(Context), nil
	}

	method, err := auth.NewWebSessionAuth(user, []byte(sid))
	if err != nil {
		return nil, err
	}
	clt, err := auth.NewTunClient(s.authServers[0], user, method)
	if err != nil {
		log.Infof("failed to connect: %v", clt, err)
		return nil, err
	}
	if _, err := clt.GetWebSession(user, sid); err != nil {
		log.Infof("session not found: %v", err)
		return nil, err
	}
	log.Infof("session validated")

	c := &LocalContext{
		clt:  clt,
		user: user,
		sid:  sid,
	}
	if err := s.sessions.Set(user+sid, c, 600); err != nil {
		log.Infof("something is wrong: %v", err)
		return nil, err
	}
	return c, nil
}

func (s *LocalAuth) SetSession(w http.ResponseWriter, user, sid string) error {
	d, err := EncodeCookie(user, sid)
	if err != nil {
		return err
	}
	c := &http.Cookie{
		Domain: fmt.Sprintf(".%v", s.host),
		Name:   "session",
		Value:  d,
		Path:   "/",
	}
	http.SetCookie(w, c)
	return nil
}

func (s *LocalAuth) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Domain: fmt.Sprintf(".%v", s.host),
		Name:   "session",
		Value:  "",
		Path:   "/",
	})
}
