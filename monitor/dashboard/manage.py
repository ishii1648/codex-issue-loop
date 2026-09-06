#!/usr/bin/env python3
import argparse
import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess


def main():
    parser = argparse.ArgumentParser(description='Independent monitor dashboard LaunchAgents')
    parser.add_argument('action', choices=['prepare', 'start', 'stop', 'restart', 'status'])
    parser.add_argument('--root', type=Path, default=Path.home() / 'Library/Application Support/codex-issue-loop-monitor-dashboard')
    parser.add_argument('--config', type=Path, default=Path.home() / '.agent-loop-monitor.yaml')
    parser.add_argument('--binary', type=Path, default=Path.home() / 'Library/Application Support/codex-issue-loop-monitor/bin/agent-loop-monitor')
    parser.add_argument('--grafana-home', type=Path)
    args = parser.parse_args()
    root = args.root.resolve()
    labels = ['com.codex-issue-loop.monitor-dashboard.' + name for name in ['api', 'prometheus', 'grafana']]
    target = 'gui/' + str(os.getuid())
    if args.action != 'prepare':
        for label in labels:
            plist = root / (label + '.plist')
            loaded = subprocess.run(['launchctl', 'print', target + '/' + label], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
            if args.action == 'stop' and loaded:
                subprocess.run(['launchctl', 'bootout', target + '/' + label], check=True)
            if args.action == 'restart' and loaded:
                subprocess.run(['launchctl', 'kickstart', '-k', target + '/' + label], check=True)
            if args.action in ['start', 'restart'] and not loaded:
                subprocess.run(['launchctl', 'bootstrap', target, str(plist)], check=True)
            if args.action == 'status':
                subprocess.run(['launchctl', 'print', target + '/' + label], check=False)
        return
    os.umask(0o077)
    for directory in ['logs', 'plugins', 'grafana', 'prometheus', 'provisioning/datasources', 'provisioning/dashboards', 'provisioning/plugins', 'provisioning/alerting', 'dashboards']:
        (root / directory).mkdir(parents=True, exist_ok=True)
    grafana = shutil.which('grafana')
    prometheus = shutil.which('prometheus')
    if not grafana or not prometheus or not args.binary.is_file() or not args.config.is_file():
        parser.error('grafana, prometheus, monitor binary and config must exist')
    grafana_home = args.grafana_home or Path(subprocess.check_output(['brew', '--prefix', 'grafana'], text=True).strip()) / 'share/grafana'
    if not (grafana_home / 'conf/defaults.ini').is_file():
        parser.error('--grafana-home must contain conf/defaults.ini')
    source = Path(__file__).resolve().parent
    shutil.copyfile(source / 'dashboard.json', root / 'dashboards/monitor.json')
    (root / 'prometheus.yml').write_text('''global:
  scrape_interval: 15s
  scrape_timeout: 10s
scrape_configs:
  - job_name: agent-loop-monitor
    static_configs:
      - targets: ['127.0.0.1:19110']
''')
    (root / 'provisioning/datasources/monitor.yaml').write_text('''apiVersion: 1
datasources:
  - name: Monitor Prometheus
    uid: monitor-prometheus
    type: prometheus
    access: proxy
    url: http://127.0.0.1:19090
    editable: false
    jsonData:
      timeInterval: 15s
  - name: Monitor History
    uid: monitor-history
    type: yesoreyeram-infinity-datasource
    access: proxy
    editable: false
    jsonData:
      allowedHosts: ['http://127.0.0.1:19110']
''')
    (root / 'provisioning/dashboards/monitor.yaml').write_text('''apiVersion: 1
providers:
  - name: independent-monitor
    folder: Independent Monitor
    type: file
    disableDeletion: true
    editable: false
    options:
      path: ''' + json.dumps(str(root / 'dashboards')) + '\n')
    (root / 'grafana.ini').write_text(f'''[paths]
data = {root / 'grafana'}
logs = {root / 'logs'}
plugins = {root / 'plugins'}
provisioning = {root / 'provisioning'}
[server]
http_addr = 127.0.0.1
http_port = 13000
domain = 127.0.0.1
enforce_domain = true
root_url = http://127.0.0.1:13000/
[security]
allow_embedding = true
[auth.anonymous]
enabled = true
org_role = Viewer
[auth]
disable_login_form = true
[users]
allow_sign_up = false
[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false
[unified_alerting]
enabled = false
[plugins]
preinstall_disabled = true
''')
    commands = [
        [str(args.binary.resolve()), 'serve', '--config', str(args.config.resolve())],
        [prometheus, '--config.file=' + str(root / 'prometheus.yml'), '--storage.tsdb.path=' + str(root / 'prometheus'), '--storage.tsdb.retention.time=30d', '--web.listen-address=127.0.0.1:19090'],
        [grafana, 'server', '--homepath', str(grafana_home), '--config', str(root / 'grafana.ini'), 'cfg:default.paths.logs=' + str(root / 'logs')],
    ]
    for label, command in zip(labels, commands):
        with (root / (label + '.plist')).open('wb') as file:
            plistlib.dump({'Label': label, 'ProgramArguments': command, 'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30, 'ProcessType': 'Background', 'StandardOutPath': str(root / 'logs' / (label + '.out.log')), 'StandardErrorPath': str(root / 'logs' / (label + '.err.log'))}, file)
    print('Prepared:', root)
    print('Install Infinity and official prometheus plugins into', root / 'plugins', 'before starting (see dashboard.md).')


if __name__ == '__main__':
    main()
