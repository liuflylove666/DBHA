import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('ec2_configure', HERE / 'configure.py')
config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(config)


class ConfigurationTest(unittest.TestCase):
    def test_native_paths_addresses_and_secret_reuse(self):
        inventory = json.loads((HERE / 'inventory.example.json').read_text())
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp)
            config.generate(inventory, output)
            secret = (output / 'secrets.env').read_bytes()
            analysis = (output / 'controller/analysis.yaml').read_text()
            self.assertIn('enableSwitching: false', analysis)
            self.assertIn('user: "dbha_store"', analysis)
            self.assertIn('    user: "dbha"', analysis)
            self.assertIn('/opt/dbha/bin/dbha-probe health', analysis)
            self.assertIn('/run/dbha-analysis/analysis.pid', analysis)
            for path in output.rglob('*'):
                if path.is_file():
                    self.assertNotIn('10.203.80.', path.read_text())
                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            proxy = json.loads((output / 'proxy2/probe.yaml').read_text())
            self.assertEqual(proxy['reporter']['endpoint'], inventory['controller'] + ':50052')
            self.assertEqual(proxy['harvester']['mysql']['endpoints'][0]['ip'], inventory['proxy2'])
            config.generate(inventory, output, enable_switching=True)
            self.assertEqual(secret, (output / 'secrets.env').read_bytes())
            self.assertIn('enableSwitching: true', (output / 'controller/analysis.yaml').read_text())
            inventory['mysql1'] = '10.80.10.99'
            with self.assertRaisesRegex(ValueError, 'inventory differs'):
                config.generate(inventory, output)

    def test_reject_duplicate_hosts(self):
        inventory = json.loads((HERE / 'inventory.example.json').read_text())
        inventory['mysql2'] = inventory['mysql1']
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, 'distinct'):
                config.generate(inventory, Path(tmp))


if __name__ == '__main__':
    unittest.main()
