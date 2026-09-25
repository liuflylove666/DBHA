import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("ec2_configure", HERE / "configure.py")
config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(config)


class ConfigurationTest(unittest.TestCase):
    def test_native_render_and_stable_identity(self):
        inventory = json.loads((HERE / "inventory.example.json").read_text())
        inventory.pop("controllers", None)
        host_keys = {name: inventory[name] + " ssh-ed25519 AAAATEST" for name in ("mysql1", "mysql2", "proxy1", "proxy2")}
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp)
            first = config.generate(inventory, output, host_keys)
            token = (output / "_generated/admin.token").read_bytes()
            self.assertEqual(first, config.generate(inventory, output, host_keys))
            self.assertEqual(token, (output / "_generated/admin.token").read_bytes())
            server = json.loads((output / "controller/server.json").read_text())
            self.assertEqual("http://" + inventory["controller"] + ":2379", server["etcd_endpoints"][0])
            self.assertEqual("controller1", server["node_id"])
            self.assertEqual("http://" + inventory["controller"] + ":8080", server["advertise_http"])
            self.assertEqual(4, len(server["allowed_networks"]))
            self.assertNotIn("mysql_dsn", server)
            self.assertIn("ssh_known_hosts_file", server["profiles"]["default"])
            for node in ("mysql1", "mysql2", "proxy1", "proxy2"):
                probe = json.loads((output / node / "discovery.json").read_text())
                self.assertEqual(inventory[node], probe["advertise_host"])
                self.assertEqual("http://" + inventory["controller"] + ":8080", probe["server_url"])
                self.assertEqual([probe["server_url"]], probe["server_urls"])
                self.assertEqual([probe["server_grpc"]], probe["server_grpc_endpoints"])
                self.assertNotIn("ca_file", probe)
                self.assertEqual(0o600, (output / node / "discovery.json").stat().st_mode & 0o777)
            self.assertNotIn("tls_cert_file", server)
            self.assertNotIn("tls_key_file", server)
            self.assertFalse((output / "controller/mysql.env").exists())
            inventory["mysql1"] = "10.80.10.99"
            with self.assertRaisesRegex(ValueError, "inventory"):
                config.generate(inventory, output, host_keys)

    def test_duplicate_hosts_rejected(self):
        inventory = json.loads((HERE / "inventory.example.json").read_text())
        inventory["mysql2"] = inventory["mysql1"]
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, "distinct"):
                config.generate(inventory, Path(tmp), {})

    def test_multiple_controller_addresses_are_rendered(self):
        inventory = json.loads((HERE / "inventory.example.json").read_text())
        inventory["controllers"] = [inventory["controller"], "10.80.10.11", "10.80.10.12"]
        host_keys = {name: inventory[name] + " ssh-ed25519 AAAATEST" for name in ("mysql1", "mysql2", "proxy1", "proxy2")}
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp)
            config.generate(inventory, output, host_keys)
            probe = json.loads((output / "proxy1/discovery.json").read_text())
            self.assertEqual([f"http://{host}:8080" for host in inventory["controllers"]], probe["server_urls"])
            self.assertEqual([f"{host}:50052" for host in inventory["controllers"]], probe["server_grpc_endpoints"])
            expected_etcd = [f"http://{host}:2379" for host in inventory["controllers"]]
            expected_cluster = ",".join(f"controller{index}=http://{host}:2380" for index, host in enumerate(inventory["controllers"], 1))
            for index, host in enumerate(inventory["controllers"], 1):
                name = "controller" if index == 1 else f"controller{index}"
                server = json.loads((output / name / "server.json").read_text())
                self.assertEqual(expected_etcd, server["etcd_endpoints"])
                self.assertEqual(f"controller{index}", server["node_id"])
                self.assertEqual(f"http://{host}:8080", server["advertise_http"])
                self.assertEqual(f"{host}:50052", server["advertise_grpc"])
                etcd = (output / name / "etcd.env").read_text()
                self.assertIn(f"ETCD_NAME=controller{index}\n", etcd)
                self.assertIn(f"ETCD_INITIAL_CLUSTER={expected_cluster}\n", etcd)


if __name__ == "__main__":
    unittest.main()
