#!/usr/bin/env python3
"""Generate five EC2 hosts' native DBHA configuration; never installs or starts anything."""
import argparse
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import sys
import tempfile

HERE = Path(__file__).resolve().parent
LAB = HERE.parent


def generate(inventory, output, enable_switching=False):
    names = ('controller', 'mysql1', 'mysql2', 'proxy1', 'proxy2')
    if set(inventory) != set(names):
        raise ValueError('Inventory must contain exactly: ' + ', '.join(names))
    for address in inventory.values():
        ipaddress.IPv4Address(address)
    if len(set(inventory.values())) != 5:
        raise ValueError('Each EC2 host must have a distinct private IP')
    os.umask(0o077)
    output.mkdir(parents=True, exist_ok=True)
    saved_inventory = output / 'inventory.json'
    if saved_inventory.exists() and json.loads(saved_inventory.read_text()) != inventory:
        raise ValueError('Existing inventory differs; topology changes require a separate migration')
    saved_inventory.write_text(json.dumps(inventory, indent=2) + '\n')
    spec = importlib.util.spec_from_file_location('lab_render', LAB / 'configure.py')
    lab = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(lab)
    with tempfile.TemporaryDirectory() as tmp:
        lab.HERE = Path(tmp)
        secrets = output / 'secrets.env'
        if secrets.exists():
            (lab.HERE / '.env').write_bytes(secrets.read_bytes())
        old_argv = sys.argv
        try:
            sys.argv = ['configure.py'] + (['--enable-switching'] if enable_switching else [])
            lab.main()
        finally:
            sys.argv = old_argv
        secrets.write_bytes((lab.HERE / '.env').read_bytes())
        env = dict(line.split('=', 1) for line in secrets.read_text().splitlines())
        ctrl = inventory['controller']
        mapping = {f'10.203.80.{n}': ctrl for n in (10, 11, 12, 13, 14, 15)}
        mapping.update({f'10.203.80.{n}': inventory[name] for n, name in ((21, 'mysql1'), (22, 'mysql2'), (31, 'proxy1'), (32, 'proxy2'))})

        def translate(text):
            # Replace in a single pass so user IPs cannot be rewritten by a later mapping.
            import re
            return re.sub(r'10\.203\.80\.(?:10|11|12|13|14|15|21|22|31|32)(?!\d)', lambda match: mapping[match.group()], text)

        for name in names:
            (output / name).mkdir(exist_ok=True)
        for service in ('admin', 'receiver', 'analysis'):
            text = translate((lab.HERE / 'generated' / f'{service}.yaml').read_text())
            indent = '      ' if service == 'receiver' else '  '
            old = f'{indent}endpoint: "{ctrl}:3306"\n{indent}user: "root"\n{indent}password: "{env["MYSQL_ROOT_PASSWORD"]}"'
            new = f'{indent}endpoint: "{ctrl}:3306"\n{indent}user: "dbha_store"\n{indent}password: "{env["DBHA_PASSWORD"]}"'
            if text.count(old) != 1:
                raise ValueError('Upstream storage template changed; inspect before rendering')
            text = text.replace(old, new).replace('    user: "root"', '    user: "dbha"')
            text = text.replace('/usr/local/bin/dbha-probe', '/opt/dbha/bin/dbha-probe')
            text = text.replace(f'/run/dbha/{service}.pid', f'/run/dbha-{service}/{service}.pid')
            (output / 'controller' / f'{service}.yaml').write_text(text)
        for name, source in (('mysql1', 'mysql-master'), ('mysql2', 'mysql-standby'), ('proxy1', 'proxy1'), ('proxy2', 'proxy2')):
            data = json.loads(translate((lab.HERE / 'generated' / f'{source}-probe.yaml').read_text()))
            data['pidFile'] = '/run/dbha-probe/probe.pid'
            (output / name / 'probe.yaml').write_text(json.dumps(data, indent=2) + '\n')
        (output / 'controller' / 'metadata.env').write_text(
            f'MYSQL_DSN=dbha_store:{env["DBHA_PASSWORD"]}@tcp({ctrl}:3306)/dbha_metadata?parseTime=true\n'
            f'API_TOKEN={env["API_TOKEN"]}\nLISTEN_ADDR={ctrl}:8080\n')
        for user, password in (('root', env['MYSQL_ROOT_PASSWORD']), ('app', env['APP_PASSWORD'])):
            (output / 'controller' / f'{user}-client.cnf').write_text(f'[client]\nuser={user}\npassword={password}\n')
        (output / 'controller' / 'bootstrap-replication.sql').write_text(
            f"CHANGE MASTER TO MASTER_HOST='{inventory['mysql1']}', MASTER_PORT=3306, MASTER_USER='repl', MASTER_PASSWORD='{env['MYSQL_REPL_PASSWORD']}', MASTER_AUTO_POSITION=1;\nSTART SLAVE;\n")
        seed_dir = lab.MODULE / 'tools/cmd/standalone-metadata'
        for name in ('controller', 'mysql1', 'mysql2'):
            host = output / name
            (host / 'mysql-init').mkdir(exist_ok=True)
            (host / 'mysql.env').write_text(f'MYSQL_ROOT_PASSWORD={env["MYSQL_ROOT_PASSWORD"]}\nMYSQL_ROOT_HOST={ctrl}\n')
            cnf = '[mysqld]\nbind-address=0.0.0.0\n'
            if name == 'controller':
                cnf += 'sql-mode=STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION\n'
                (host / 'mysql-init/10-schema.sql').write_text((seed_dir / 'schema.sql').read_text())
                (host / 'mysql-init/20-seed.sql').write_text(translate((seed_dir / 'seed.sql').read_text()))
                (host / 'mysql-init/30-account.sql').write_text(
                    f"CREATE USER 'dbha_store'@'{ctrl}' IDENTIFIED BY '{env['DBHA_PASSWORD']}';\n"
                    f"GRANT ALL PRIVILEGES ON dbha_metadata.* TO 'dbha_store'@'{ctrl}';\n"
                    f"GRANT ALL PRIVILEGES ON dbha_data.* TO 'dbha_store'@'{ctrl}';\n")
            else:
                cnf += ('server-id=' + ('21' if name == 'mysql1' else '22') + '\nlog-bin=mysql-bin\ngtid-mode=ON\nenforce-gtid-consistency=ON\nlog-slave-updates=ON\nbinlog-format=ROW\nsync-binlog=1\ninnodb-flush-log-at-trx-commit=1\ndefault-authentication-plugin=mysql_native_password\n')
                sql = 'SET SESSION sql_log_bin=0;\nCREATE DATABASE IF NOT EXISTS infodba_schema;\n'
                for user, password, grant in (('dbha', env['DBHA_PASSWORD'], 'ALL PRIVILEGES ON *.*'), ('repl', env['MYSQL_REPL_PASSWORD'], 'REPLICATION SLAVE, REPLICATION CLIENT ON *.*'), ('app', env['APP_PASSWORD'], 'ALL PRIVILEGES ON lab.*')):
                    sql += f"CREATE USER '{user}'@'%' IDENTIFIED WITH mysql_native_password BY '{password}';\nGRANT {grant} TO '{user}'@'%';\n"
                (host / 'mysql-init/10-accounts.sql').write_text(sql)
            (host / 'mysql.cnf').write_text(cnf)
        (output / 'controller' / 'etcd.env').write_text(
            f'ETCD_NAME=controller\nETCD_DATA_DIR=/etcd-data\nETCD_LISTEN_CLIENT_URLS=http://{ctrl}:2379\nETCD_ADVERTISE_CLIENT_URLS=http://{ctrl}:2379\n'
            f'ETCD_LISTEN_PEER_URLS=http://{ctrl}:2380\nETCD_INITIAL_ADVERTISE_PEER_URLS=http://{ctrl}:2380\nETCD_INITIAL_CLUSTER=controller=http://{ctrl}:2380\n')
        for name in ('mysql1', 'mysql2', 'proxy1', 'proxy2'):
            (output / name / 'ssh-password').write_text(env['SSH_PASSWORD'] + '\n')
        for name in ('proxy1', 'proxy2'):
            (output / name / 'proxy.env').write_text(
                f'PROXY_ADMIN_USER=admin\nPROXY_ADMIN_PASSWORD={env["PROXY_ADMIN_PASSWORD"]}\n'
                f'API_TOKEN={env["API_TOKEN"]}\nMETADATA_URL=http://{ctrl}:8080/api/v1/metadata\n')
    print(f'Generated {output}; secrets preserved. Distribute only each host directory, not secrets.env.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inventory', type=Path, required=True)
    parser.add_argument('--output', type=Path, default=HERE / 'generated')
    parser.add_argument('--enable-switching', action='store_true')
    args = parser.parse_args()
    generate(json.loads(args.inventory.read_text()), args.output, args.enable_switching)
