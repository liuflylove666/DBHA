import json
import io
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import configure
import install
import migration_agent_map


class InstallerTest(unittest.TestCase):
    def test_offline_migration_credentials_are_reused(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            ids = configure.render(out)
            mapping = migration_agent_map.generate(out)
            rows = json.loads(mapping.read_text())
            self.assertEqual(4, len(rows))
            self.assertEqual({ids["agents"][node] for node in configure.node_names(configure.SINGLE[0])},
                             {row["agent_id"] for row in rows})
            first = {row["agent_id"]: Path(row["token_file"]).read_text() for row in rows}
            migration_agent_map.generate(out)
            self.assertEqual(first, {row["agent_id"]: Path(row["token_file"]).read_text() for row in rows})
            with patch.object(install.urllib.request, "urlopen", side_effect=AssertionError("must not call API")):
                install.install(out, "http://127.0.0.1:18080")
            self.assertTrue(all(Path(row["token_file"]).stat().st_mode & 0o077 == 0 for row in rows))

    def test_installer_uses_next_server_after_follower_response(self):
        class Response:
            def __init__(self, value):
                self.value = value

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

            def read(self):
                return json.dumps(self.value).encode()

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            ids = configure.render(out)
            calls = []

            def request(req, **_):
                calls.append(req.full_url)
                if req.full_url.startswith("http://follower"):
                    body = io.BytesIO(b'{"error":{"code":"NOT_LEADER"}}')
                    raise urllib.error.HTTPError(req.full_url, 503, "not leader", {}, body)
                body = json.loads(req.data)
                if req.full_url.endswith("/api/v1/deployments"):
                    return Response({"data": {"id": "deployment-1", "cluster_id": 1}})
                return Response({"data": {"agent_id": body["agent_id"], "token": "agent-token"}})

            with patch.object(install.urllib.request, "urlopen", side_effect=request):
                install.install(out, ["http://follower", "http://leader"])
            self.assertEqual(1, sum(url.startswith("http://follower") for url in calls))
            self.assertTrue(all((out / (node + "-agent.token")).exists()
                                for node in configure.node_names(configure.SINGLE[0])))

    def test_installer_does_not_hide_business_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            configure.render(out)
            calls = []

            def request(req, **_):
                calls.append(req.full_url)
                body = io.BytesIO(b'{"error":{"code":"ETCD_NOSPACE"}}')
                raise urllib.error.HTTPError(req.full_url, 503, "storage unavailable", {}, body)

            with patch.object(install.urllib.request, "urlopen", side_effect=request):
                with self.assertRaises(urllib.error.HTTPError):
                    install.install(out, ["http://first", "http://second"])
            self.assertEqual(1, len(calls))


if __name__ == "__main__":
    unittest.main()
