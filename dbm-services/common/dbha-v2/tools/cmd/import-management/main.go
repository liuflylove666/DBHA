// import-management is an offline, fail-closed migration from the old
// management MySQL into one DBHA etcd namespace. Dry-run is the default.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"dbm-services/common/dbha-v2/internal/server"
	"dbm-services/common/dbha-v2/pkg/discovery"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migration:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("import-management", flag.ContinueOnError)
	var source, schema, dsnEnv, agentMap, bizUser, bizPasswordFile, etcdEndpoints, environment, etcdCA, etcdCert, etcdKey, logsOut, stateDir string
	var apply, oldStopped, v1Disabled bool
	f.StringVar(&source, "source", "standalone", "standalone or hamodel")
	f.StringVar(&schema, "legacy-schema", "dbha_metadata", "old management MySQL schema")
	f.StringVar(&dsnEnv, "legacy-dsn-env", "LEGACY_MYSQL_DSN", "environment variable with old management DSN")
	f.StringVar(&agentMap, "agent-map", "", "installer-generated agent map JSON")
	f.StringVar(&bizUser, "business-user", "", "read-only MySQL user for direct identity checks")
	f.StringVar(&bizPasswordFile, "business-password-file", "", "file with read-only business MySQL password")
	f.StringVar(&etcdEndpoints, "etcd-endpoints", "127.0.0.1:2379", "comma-separated etcd endpoints")
	f.StringVar(&environment, "environment", "lab", "target DBHA environment ID")
	f.StringVar(&etcdCA, "etcd-ca", "", "etcd CA PEM")
	f.StringVar(&etcdCert, "etcd-cert", "", "etcd client certificate PEM")
	f.StringVar(&etcdKey, "etcd-key", "", "etcd client key PEM")
	f.StringVar(&logsOut, "logs-out", "", "explicit JSONL path for old switch logs; must not already exist")
	f.StringVar(&stateDir, "state-dir", "", "mounted dbha-server state directory; required with --apply")
	f.BoolVar(&apply, "apply", false, "write prechecked migration to etcd")
	f.BoolVar(&oldStopped, "confirm-old-control-stopped", false, "operator confirms old control writer and switch activity stopped")
	f.BoolVar(&v1Disabled, "confirm-v1-whitelist-disabled", false, "operator confirms external v1 whitelist disabled")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if agentMap == "" || bizUser == "" || bizPasswordFile == "" || environment == "" || strings.ContainsAny(environment, "/\\") {
		return fmt.Errorf("agent-map, business credentials and valid environment required")
	}
	if err := validateApplyPrerequisites(apply, stateDir, oldStopped, v1Disabled); err != nil {
		return err
	}
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		return fmt.Errorf("legacy DSN environment variable %s is empty", dsnEnv)
	}
	password, err := os.ReadFile(bizPasswordFile)
	if err != nil {
		return err
	}
	bindings, err := loadBindings(agentMap)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	legacy, err := readLegacy(readCtx, dsn, schema, source)
	if err != nil {
		return err
	}
	evidence := map[string]discovery.Observation{}
	for _, r := range legacy.Instances {
		if r.MachineType != "backend" {
			continue
		}
		checkCtx, done := context.WithTimeout(ctx, 4*time.Second)
		db, e := discovery.OpenLocalMySQL(discovery.LocalMySQL{Network: "tcp", Address: endpoint(r.Host, r.Port), User: bizUser, Password: strings.TrimSpace(string(password))})
		if e != nil {
			done()
			return fmt.Errorf("business MySQL %s connection setup failed: %w", endpoint(r.Host, r.Port), e)
		}
		obs, e := discovery.CollectMySQL(checkCtx, db, r.Host, uint32(r.Port))
		db.Close()
		done()
		if e != nil {
			return fmt.Errorf("business MySQL %s identity verification failed (%s): %w", endpoint(r.Host, r.Port), obs.ErrorCode, e)
		}
		evidence[bindingKey("mysql", r.Host, r.Port)] = obs
	}
	plan, err := buildPlan(legacy.Instances, bindings, evidence, legacy.Policies, legacy.Exclusions)
	if err != nil {
		return err
	}
	tlsConfig, err := etcdTLS(etcdCA, etcdCert, etcdKey)
	if err != nil {
		return err
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: strings.Split(etcdEndpoints, ","), TLS: tlsConfig, DialTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer client.Close()
	prefix := "/dbha/v1/" + url.PathEscape(environment) + "/"
	inspectCtx, done := context.WithTimeout(ctx, 5*time.Second)
	existing, err := client.Get(inspectCtx, prefix, clientv3.WithPrefix())
	done()
	if err != nil {
		return err
	}
	if err := checkExisting(existing, prefix, plan); err != nil {
		return err
	}
	if logsOut != "" {
		if err := exportLogs(ctx, dsn, schema, logsOut); err != nil {
			return err
		}
	}
	if !apply {
		fmt.Printf("DRY RUN: %d clusters, %d agents, %d exclusions, %d disabled legacy policies; maximum imported cluster_id=%d; etcd unchanged\n", len(plan.Clusters), len(plan.Agents), len(plan.Exclusions), len(plan.Policies), plan.MaxClusterID)
		return nil
	}
	store, err := server.NewStore(ctx, client, environment)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Update(ctx, func(st *server.State) error {
		for key := range plan.Deployments {
			if _, ok := st.Deployments[key]; ok {
				return fmt.Errorf("deployment %s already exists", key)
			}
		}
		for key := range plan.Clusters {
			if _, ok := st.Clusters[key]; ok {
				return fmt.Errorf("cluster %s already exists", key)
			}
		}
		for key := range plan.Agents {
			if _, ok := st.Agents[key]; ok {
				return fmt.Errorf("agent %s already exists", key)
			}
		}
		for key := range plan.Credentials {
			if _, ok := st.Credentials[key]; ok {
				return fmt.Errorf("credential %s already exists", key)
			}
		}
		for key := range plan.Instances {
			if _, ok := st.Instances[key]; ok {
				return fmt.Errorf("instance %s already exists", key)
			}
		}
		for key := range plan.EndpointIndex {
			if _, ok := st.EndpointIndex[key]; ok {
				return fmt.Errorf("endpoint %s already exists", key)
			}
		}
		for key := range plan.Policies {
			if _, ok := st.Policies[key]; ok {
				return fmt.Errorf("policy %s already exists", key)
			}
		}
		for key := range plan.Exclusions {
			if _, ok := st.Exclusions[key]; ok {
				return fmt.Errorf("exclusion %s already exists", key)
			}
		}
		for k, v := range plan.Deployments {
			st.Deployments[k] = v
		}
		for k, v := range plan.Clusters {
			st.Clusters[k] = v
		}
		for k, v := range plan.Credentials {
			st.Credentials[k] = v
		}
		for k, v := range plan.Agents {
			st.Agents[k] = v
		}
		for k, v := range plan.Instances {
			st.Instances[k] = v
		}
		for k, v := range plan.EndpointIndex {
			st.EndpointIndex[k] = v
		}
		for k, v := range plan.Policies {
			st.Policies[k] = v
		}
		for k, v := range plan.Exclusions {
			st.Exclusions[k] = v
		}
		if st.ClusterIDCounter < plan.MaxClusterID {
			st.ClusterIDCounter = plan.MaxClusterID
		}
		return nil
	}); err != nil {
		return err
	}
	if err := server.FinalizeMigration(ctx, store, stateDir); err != nil {
		return fmt.Errorf("IMPORT COMMITTED but control watermark was not persisted; DO NOT START dbha-server; repair state directory and finalize under the migration owner: %w", err)
	}
	fmt.Printf("APPLIED: %d clusters, %d agents; recovery gate MIGRATION_UNVERIFIED; switching disabled\n", len(plan.Clusters), len(plan.Agents))
	return nil
}

func validateApplyPrerequisites(apply bool, stateDir string, oldStopped, v1Disabled bool) error {
	if !apply {
		return nil
	}
	if !oldStopped || !v1Disabled {
		return fmt.Errorf("--apply requires stopped old control and disabled external v1 whitelist confirmations")
	}
	if stateDir == "" {
		return fmt.Errorf("--apply requires --state-dir mounted as dbha-server state volume")
	}
	info, err := os.Stat(stateDir)
	if err != nil {
		return fmt.Errorf("state directory preflight: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state directory preflight: not a directory")
	}
	f, err := os.CreateTemp(stateDir, ".migration-preflight-*")
	if err != nil {
		return fmt.Errorf("state directory is not writable: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func loadBindings(path string) ([]agentBinding, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bindings []agentBinding
	if err := json.Unmarshal(b, &bindings); err != nil {
		return nil, err
	}
	for i := range bindings {
		info, err := os.Stat(bindings[i].TokenFile)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("agent token file for %s must be private", bindings[i].AgentID)
		}
		secret, err := os.ReadFile(bindings[i].TokenFile)
		if err != nil {
			return nil, err
		}
		token := strings.TrimSpace(string(secret))
		raw, err := hex.DecodeString(token)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("agent token for %s must be 256-bit hex", bindings[i].AgentID)
		}
		bindings[i].TokenHash = tokenHash(token)
	}
	return bindings, nil
}

func etcdTLS(ca, cert, key string) (*tls.Config, error) {
	if ca == "" && cert == "" && key == "" {
		return nil, nil
	}
	if ca == "" || cert == "" || key == "" {
		return nil, fmt.Errorf("etcd CA, cert and key must be supplied together")
	}
	b, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("invalid etcd CA")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

func checkExisting(response *clientv3.GetResponse, prefix string, p importPlan) error {
	planned := map[string]bool{}
	add := func(section, id string) { planned[section+"/"+url.PathEscape(id)] = true }
	for id := range p.Deployments {
		add("deployments", id)
	}
	for id := range p.Clusters {
		add("clusters", id)
	}
	for id := range p.Credentials {
		add("credentials", id)
	}
	for id := range p.Agents {
		add("agents", id)
	}
	for id := range p.Instances {
		add("instances", id)
	}
	for id := range p.EndpointIndex {
		add("endpoint-index", id)
	}
	for id := range p.Policies {
		add("policies", id)
	}
	for id := range p.Exclusions {
		add("exclusions", id)
	}
	for _, kv := range response.Kvs {
		key := strings.TrimPrefix(string(kv.Key), prefix)
		if key == "coordination/server-owner" {
			return fmt.Errorf("target environment has an active control owner")
		}
		if planned[key] {
			return fmt.Errorf("target key %s already exists", key)
		}
		if key == "counters/cluster-id" {
			var v struct {
				Value json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(kv.Value, &v); err != nil {
				return err
			}
			var counter uint64
			if err := json.Unmarshal(v.Value, &counter); err != nil {
				return err
			}
		}
	}
	return nil
}
