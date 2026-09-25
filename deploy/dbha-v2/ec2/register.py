#!/usr/bin/env python3
"""Call the control API installer then place each node's private token for distribution."""
import argparse
import importlib.util
from pathlib import Path
import shutil
import sys

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
SPEC = importlib.util.spec_from_file_location("lab_install", HERE.parent / "install.py")
installer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(installer)
NODES = {"mysql1": "lab-mysql-master", "mysql2": "lab-mysql-standby",
         "proxy1": "lab-proxy1", "proxy2": "lab-proxy2"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=HERE / "generated")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--server")
    group.add_argument("--servers", nargs="+")
    args = parser.parse_args()
    source = args.output / "_generated"
    installer.install(source, args.servers or args.server)
    for host, node in NODES.items():
        target = args.output / host / "agent.token"
        shutil.copy2(source / (node + "-agent.token"), target)
        target.chmod(0o600)
