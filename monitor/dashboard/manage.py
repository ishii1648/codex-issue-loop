#!/usr/bin/env python3
import argparse
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
    args = parser.parse_args()
    root = args.root.resolve()
    labels = ['com.codex-issue-loop.monitor-dashboard.' + name for name in ['api', 'prometheus']]
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
    for directory in ['logs', 'prometheus']:
        (root / directory).mkdir(parents=True, exist_ok=True)
    prometheus = shutil.which('prometheus')
    if not prometheus or not args.binary.is_file() or not args.config.is_file():
        parser.error('prometheus, monitor binary and config must exist')
    (root / 'prometheus.yml').write_text('''global:
  scrape_interval: 15s
  scrape_timeout: 10s
scrape_configs:
  - job_name: agent-loop-monitor
    static_configs:
      - targets: ['127.0.0.1:19110']
''')
    commands = [
        [str(args.binary.resolve()), 'serve', '--config', str(args.config.resolve())],
        [prometheus, '--config.file=' + str(root / 'prometheus.yml'), '--storage.tsdb.path=' + str(root / 'prometheus'), '--storage.tsdb.retention.time=30d', '--web.listen-address=127.0.0.1:19090'],
    ]
    for label, command in zip(labels, commands):
        with (root / (label + '.plist')).open('wb') as file:
            plistlib.dump({'Label': label, 'ProgramArguments': command, 'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30, 'ProcessType': 'Background', 'StandardOutPath': str(root / 'logs' / (label + '.out.log')), 'StandardErrorPath': str(root / 'logs' / (label + '.err.log'))}, file)
    print('Prepared:', root)


if __name__ == '__main__':
    main()
