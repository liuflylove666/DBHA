package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyStopEvidenceDoesNotTrustMissingPIDFile(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to verify the Linux deployment command")
	}
	for _, tc := range []struct {
		name, comm, stat, want string
		failure                bool
	}{
		{name: "no processes", want: "STOPPED"},
		{name: "running without PID file", comm: "mysql-proxy", stat: "123 (mysql-proxy) S 1", want: "RUNNING"},
		{name: "exited zombie", comm: "mysql-proxy", stat: "123 (mysql-proxy) Z 1", want: "STOPPED"},
		{name: "unrelated process", comm: "sshd", stat: "123 (sshd) S 1", want: "STOPPED"},
		{name: "unreadable process evidence", comm: "mysql-proxy", stat: "invalid", failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.comm != "" {
				p := filepath.Join(dir, "123")
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "comm"), []byte(tc.comm), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "stat"), []byte(tc.stat), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command(python, "-c", proxyProcessCheck, dir).CombinedOutput()
			if tc.failure {
				if err == nil {
					t.Fatal("unknown evidence was treated as stopped")
				}
				return
			}
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("got %q, %v; want %s", out, err, tc.want)
			}
		})
	}
}
