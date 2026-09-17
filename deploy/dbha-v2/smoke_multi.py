#!/usr/bin/env python3
"""Check three isolated clusters; --failover stops their disposable master containers."""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import time
import uuid

import smoke

HERE = Path(__file__).resolve().parent
smoke.COMPOSE = ['docker', 'compose', '--env-file', str(HERE / 'generated-multi/.env'), '-f', str(HERE / 'generated-multi/compose.json')]
compose, sql, eventually = smoke.compose, smoke.sql, smoke.eventually


def ip(group, offset):
    return f'10.203.81.{20 * group + offset}'


def primary(group):
    return sql(f"SELECT ip FROM dbha_metadata.standalone_instances WHERE cluster_id={100+group} AND instance_role='backend_master' AND status IN ('running','available')")


def check_routes(group, expected):
    for offset in (11, 12):
        assert smoke.check_proxy(ip(group, offset), str(expected)), f'cluster {group} proxy {offset} routed elsewhere'
    return True


def check_data(group, replicate=False):
    note = f'cluster-{group}-{uuid.uuid4().hex}'
    service = f'cluster{group}-mysql-standby'
    sql(f"INSERT INTO lab.ha_probe(note) VALUES ('{note}')", service=service, host=ip(group, 11), port=10000, user='app')
    assert sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", service=service, host=ip(group, 12), port=10000, user='app') == '1'
    if replicate:
        eventually(lambda: sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", host=ip(group, 2)) == '1')
    for other in (1, 2, 3):
        if other != group:
            assert sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", host=ip(other, 11), port=10000) == '0', 'Cross-cluster data leak'


def check_metadata():
    for group in (1, 2, 3):
        payload = json.dumps({'bk_cloud_id': 0, 'addresses': [f'research-mysql-{group}.local']})
        raw = compose('exec', '-T', 'metadata-api', 'sh', '-c', 'curl -fsS -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" --data-binary @- http://localhost:8080/api/v1/metadata', input=payload)
        result = json.loads(raw)
        assert result['code'] == 0
        rows = result['data']
        assert len(rows) == 4 and {r['cluster_id'] for r in rows} == {100+group}
        masters = [r for r in rows if r['instance_role'] == 'backend_master']
        assert len(masters) == 1
        master = masters[0]
        assert {p['ip'] for p in master['proxyinstance_set']} == {ip(group, 11), ip(group, 12)}
        assert {r['ip'] for r in master['receiver']} == ({ip(group, 1), ip(group, 2)} - {master['ip']})
    return True


def check_cross_cluster_rejected():
    before = {g: primary(g) for g in (1, 2, 3)}
    payload = json.dumps({'bk_cloud_id': 0, 'payloads': [{'instance1': {'ip': ip(1, 1), 'port': 3306}, 'instance2': {'ip': ip(2, 2), 'port': 3306}}]})
    code = compose('exec', '-T', 'metadata-api', 'sh', '-c', 'curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" --data-binary @- http://localhost:8080/api/v1/swap-mysql-role', input=payload)
    assert code == '409', f'Cross-cluster role swap returned {code}'
    assert before == {g: primary(g) for g in (1, 2, 3)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--failover', action='store_true')
    args = parser.parse_args()
    eventually(lambda: sql('SELECT COUNT(*) FROM dbha_metadata.standalone_instances') == '12')
    eventually(check_metadata)
    check_cross_cluster_rejected()
    before = {g: eventually(lambda g=g: primary(g)) for g in (1, 2, 3)}
    for group, master in before.items():
        expected = int(master.rsplit('.', 1)[1])
        eventually(lambda g=group, e=expected: check_routes(g, e))
    for group, master in before.items():
        check_data(group, replicate=master == ip(group, 1))
    active = int(sql("SELECT COUNT(*) FROM dbha_metadata.standalone_instances WHERE status IN ('running','available')"))
    eventually(lambda: int(sql('SELECT COUNT(DISTINCT db_ip) FROM dbha_data.t_dbha_status WHERE report_timestamp >= UNIX_TIMESTAMP()-30')) == active)
    print('PASS: three metadata scopes, six Proxy routes, isolated writes and fresh metrics; replication checked for groups still on original masters', flush=True)
    if not args.failover:
        return
    assert all(before[g] == ip(g, 1) for g in before), 'Failover test requires original masters; no reset is performed'
    assert 'enableSwitching: true' in (HERE / 'generated-multi/analysis.yaml').read_text(), 'Enable switching before fault injection'
    started = time.monotonic()
    compose('stop', 'cluster1-mysql-master')
    deadline = time.monotonic() + 300
    while primary(1) != ip(1, 2):
        for group in (2, 3):
            assert primary(group) == before[group], 'Unrelated cluster role changed'
            check_routes(group, 20*group+1)
        if time.monotonic() > deadline:
            raise RuntimeError('Cluster 1 failover timed out')
        time.sleep(2)
    for group in (1, 2, 3):
        eventually(lambda g=group: check_routes(g, 20*g+(2 if g == 1 else 1)))
        check_data(group)
    print(f'PASS: cluster 1 failover, clusters 2/3 unchanged ({time.monotonic()-started:.1f}s observed)', flush=True)
    started = time.monotonic()
    compose('stop', 'cluster2-mysql-master', 'cluster3-mysql-master')
    eventually(lambda: all(primary(g) == ip(g, 2) for g in (2, 3)), timeout=360)
    for group in (1, 2, 3):
        eventually(lambda g=group: check_routes(g, 20*g+2))
        check_data(group)
        assert sql('SHOW SLAVE STATUS', host=ip(group, 2)) == '', 'New master still has replication configuration'
    print(f'PASS: clusters 2/3 simultaneous fault injection and promotion ({time.monotonic()-started:.1f}s observed)', flush=True)
    # Planned restarts must not be interpreted as new host failures by Analysis.
    subprocess.run([sys.executable, str(HERE / 'multicluster.py')], check=True)
    compose('up', '-d', '--no-deps', '--force-recreate', 'analysis')
    compose('restart', *(f'cluster{g}-proxy{p}' for g in (1, 2, 3) for p in (1, 2)))
    for group in (1, 2, 3):
        eventually(lambda g=group: check_routes(g, 20*g+2))
        check_data(group)
    check_metadata()
    assert sql("SELECT COUNT(*) FROM dbha_metadata.standalone_instances WHERE machine_type='proxy' AND status IN ('running','available')") == '6'
    print('PASS: all six Proxy restarts retain their own new master; isolated application writes pass', flush=True)
    print('All three original masters remain stopped; automatic switching is disabled.', flush=True)


if __name__ == '__main__':
    main()
