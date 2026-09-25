#!/usr/bin/env python3
"""Supervise only this container's Proxy PID and obey server start permits."""
import ipaddress
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import uuid

CONFIG = json.loads(Path(os.environ.get("DBHA_DISCOVERY_CONFIG", "/etc/dbha/discovery.json")).read_text())
INSTANCE = "proxy:" + CONFIG["proxy"]["uuid"]
CONFIGURED_APIS = CONFIG.get("server_urls") or [CONFIG["server_url"]]
if not isinstance(CONFIGURED_APIS, list):
    raise ValueError("server_urls must be an array")
APIS = [value.rstrip("/") for value in CONFIGURED_APIS]
if not APIS or any(not value for value in APIS) or len(set(APIS)) != len(APIS):
    raise ValueError("server_urls must contain at least one address")
SSL = ssl.create_default_context(cafile=CONFIG.get("ca_file")) if any(value.startswith("https://") for value in APIS) else None
PREFERRED_API = 0
PIDFILE = Path("/run/dbha/mysql-proxy.pid")
SIGNAL = Path(CONFIG["route_reconcile_signal_file"])
INTENT = Path("/var/lib/dbha-probe/proxy-start-intent.json")
STOP = False


def stopping(*_):
    global STOP
    STOP = True


signal.signal(signal.SIGTERM, stopping)
signal.signal(signal.SIGINT, stopping)


def call(method, path, payload=None):
    global PREFERRED_API
    token = Path(CONFIG["token_file"]).read_text().strip()
    body = None if payload is None else json.dumps(payload).encode()
    last_error = None
    for offset in range(len(APIS)):
        index = (PREFERRED_API + offset) % len(APIS)
        req = urllib.request.Request(APIS[index] + path, data=body, method=method,
                                     headers={"Authorization": "Bearer " + token,
                                              "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, context=SSL, timeout=5) as response:
                result = json.load(response)["data"]
            PREFERRED_API = index
            return result
        except urllib.error.HTTPError as exc:
            response = exc.read()
            try:
                code = json.loads(response).get("error", {}).get("code")
            except (json.JSONDecodeError, UnicodeDecodeError):
                code = None
            if code != "NOT_LEADER":
                raise
            last_error = exc
        except (urllib.error.URLError, TimeoutError, ConnectionError, OSError) as exc:
            last_error = exc
    raise last_error


def active(pid):
    if not pid:
        return False
    try:
        os.kill(pid, 0)
        return Path(f"/proc/{pid}/stat").read_text().split()[2] != "Z"
    except (ProcessLookupError, FileNotFoundError):
        return False


def stop_proxy(pid):
    if active(pid):
        os.kill(pid, signal.SIGTERM)
        for _ in range(100):
            if not active(pid):
                break
            time.sleep(.1)
    if active(pid):
        os.kill(pid, signal.SIGKILL)
        for _ in range(50):
            if not active(pid):
                break
            time.sleep(.1)
    if active(pid):
        raise RuntimeError("Proxy PID remains alive after stop; retaining PID file")
    PIDFILE.unlink(missing_ok=True)


def save_intent(value):
    INTENT.parent.mkdir(parents=True, exist_ok=True)
    temp = INTENT.with_name(INTENT.name + ".tmp")
    with temp.open("w") as handle:
        json.dump(value, handle)
        handle.flush()
        os.fsync(handle.fileno())
    temp.chmod(0o600)
    temp.replace(INTENT)
    descriptor = os.open(INTENT.parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def clear_intent():
    INTENT.unlink(missing_ok=True)
    descriptor = os.open(INTENT.parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def claim_permit(path):
    intent = json.loads(INTENT.read_text()) if INTENT.exists() else {"request_id": str(uuid.uuid4())}
    if not INTENT.exists():
        save_intent(intent)  # Durable before an external POST can take effect.
    if "permit_id" not in intent:
        permit = call("POST", path, {"request_id": intent["request_id"]})
        intent["permit_id"] = permit["permit_id"]
        save_intent(intent)
    return intent


def recover_intent(path):
    if not INTENT.exists():
        return
    old = json.loads(INTENT.read_text())
    if old.get("permit_id"):
        call("POST", path + "/" + old["permit_id"] + "/complete", {"stopped": True})
        clear_intent()


def backend_address(value):
    # Never interpolate arbitrary server text into the vendor configuration.
    host, port = value["host"], value["port"]
    ipaddress.ip_address(host)
    port = int(port)
    if not 1 <= port <= 65535:
        raise ValueError("invalid backend port")
    return f"{host}:{port}"


def write_config(backend):
    user, password = os.environ["PROXY_ADMIN_USER"], os.environ["PROXY_ADMIN_PASSWORD"]
    for value in (user, password):
        if not value.isalnum():
            raise ValueError("invalid Proxy admin credential")
    Path("/run/dbha/proxy-users.cnf").write_text("app@%\nroot@%\ndbha@%\n")
    content = f"""[mysql-proxy]
basedir = /opt/mysql-proxy
plugin-dir = /opt/mysql-proxy/lib/mysql-proxy/plugins
admin-lua-script = /opt/mysql-proxy/lib/mysql-proxy/lua/admin.lua
admin-users-file = /run/dbha/proxy-users.cnf
proxy-address = 0.0.0.0:10000
admin-address = 0.0.0.0:11000
admin-username = {user}
admin-password = {password}
proxy-backend-addresses = {backend}
pid-file = /run/dbha/mysql-proxy.pid
daemon = true
log-file = /var/log/dbha/mysql-proxy.log
log-level = info
plugins = proxy,admin
"""
    path = Path("/run/dbha/mysql-proxy.cnf")
    path.write_text(content)
    path.chmod(0o600)


def main():
    pid = None
    path = f"/api/v1/proxies/{INSTANCE}/start-permits"
    while not STOP:
        if pid and active(pid):
            if SIGNAL.exists():
                print("route reconciliation requested; stopping controlled Proxy", flush=True)
                stop_proxy(pid)
                pid = None
                SIGNAL.unlink(missing_ok=True)
            else:
                time.sleep(1)
                continue
        if pid:
            print("Proxy exited; reacquiring start permit", flush=True)
            pid = None
        try:
            # A prior process may have exited after claiming a permit. Cancel
            # only after the server independently verifies the local stop.
            recover_intent(path)
            # A stopped Proxy cannot retain an old reconcile request.
            SIGNAL.unlink(missing_ok=True)
            intent = claim_permit(path)
            permit_id = intent["permit_id"]
            verified = call("GET", path + "/" + permit_id)
            if verified["permit_id"] != permit_id:
                raise ValueError("permit changed before startup")
            backend = backend_address(verified["current_primary"])
            write_config(backend)
            PIDFILE.unlink(missing_ok=True)
            subprocess.run(["/opt/mysql-proxy/bin/mysql-proxy", "--defaults-file=/run/dbha/mysql-proxy.cnf"], check=True)
            for _ in range(50):
                if PIDFILE.exists():
                    break
                time.sleep(.1)
            pid = int(PIDFILE.read_text().strip())
            if not active(pid):
                raise RuntimeError("Proxy failed to start")
            # Complete acknowledgement may fail during a network partition.
            # Keep the existing route alive; retry until the server confirms.
            while active(pid) and not STOP and not SIGNAL.exists():
                try:
                    call("POST", path + "/" + permit_id + "/complete", {"stopped": False})
                    clear_intent()
                    break
                except Exception as exc:
                    print(f"permit completion pending: {exc}", flush=True)
                    time.sleep(2)
        except Exception as exc:
            print(f"waiting for DBHA start permit: {exc}", flush=True)
            time.sleep(2)
    stop_proxy(pid)


if __name__ == "__main__":
    main()
