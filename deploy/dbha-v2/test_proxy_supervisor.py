import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch


HERE = Path(__file__).resolve().parent


class SupervisorRecoveryTest(unittest.TestCase):
    def load(self, directory, server_urls=None):
        config = directory / "discovery.json"
        value = {"proxy": {"uuid": "p"}, "server_url": "https://localhost",
                 "token_file": str(directory / "token"),
                 "ca_file": str(directory / "unused-ca"),
                 "route_reconcile_signal_file": str(directory / "signal")}
        if server_urls is not None:
            value["server_urls"] = server_urls
        config.write_text(json.dumps(value))
        (directory / "token").write_text("secret\n")
        spec = importlib.util.spec_from_file_location("proxy_supervisor_test", HERE / "nodes/proxy-supervisor.py")
        module = importlib.util.module_from_spec(spec)
        with patch.dict("os.environ", {"DBHA_DISCOVERY_CONFIG": str(config)}), \
             patch("ssl.create_default_context"):
            spec.loader.exec_module(module)
        module.INTENT = directory / "intent.json"
        module.PIDFILE = directory / "proxy.pid"
        return module

    def test_call_fails_over_and_caches_successful_server(self):
        with tempfile.TemporaryDirectory() as tmp:
            supervisor = self.load(Path(tmp), ["http://one", "http://two"])

            class Response:
                def __enter__(self):
                    return self

                def __exit__(self, *_):
                    return False

                def read(self):
                    return b'{"data":{"ok":true}}'

            unavailable = urllib.error.HTTPError("http://one/test", 503, "standby", {}, None)
            unavailable.read = lambda: b'{"error":{"code":"NOT_LEADER"}}'
            with patch.object(supervisor.urllib.request, "urlopen", side_effect=[unavailable, Response(), Response()]) as request:
                self.assertEqual({"ok": True}, supervisor.call("GET", "/test"))
                self.assertEqual({"ok": True}, supervisor.call("GET", "/test"))
            self.assertEqual(["http://one/test", "http://two/test", "http://two/test"],
                             [entry.args[0].full_url for entry in request.call_args_list])

    def test_call_does_not_retry_non_leader_independent_http_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            supervisor = self.load(Path(tmp), ["http://one", "http://two"])
            denied = urllib.error.HTTPError("http://one/test", 503, "storage unavailable", {}, None)
            denied.read = lambda: b'{"error":{"code":"ETCD_NOSPACE"}}'
            with patch.object(supervisor.urllib.request, "urlopen", side_effect=denied) as request:
                with self.assertRaises(urllib.error.HTTPError):
                    supervisor.call("POST", "/test", {"value": 1})
            request.assert_called_once()

    def test_post_response_loss_reuses_request_id_then_recovers_after_restart(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            supervisor = self.load(directory)
            seen = []

            def lost_response(method, path, payload=None):
                seen.append(payload["request_id"])
                if len(seen) == 1:
                    raise TimeoutError("server accepted permit but response was lost")
                return {"permit_id": "same-server-permit"}

            with patch.object(supervisor, "call", side_effect=lost_response):
                with self.assertRaises(TimeoutError):
                    supervisor.claim_permit("/permits")
                intent = json.loads(supervisor.INTENT.read_text())
                self.assertEqual(intent["request_id"], seen[0])
                self.assertEqual(0o600, supervisor.INTENT.stat().st_mode & 0o777)
                self.assertEqual("same-server-permit", supervisor.claim_permit("/permits")["permit_id"])
            self.assertEqual([seen[0], seen[0]], seen)

            restarted = self.load(directory)
            with patch.object(restarted, "call", return_value={"complete": True}) as call:
                restarted.recover_intent("/permits")
            call.assert_called_once_with("POST", "/permits/same-server-permit/complete", {"stopped": True})
            self.assertFalse(restarted.INTENT.exists())

    def test_pid_file_is_kept_if_sigkill_does_not_stop_process(self):
        with tempfile.TemporaryDirectory() as tmp:
            supervisor = self.load(Path(tmp))
            supervisor.PIDFILE.write_text("123\n")
            with patch.object(supervisor, "active", return_value=True), \
                 patch.object(supervisor.os, "kill"), patch.object(supervisor.time, "sleep"):
                with self.assertRaisesRegex(RuntimeError, "retaining PID file"):
                    supervisor.stop_proxy(123)
            self.assertTrue(supervisor.PIDFILE.exists())


if __name__ == "__main__":
    unittest.main()
