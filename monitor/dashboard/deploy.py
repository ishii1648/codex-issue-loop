import contextlib
from datetime import datetime, timedelta, timezone
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import plistlib
import re
import socket
import subprocess
import tempfile
import time
from urllib.request import urlopen


REPOSITORY = 'ishii1648/codex-issue-loop'
ASSET = 'agent-loop-monitor_Darwin_arm64'
LABELS = ['com.codex-issue-loop.monitor', 'com.codex-issue-loop.monitor-dashboard.api']


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def command(*args):
    result = subprocess.run([str(a) for a in args], capture_output=True, timeout=120)
    require(result.returncode == 0, 'command failed: ' + Path(str(args[0])).name + ' ' + str(args[1]))
    return result.stdout.decode()


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def atomic(path, data, mode=0o600):
    path = Path(path)
    fd, name = tempfile.mkstemp(prefix='.deploy-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def fetch(url, as_json=True):
    with urlopen(url, timeout=5) as response:
        data = response.read(16 * 1024 * 1024)
    return json.loads(data) if as_json else data


def wait_for(check, seconds=120):
    deadline = time.monotonic() + seconds
    while True:
        try:
            return check()
        except (RuntimeError, OSError, ValueError, KeyError, IndexError) as error:
            if time.monotonic() >= deadline:
                raise RuntimeError('verification timeout: ' + str(error)) from error
            time.sleep(1)


def version(binary):
    return json.loads(command(binary, 'version', '--json'))


def verify_release(directory, tag, commit):
    release = json.loads(command('gh', 'release', 'view', tag, '--repo', REPOSITORY,
                                 '--json', 'tagName,isDraft,isPrerelease'))
    require(release == {'tagName': tag, 'isDraft': False, 'isPrerelease': False}, 'release is not stable')
    runs = json.loads(command('gh', 'run', 'list', '--repo', REPOSITORY, '--workflow', 'release.yml',
                              '--commit', commit, '--json', 'headSha,headBranch,status,conclusion'))
    require(any(run['headSha'] == commit and run['headBranch'] == tag and run['status'] == 'completed' and
                run['conclusion'] == 'success' for run in runs), 'no successful release workflow for tag/commit')
    ref = json.loads(command('gh', 'api', 'repos/' + REPOSITORY + '/git/ref/tags/' + tag))['object']
    require(ref['type'] == 'tag', 'annotated tag required')
    tagged = json.loads(command('gh', 'api', 'repos/' + REPOSITORY + '/git/tags/' + ref['sha']))
    require(tagged['tag'] == tag and tagged['object']['type'] == 'commit' and tagged['object']['sha'] == commit,
            'tag commit mismatch')
    command('gh', 'release', 'download', tag, '--repo', REPOSITORY, '--dir', directory,
            '--pattern', ASSET, '--pattern', 'checksums.txt', '--pattern', 'release-manifest.json')
    for asset in [ASSET, 'checksums.txt', 'release-manifest.json']:
        command('gh', 'attestation', 'verify', directory / asset, '--repo', REPOSITORY,
                '--signer-workflow', REPOSITORY + '/.github/workflows/release.yml',
                '--source-ref', 'refs/tags/' + tag, '--source-digest', commit, '--deny-self-hosted-runners')
    checksums = {}
    for line in (directory / 'checksums.txt').read_text().splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  ([A-Za-z0-9_.-]+)', line)
        require(match is not None, 'invalid checksum line')
        require(match[2] not in checksums, 'duplicate checksum')
        checksums[match[2]] = match[1]
    for asset in [ASSET, 'release-manifest.json']:
        require(checksums.get(asset) == digest(directory / asset), 'checksum mismatch: ' + asset)
    manifest = json.loads((directory / 'release-manifest.json').read_text())
    require(manifest['version'] == tag and manifest['commit'] == commit, 'manifest identity mismatch')
    binary = directory / ASSET
    binary.chmod(0o700)
    info = version(binary)
    require(info.get('version') == tag and info.get('commit') == commit and
            info.get('target') == 'darwin/arm64' and info.get('monitor_schema_version') == 1,
            'monitor identity/schema mismatch')
    return info


def loaded_arguments(output, service):
    loaded_path = re.search(r'^\s*path = (.+)$', output, re.M)
    require(loaded_path and Path(loaded_path[1].strip()).resolve() == Path(service['plist']),
            'loaded plist path differs: ' + service['label'])
    program = re.search(r'^\s*program = (.+)$', output, re.M)
    arguments = re.search(r'^\s*arguments = \{\n(.*?)\n\s*\}', output, re.M | re.S)
    require(program and arguments, 'cannot read loaded arguments: ' + service['label'])
    require(program[1].strip() == service['args'][0] and
            [line.strip() for line in arguments[1].splitlines()] == service['args'],
            'loaded service arguments differ: ' + service['label'])


def inspect(service, target):
    output = command('launchctl', 'print', target + '/' + service['label'])
    loaded_arguments(output, service)
    pid = re.search(r'^\s*pid = (\d+)\s*$', output, re.M)
    require(pid, 'service is not running: ' + service['label'])
    pid = int(pid[1])
    require(command('ps', '-p', pid, '-o', 'uid=').strip() == str(os.getuid()), 'service owner mismatch')
    files = command('lsof', '-a', '-p', pid, '-d', 'txt', '-Fni').splitlines()
    require('n' + service['binary'] in files, 'running executable path mismatch')
    index = files.index('n' + service['binary'])
    require(index > 0 and files[index - 1] == 'i' + str(Path(service['binary']).stat().st_ino),
            'running executable inode mismatch')
    if service['args'][1] == 'serve':
        require(command('lsof', '-a', '-p', pid, '-iTCP:19110', '-sTCP:LISTEN', '-t').strip() == str(pid),
                'dashboard listener does not belong to the service PID')
    return pid


def discover(root, config):
    services = []
    paths = [Path.home() / 'Library/LaunchAgents' / (LABELS[0] + '.plist'), root / (LABELS[1] + '.plist')]
    for path, label, action in zip(paths, LABELS, ['run', 'serve']):
        path = path.resolve(strict=True)
        plist = plistlib.loads(path.read_bytes())
        args = plist['ProgramArguments']
        require(plist['Label'] == label and len(args) >= 4 and args[1] == action, 'unexpected service plist')
        require('Program' not in plist or plist['Program'] == args[0], 'Program overrides binary')
        require(args.count('--config') == 1 and Path(args[args.index('--config') + 1]).resolve() == config,
                'both services must use the specified config')
        require(Path(args[0]).is_absolute(), 'absolute executable path required')
        require('--listen' not in args or args[args.index('--listen') + 1] == '127.0.0.1:19110',
                'deployment supports the existing dashboard endpoint only')
        services.append({'label': label, 'plist': str(path), 'args': args,
                         'binary': str(Path(args[0]).resolve(strict=True)), 'plist_sha256': digest(path)})
    return services


@contextlib.contextmanager
def candidate_page(binary, config):
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    process = subprocess.Popen([str(binary), 'serve', '--config', str(config), '--listen', '127.0.0.1:' + str(port)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        def read():
            require(process.poll() is None, 'candidate serve exited')
            return fetch('http://127.0.0.1:' + str(port) + '/', False)
        yield wait_for(read, 15)
    finally:
        if process.poll() is None:
            process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def compatible(binary, config):
    for operation in ['status', 'history', 'report']:
        args = [binary, operation, '--config', config, '--json']
        if operation == 'report':
            args += ['--from', '1970-01-01T00:00:00Z']
        json.loads(command(*args))


class Deployment:
    def __init__(self, directory, config, target):
        self.directory, self.config, self.target = directory, config, target
        self.path = directory / 'deployment.json'
        self.record = json.loads(self.path.read_text()) if self.path.exists() else None

    def save(self, phase):
        self.record['phase'] = phase
        atomic(self.path, json.dumps(self.record, indent=2).encode())
        print(json.dumps({'tag': self.record['tag'], 'commit': self.record['commit'], 'phase': phase}), flush=True)

    def unchanged(self):
        require(digest(self.config) == self.record['config_sha256'], 'config changed; manual reconciliation required')
        for service in self.record['services']:
            require(digest(service['plist']) == service['plist_sha256'], 'plist changed; manual reconciliation required')
            require(str(Path(service['args'][0]).resolve()) == service['binary'], 'binary symlink changed')

    def stop(self):
        for service in self.record['services']:
            result = subprocess.run(['launchctl', 'print', self.target + '/' + service['label']], capture_output=True)
            if result.returncode:
                require(b'Could not find service' in result.stderr + result.stdout, 'cannot inspect service before stop')
                continue
            loaded_arguments(result.stdout.decode(), service)
            pid = re.search(rb'^\s*pid = (\d+)\s*$', result.stdout, re.M)
            command('launchctl', 'bootout', self.target + '/' + service['label'])
            if pid:
                def stopped():
                    probe = subprocess.run(['ps', '-p', pid[1].decode(), '-o', 'pid='], capture_output=True)
                    require(probe.returncode == 1, 'old process still running')
                wait_for(stopped, 30)

    def health(self, rollback=False):
        self.unchanged()
        pids = {}
        for service in self.record['services']:
            expected = service['old_sha256'] if rollback else self.record['sha256']
            require(digest(service['binary']) == expected, 'service binary digest mismatch')
            pids[service['label']] = inspect(service, self.target)
            require(pids[service['label']] != service['initial_pid'], 'service was not restarted')
        expected_page = (self.directory / ('old.html' if rollback else 'candidate.html')).read_bytes()
        require(fetch('http://127.0.0.1:19110/', False) == expected_page, 'served HTML mismatch')
        status = fetch('http://127.0.0.1:19110/api/status')['repositories']
        require(bool(status), 'no monitored repositories')
        for snapshot in status:
            observed = datetime.fromisoformat(snapshot['last_observation_at'].replace('Z', '+00:00')).timestamp()
            require(observed > self.record['started_at'] and 0 <= time.time() - observed < 180 and not snapshot.get('last_error'), 'observation has not succeeded after restart')
        now = datetime.now(timezone.utc)
        query = '?from=' + (now - timedelta(hours=24)).strftime('%Y-%m-%dT%H:%M:%SZ') + '&to=' + now.strftime('%Y-%m-%dT%H:%M:%SZ')
        reports = fetch('http://127.0.0.1:19110/api/report' + query)['reports']
        timelines = fetch('http://127.0.0.1:19110/api/timeline' + query)['repositories']
        names = {s['repository'] for s in status}
        require({r['repository'] for r in reports} == names and set(timelines) == names, 'API repository mismatch')
        freshness = fetch('http://127.0.0.1:19110/api/freshness')
        require(freshness['status'] == 'success' and len(freshness['data']['result']) == 1, 'Prometheus scrape missing')
        reference = float(freshness['data']['result'][0]['value'][1])
        require(math.isfinite(reference) and 0 <= time.time() - reference < 45 and reference > self.record['started_at'], 'Prometheus scrape is stale')
        return pids

    def apply(self, rollback=False):
        self.unchanged()
        for service in self.record['services']:
            require(digest(service['binary']) in [service['old_sha256'], self.record['sha256']],
                    'installed binary changed outside this deployment')
        self.save('rollback-stopping' if rollback else 'stopping')
        self.stop()
        self.save('rollback-compatibility' if rollback else 'compatibility')
        for service in self.record['services']:
            old = self.directory / service['backup']
            require(digest(old) == service['old_sha256'], 'backup digest mismatch')
            compatible(old, self.config)
        self.save('rollback-installing' if rollback else 'installing')
        for service in self.record['services']:
            source = self.directory / (service['backup'] if rollback else ASSET)
            expected = service['old_sha256'] if rollback else self.record['sha256']
            require(digest(source) == expected, 'installation source changed')
            atomic(service['binary'], source.read_bytes(), service['mode'])
            if rollback:
                backup = self.directory / service['plist_backup']
                require(digest(backup) == service['plist_sha256'], 'plist backup changed')
                atomic(service['plist'], backup.read_bytes(), service['plist_mode'])
        self.record['started_at'] = time.time()
        self.save('rollback-starting' if rollback else 'starting')
        for service in self.record['services']:
            command('launchctl', 'bootstrap', self.target, service['plist'])
        self.save('rollback-health' if rollback else 'health')
        self.record['pids'] = wait_for(lambda: self.health(rollback), 180)
        self.save('rolled-back' if rollback else 'complete')


def main(args):
    require(platform.system() == 'Darwin' and platform.machine() == 'arm64', 'Darwin arm64 host required')
    require(args.tag and re.fullmatch(r'v\d+\.\d+\.\d+', args.tag) and args.commit and
            re.fullmatch(r'[0-9a-f]{40}', args.commit), '--tag stable tag and --commit full SHA are required')
    os.umask(0o077)
    config, root = args.config.resolve(strict=True), args.root.resolve(strict=True)
    services = discover(root, config)
    binary = Path(services[0]['binary'])
    require(binary.parent.name == 'bin' and binary.name == 'agent-loop-monitor', 'run must reference its installed state-root/bin/agent-loop-monitor')
    directory = binary.parent.parent / 'deployments'
    directory.mkdir(mode=0o700, exist_ok=True)
    with (directory / 'deploy.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        selected = args.tag + '-' + args.commit
        for journal in directory.glob('*/deployment.json'):
            if journal.parent.name != selected:
                require(json.loads(journal.read_text())['phase'] in ['complete', 'rolled-back'],
                        'another deployment is unfinished: ' + journal.parent.name)
        directory = directory / selected
        directory.mkdir(mode=0o700, exist_ok=True)
        deployment = Deployment(directory, config, 'gui/' + str(os.getuid()))
        try:
            if deployment.record is None:
                require(args.action == 'deploy', 'no recorded deployment to roll back')
                print(json.dumps({'tag': args.tag, 'commit': args.commit, 'phase': 'release-verification'}), flush=True)
                with tempfile.TemporaryDirectory(dir=directory) as temporary:
                    candidate = Path(temporary)
                    info = verify_release(candidate, args.tag, args.commit)
                    for service in services:
                        service['initial_pid'] = inspect(service, deployment.target)
                        service['old_version'] = version(service['binary'])
                        require(service['old_version'].get('monitor_schema_version') == 1, 'old binary is not a supported monitor')
                        compatible(service['binary'], config)
                        service['old_sha256'] = digest(service['binary'])
                        service['mode'] = Path(service['binary']).stat().st_mode & 0o777
                        service['plist_mode'] = Path(service['plist']).stat().st_mode & 0o777
                        service['backup'] = service['label'] + '.binary'
                        service['plist_backup'] = service['label'] + '.plist'
                        atomic(directory / service['backup'], Path(service['binary']).read_bytes(), 0o700)
                        atomic(directory / service['plist_backup'], Path(service['plist']).read_bytes())
                    with candidate_page(candidate / ASSET, config) as page:
                        atomic(directory / 'candidate.html', page)
                    with candidate_page(directory / services[1]['backup'], config) as page:
                        atomic(directory / 'old.html', page)
                    atomic(directory / ASSET, (candidate / ASSET).read_bytes(), 0o700)
                    deployment.record = {'tag': args.tag, 'commit': args.commit, 'version': info,
                                         'sha256': digest(directory / ASSET), 'config_sha256': digest(config),
                                         'services': services}
                    deployment.save('prepared')
            require(deployment.record['tag'] == args.tag and deployment.record['commit'] == args.commit, 'record identity mismatch')
            require(services == [{k: s[k] for k in services[0]} for s in deployment.record['services']], 'service references changed')
            rollback = args.action == 'rollback'
            phase = deployment.record['phase']
            require(rollback or not (phase.startswith('rollback-') or phase == 'rolled-back'), 'rollback already selected; finish rollback before a new release')
            if phase == ('rolled-back' if rollback else 'complete'):
                deployment.health(rollback)
                deployment.save(phase)
                return
            deployment.apply(rollback)
        except Exception:
            print(json.dumps({'tag': args.tag, 'commit': args.commit, 'failed_phase':
                              deployment.record['phase'] if deployment.record else 'release-verification',
                              'record': str(deployment.path)}), flush=True)
            raise
