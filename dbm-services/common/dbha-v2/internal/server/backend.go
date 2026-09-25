package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	switchmysql "dbm-services/common/dbha-v2/internal/analysis/switcher/mysql"
	"dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/storage/hamysql"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type observationResult = discovery.Observation

func (s *Server) profile(st *State, deployment string) CredentialProfile {
	name := st.Deployments[deployment].CredentialProfile
	if name == "" {
		name = "default"
	}
	return s.Config.Profiles[name]
}

func allowedEndpoint(endpoint string, networks []string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, value := range networks {
		_, network, err := net.ParseCIDR(value)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func dbConnection(endpoint, user, password string) (*sql.DB, error) {
	return discovery.OpenLocalMySQL(discovery.LocalMySQL{Network: "tcp", Address: endpoint, User: user, Password: password})
}

func (s *Server) inspect(ctx context.Context, instance Instance, profile CredentialProfile) (observationResult, error) {
	if !allowedEndpoint(instance.Endpoint, s.Config.AllowedNetworks) {
		return observationResult{}, apiError(412, "ADDRESS_UNRESOLVED", "endpoint outside configured networks")
	}
	host, portText, _ := net.SplitHostPort(instance.Endpoint)
	port, _ := strconv.Atoi(portText)
	user, password := profile.MySQLUser, profile.MySQLPassword
	if instance.Kind == "proxy" {
		user, password = profile.ProxyUser, profile.ProxyPassword
	}
	endpoint := instance.Endpoint
	var observed discovery.Observation
	if instance.Kind == "proxy" {
		st, err := s.Store.Read(ctx)
		if err != nil {
			return observed, err
		}
		if err = json.Unmarshal(st.Observations[instance.ID].Payload, &observed); err != nil {
			return observed, err
		}
		if observed.AdminPort == 0 {
			return observed, errors.New("missing proxy admin port")
		}
		endpoint = net.JoinHostPort(host, strconv.Itoa(int(observed.AdminPort)))
	}
	db, err := dbConnection(endpoint, user, password)
	if err != nil {
		return observed, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if instance.Kind == "mysql" {
		return discovery.CollectMySQL(ctx, db, host, uint32(port))
	}
	return discovery.CollectProxy(ctx, db, host, uint32(port), observed.AdminPort, strings.TrimPrefix(instance.ID, "proxy:"))
}

func sameBackend(o discovery.Observation, endpoint string) bool {
	if o.CollectionState != "OK" || o.BackendQueryState != "OK" || len(o.Backends) == 0 {
		return false
	}
	for _, b := range o.Backends {
		if b.Address != endpoint {
			return false
		}
	}
	return true
}

func (s *Server) refreshProxy(ctx context.Context, instance Instance, profile CredentialProfile, target string) error {
	o, err := s.inspect(ctx, instance, profile)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(instance.Endpoint)
	targetHost, targetPort, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(targetPort)
	if net.ParseIP(targetHost) == nil {
		return errors.New("invalid target IP")
	}
	// Keep the vendor's refresh_backends implementation and independently check
	// for a nonempty result; its legacy helper accepts an empty backend table.
	if err = switchmysql.ProxyRefreshBackends(host, int(o.AdminPort), profile.ProxyUser, profile.ProxyPassword, targetHost, port, nil); err != nil {
		return errors.New("proxy refresh failed; result requires verification")
	}
	o, err = s.inspect(ctx, instance, profile)
	if err != nil {
		return err
	}
	if !sameBackend(o, target) {
		return errors.New("proxy backend verification failed")
	}
	return nil
}

func (s *Server) promote(ctx context.Context, instance Instance, profile CredentialProfile, sourceUUID string) error {
	o, err := s.inspect(ctx, instance, profile)
	if err != nil {
		return err
	}
	if "mysql:"+o.ServerUUID != instance.ID || !o.ReadOnly || len(o.Channels) != 1 || o.Channels[0].SourceUUID != sourceUUID || !o.Channels[0].SQLRunning || o.GTIDMode != "ON" {
		return errors.New("candidate no longer matches prepared evidence")
	}
	host, portText, _ := net.SplitHostPort(instance.Endpoint)
	port, _ := strconv.Atoi(portText)
	db, err := dbConnection(instance.Endpoint, profile.MySQLUser, profile.MySQLPassword)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = s.Store.Ready(ctx); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, "STOP REPLICA IO_THREAD"); err != nil {
		return err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		o, err = discovery.CollectMySQL(ctx, db, host, uint32(port))
		if err != nil {
			return err
		}
		if len(o.Channels) != 1 || o.Channels[0].SourceUUID != sourceUUID || !o.Channels[0].SQLRunning {
			return errors.New("candidate replication changed during drain")
		}
		var applied int
		if err = db.QueryRowContext(ctx, "SELECT GTID_SUBSET(?, @@GLOBAL.gtid_executed)", o.Channels[0].RetrievedGTIDSet).Scan(&applied); err != nil {
			return err
		}
		if applied == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("relay log drain timed out")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err = s.Store.Ready(ctx); err != nil {
		return err
	}
	legacy, err := hamysql.NewGormDB(hamysql.OptionIP(host), hamysql.OptionPort(port), hamysql.OptionUser(profile.MySQLUser), hamysql.OptionPassword(profile.MySQLPassword), hamysql.OptionTimeout(requestTimeout))
	if err != nil {
		return err
	}
	defer legacy.Close()
	if err = switchmysql.DoStopSlave(legacy, nil); err != nil {
		return err
	}
	if err = s.Store.Ready(ctx); err != nil {
		return err
	}
	if err = switchmysql.DoResetSlave(legacy, nil); err != nil {
		return err
	}
	for _, query := range []string{"SET PERSIST super_read_only=OFF", "SET PERSIST read_only=OFF"} {
		if err = s.Store.Ready(ctx); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	o, err = discovery.CollectMySQL(ctx, db, host, uint32(port))
	if err != nil {
		return err
	}
	if "mysql:"+o.ServerUUID != instance.ID || len(o.Channels) != 0 || o.ReadOnly || o.SuperReadOnly {
		return errors.New("promotion postcondition failed")
	}
	return nil
}

// A network failure is distinct from an authentication or SQL error. The
// latter must never be interpreted as evidence that a database has stopped.
func networkFailure(err error) bool {
	var netErr *net.OpError
	return errors.As(err, &netErr)
}

func (s *Server) sshOutput(ctx context.Context, instance Instance, p CredentialProfile, command string) ([]byte, error) {
	if p.SSHKnownHostsFile == "" {
		return nil, errors.New("SSH known hosts file is required")
	}
	callback, err := knownhosts.New(p.SSHKnownHostsFile)
	if err != nil {
		return nil, err
	}
	var methods []ssh.AuthMethod
	if p.SSHPassword != "" {
		methods = append(methods, ssh.Password(p.SSHPassword))
	}
	if p.SSHKeyFile != "" {
		b, err := os.ReadFile(p.SSHKeyFile)
		if err != nil {
			return nil, err
		}
		key, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(key))
	}
	host, _, _ := net.SplitHostPort(instance.Endpoint)
	port := p.SSHPort
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{User: p.SSHUser, Auth: methods, HostKeyCallback: callback, Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	return session.Output(command)
}

func (s *Server) sshHealth(ctx context.Context, instance Instance, p CredentialProfile) (bool, error) {
	out, runErr := s.sshOutput(ctx, instance, p, p.ProbeHealthCommand)
	if networkFailure(runErr) {
		return true, runErr
	}
	var body struct {
		State           string `json:"state"`
		Status          string `json:"status"`
		CollectionState string `json:"collection_state"`
		ErrorCode       string `json:"error_code"`
	}
	if json.Unmarshal(out, &body) != nil {
		return false, errors.New("invalid remote probe health response")
	}
	var exit *ssh.ExitError
	if errors.As(runErr, &exit) && exit.ExitStatus() == 2 && (body.State == "DB_DOWN" || body.Status == "DB_DOWN" || body.ErrorCode == "DB_DOWN") {
		return true, nil
	}
	if runErr != nil {
		return false, fmt.Errorf("remote health failed without database-down evidence")
	}
	return false, nil
}

func (s *Server) verifyProxyStopped(ctx context.Context, instance Instance, profile CredentialProfile) error {
	if s.verifyStop != nil {
		return s.verifyStop(ctx, instance, profile)
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", instance.Endpoint)
	if err == nil {
		conn.Close()
		return errors.New("proxy data port still accepts connections")
	}
	if !networkFailure(err) {
		return err
	}
	// PID files can disappear while the daemon is alive. Check the actual
	// process table, conservatively blocking when any Proxy is still running.
	// EC2 containers share the host PID namespace for this verification.
	command := "python3 -c '" + strings.ReplaceAll(proxyProcessCheck, "'", "'\"'\"'") + "' /proc"
	out, err := s.sshOutput(ctx, instance, profile, command)
	if err != nil {
		return errors.New("proxy process could not be independently verified")
	}
	if strings.TrimSpace(string(out)) != "STOPPED" {
		return errors.New("proxy process remains running")
	}
	return nil
}

const proxyProcessCheck = `import pathlib, sys
root = pathlib.Path(sys.argv[1])
for process in root.iterdir():
    if not process.name.isdigit():
        continue
    try:
        name = (process / "comm").read_text().strip()
        if name != "mysql-proxy":
            continue
        state = (process / "stat").read_text().rsplit(") ", 1)[1].split()[0]
    except FileNotFoundError:
        continue
    if state not in ("Z", "X"):
        print("RUNNING")
        sys.exit(0)
print("STOPPED")
`
