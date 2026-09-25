import json
from pathlib import Path
import tempfile
import unittest

import configure
import multicluster


class DeploymentRenderTest(unittest.TestCase):
    def test_single_identity_is_stable_and_contains_no_manual_roles(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            first = configure.render(out)
            token = (out / "admin.token").read_bytes()
            second = configure.render(out)
            self.assertEqual(first, second)
            self.assertEqual(token, (out / "admin.token").read_bytes())
            self.assertEqual(0o600, (out / "server.json").stat().st_mode & 0o777)
            mysql = json.loads((out / "lab-mysql-standby-discovery.json").read_text())
            self.assertNotIn("instanceRole", json.dumps(mysql))
            self.assertNotIn("cluster_id", json.dumps(mysql))
            self.assertEqual("/var/lib/dbha-probe/identity.json", mysql["state_file"])
            server = json.loads((out / "server.json").read_text())
            self.assertNotIn("mysql_dsn", server)
            self.assertNotIn("tls_cert_file", server)
            self.assertNotIn("tls_key_file", server)
            self.assertNotIn("ca_file", mysql)
            self.assertTrue(mysql["server_url"].startswith("http://"))
            self.assertEqual([mysql["server_url"]], mysql["server_urls"])
            self.assertEqual([mysql["server_grpc"]], mysql["server_grpc_endpoints"])
            self.assertEqual(["10.203.80.0/24"], server["allowed_networks"])
            self.assertEqual(4, len(first["agents"]))

    def test_three_groups_have_four_distinct_nodes_each(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            ids = configure.render(out, configure.MULTI, multicluster.NET)
            compose = multicluster.render_compose(out)
            services = compose["services"]
            self.assertEqual(14, len([s for s in services if not s.endswith("-bootstrap")]))
            self.assertEqual({"etcd", "dbha-server"}, set(services) - {
                name for group in configure.MULTI for name in (*configure.node_names(group), group[0] + "-bootstrap")})
            self.assertEqual(3, len(ids["deployments"]))
            self.assertEqual(12, len(set(ids["agents"].values())))
            self.assertEqual(12, len({services[name]["networks"]["lab"]["ipv4_address"]
                                      for group in configure.MULTI for name in configure.node_names(group)}))
            for group in configure.MULTI:
                name = group[0] + "-proxy1"
                self.assertNotIn(group[0] + "-bootstrap", services[name]["depends_on"])
                self.assertIn("dbha-server", services[name]["depends_on"])
            self.assertNotIn("metadata-store", services)
            self.assertNotIn("migrate", services)
            self.assertFalse((out / "seed.sql").exists())
            etcd_command = services["etcd"]["command"]
            self.assertIn("--auto-compaction-retention=1h", etcd_command)
            self.assertIn("--quota-backend-bytes=2147483648", etcd_command)
            self.assertEqual(["CMD", "curl", "-fsS", "http://localhost:8080/readyz"],
                             services["dbha-server"]["healthcheck"]["test"])
            for group in configure.MULTI:
                command = services[group[0] + "-mysql-standby"]["command"]
                self.assertIn("--relay-log=dbha-relay-bin", command)
                self.assertIn("--relay-log-index=dbha-relay-bin.index", command)


if __name__ == "__main__":
    unittest.main()
