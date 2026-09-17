import json
from pathlib import Path
import re
import tempfile
import unittest

import multicluster


class MultiClusterRenderTest(unittest.TestCase):
    def test_topology_has_three_isolated_clusters(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            multicluster.render_seed(output)
            multicluster.render_compose(output)

            compose = json.loads((output / "compose.json").read_text())
            mysql_services = [name for name in compose["services"] if "-mysql-" in name]
            proxy_services = [name for name in compose["services"] if "-proxy" in name]
            self.assertEqual(6, len(mysql_services))
            self.assertEqual(6, len(proxy_services))

            server_ids = {
                next(arg for arg in compose["services"][name]["command"] if arg.startswith("--server-id="))
                for name in mysql_services
            }
            self.assertEqual(6, len(server_ids))
            self.assertEqual(
                {"research-mysql-1.local", "research-mysql-2.local", "research-mysql-3.local"},
                {compose["services"][name]["environment"]["METADATA_CLUSTER_ADDRESS"] for name in proxy_services},
            )

            seed = (output / "seed.sql").read_text()
            self.assertEqual(12, seed.count("'tendbha'"))
            self.assertEqual(12, len(set(re.findall(r"'10\.203\.81\.([0-9]+)'", seed))))


if __name__ == "__main__":
    unittest.main()
