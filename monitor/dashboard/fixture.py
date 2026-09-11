#!/usr/bin/env python3
import argparse
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path

parser = argparse.ArgumentParser(description='Create new isolated monitor state; never polls GitHub')
parser.add_argument('directory', type=Path)
parser.add_argument('--case', choices=['mixed', 'healthy', 'down', 'idle', 'unknown', 'stale', 'missing', 'corrupt'], default='mixed')
args = parser.parse_args()
root = args.directory.resolve()
root.mkdir(mode=0o700, parents=True, exist_ok=False)
now = datetime.now(timezone.utc).replace(microsecond=0)
def stamp(t):
    return t.isoformat().replace('+00:00', 'Z')
repos = ['ishii1648/codex-issue-loop', 'ishii1648/zeitreise']
(root / 'config.yaml').write_text('version: 1\npoll_interval: 1m\nobservation_timeout: 3m\nstate_dir: ' + json.dumps(str(root / 'state')) + '\nrepositories:\n' + ''.join('  - name: ' + r + '\n' for r in repos))
for index, repo in enumerate(repos):
    if args.case == 'missing':
        continue
    directory = root / 'state/repositories' / repo.replace('/', '--')
    directory.mkdir(parents=True, mode=0o700)
    observed = now - timedelta(minutes=10) if args.case == 'stale' else now
    status = ['DOWN', 'UNKNOWN'][index] if args.case == 'mixed' else ('HEALTHY' if args.case == 'stale' else args.case.upper())
    if args.case == 'corrupt':
        (directory / 'current.json').write_text('{invalid')
        continue
    states = ['IDLE', 'HEALTHY', 'DOWN', 'UNKNOWN'] if args.case == 'mixed' else [status]*4
    intervals = []
    for i, state in enumerate(states):
        start = observed - timedelta(hours=4-i)
        intervals.append({'decision_version':3,'id': repo+':'+str(i), 'repository': repo, 'status': state, 'started_at': stamp(start), 'ended_at': stamp(start+timedelta(hours=1)), 'reason': 'queue progress deadline exceeded' if state == 'DOWN' else ''})
    (directory / 'intervals.jsonl').write_text(''.join(json.dumps(v)+'\n' for v in intervals))
    (directory / 'current.json').write_text(json.dumps({'schema_version':1,'decision_version':3,'decision_since':stamp(observed-timedelta(hours=4)),'repository':repo,'last_observation_at':stamp(observed),'queue_deadline':stamp(observed+timedelta(minutes=10)) if status=='HEALTHY' else stamp(observed),'queue':([{'number':295,'phase':('ready' if status=='DOWN' else 'running'),'phase_since':stamp(observed-timedelta(hours=1)),'deadline':stamp(observed)}] if status in ['HEALTHY','DOWN'] else []),'current':{'decision_version':3,'id':repo+':current','repository':repo,'status':status,'started_at':stamp(observed),'reason':'queue progress deadline exceeded' if status == 'DOWN' else ''}}))
print(json.dumps({'config':str(root / 'config.yaml'),'at':stamp(now),'from':stamp(now-timedelta(hours=24))}))
