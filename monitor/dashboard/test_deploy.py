import contextlib
from datetime import datetime, timezone
import json
from pathlib import Path
import plistlib
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

import deploy


class DeployTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name).resolve()
        self.root = self.home / 'dashboard'
        self.root.mkdir()
        self.config = self.home / 'config.yaml'
        self.config.write_text('private config')
        self.binaries = [self.home / 'state/bin/agent-loop-monitor', self.home / 'display/monitor']
        self.plists = [self.home / 'Library/LaunchAgents' / (deploy.LABELS[0] + '.plist'), self.root / (deploy.LABELS[1] + '.plist')]
        for i, (binary, plist) in enumerate(zip(self.binaries, self.plists)):
            binary.parent.mkdir(parents=True)
            binary.write_bytes(b'old' + str(i).encode())
            binary.chmod(0o755)
            plist.parent.mkdir(parents=True, exist_ok=True)
            plist.write_bytes(plistlib.dumps({'Label': deploy.LABELS[i], 'ProgramArguments': [str(binary), ['run', 'serve'][i], '--config', str(self.config)]}))
        self.preserved = [self.config, self.home / 'state/history', self.root / 'prometheus/data'] + self.plists
        for path in self.preserved[1:3]:
            path.parent.mkdir(exist_ok=True, parents=True)
            path.write_bytes(b'history must survive')
        for name in ['agent-loop/bin/agent-loop', 'agent-loop/assignments.json', 'agent-loop/supervisor.plist']:
            path = self.home / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b'unchanged main installation')
            self.preserved.append(path)
        self.original = {p: p.read_bytes() for p in self.preserved}
        self.loaded = dict(zip(deploy.LABELS, [100, 101]))
        self.loaded["com.codex-issue-loop.supervisor"] = 999
        self.pid = 200
        self.commands = []
        self.args = types.SimpleNamespace(root=self.root, config=self.config, tag='monitor-v0.1.0', commit='a' * 40, action='deploy')
        self.fail_bootstrap = False
        self.fail_health = False
        self.fail_compatibility = False
        self.fail_release = False
        for name, replacement in [('platform.system', lambda: 'Darwin'), ('platform.machine', lambda: 'arm64'),
                                  ('Path.home', lambda: self.home), ('verify_release', self.release),
                                  ('inspect', self.inspect), ('compatible', self.compatible),
                                  ('version', lambda p: {'version': 'old', 'monitor_schema_version': 1}), ('candidate_page', self.page),
                                  ('command', self.command), ('subprocess.run', self.run_command),
                                  ('fetch', self.fetch), ('wait_for', lambda f, *a: f())]:
            mock = patch('deploy.' + name, replacement)
            mock.start()
            self.addCleanup(mock.stop)

    @property
    def journal(self):
        return self.home / 'state/deployments' / (self.args.tag + '-' + self.args.commit) / 'deployment.json'

    def release(self, directory, tag, commit):
        if self.fail_release:
            raise RuntimeError('attestation rejected')
        (directory / deploy.ASSET).write_bytes(b'new')
        return {'version': tag, 'commit': commit}

    @contextlib.contextmanager
    def page(self, binary, config):
        yield b'new html' if binary.read_bytes() == b'new' else b'old html'

    def inspect(self, service, target):
        deploy.require(service['label'] in self.loaded, 'not loaded')
        return self.loaded[service['label']]

    def compatible(self, binary, config):
        if self.fail_compatibility:
            raise RuntimeError('unsupported decision version')

    def command(self, *args):
        self.commands.append(args)
        if args[:2] == ('launchctl', 'bootout'):
            del self.loaded[args[2].split('/')[-1]]
        elif args[:2] == ('launchctl', 'bootstrap'):
            if self.fail_bootstrap:
                raise RuntimeError('bootstrap failed')
            label = plistlib.loads(Path(args[3]).read_bytes())['Label']
            self.pid += 1
            self.loaded[label] = self.pid
        return ''

    def run_command(self, args, **kwargs):
        if args[0] == 'ps':
            return subprocess.CompletedProcess(args, 1, b'', b'')
        label = args[2].split('/')[-1]
        if label not in self.loaded:
            return subprocess.CompletedProcess(args, 1, b'', b'Could not find service')
        service = plistlib.loads(self.plists[deploy.LABELS.index(label)].read_bytes())['ProgramArguments']
        output = 'path = ' + str(self.plists[deploy.LABELS.index(label)]) + '\npid = ' + str(self.loaded[label]) + '\nprogram = ' + service[0] + '\narguments = {\n' + '\n'.join(service) + '\n}'
        return subprocess.CompletedProcess(args, 0, output.encode(), b'')

    def fetch(self, url, as_json=True):
        if self.fail_health:
            raise RuntimeError('health failed')
        if url.endswith('/'):
            return b'new html' if self.binaries[1].read_bytes() == b'new' else b'old html'
        if '/api/status' in url:
            return {'repositories': [{'repository': 'owner/repo', 'last_observation_at': datetime.now(timezone.utc).isoformat()}]}
        if '/api/report' in url:
            return {'reports': [{'repository': 'owner/repo'}]}
        if '/api/timeline' in url:
            return {'repositories': {'owner/repo': []}}
        return {'status': 'success', 'data': {'result': [{'value': [0, str(deploy.time.time())]}]}}

    def assert_preserved(self):
        self.assertEqual(self.loaded.get("com.codex-issue-loop.supervisor"), 999)
        for path, data in self.original.items():
            self.assertEqual(path.read_bytes(), data)
        self.assertTrue(all('supervisor' not in str(c) and 'prometheus' not in str(c) for c in self.commands))

    def test_update_two_distinct_binaries_repeat_and_rollback(self):
        deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'new', b'new'])
        self.assertEqual(json.loads(self.journal.read_text())['phase'], 'complete')
        commands = self.commands[:]
        deploy.main(self.args)
        self.assertEqual(self.commands, commands)
        self.args.action = 'rollback'
        deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'old0', b'old1'])
        self.assertEqual(json.loads(self.journal.read_text())['phase'], 'rolled-back')
        deploy.main(self.args)
        self.assert_preserved()

    def test_verification_failure_never_stops_or_installs(self):
        self.fail_release = True
        with self.assertRaisesRegex(RuntimeError, 'attestation'):
            deploy.main(self.args)
        self.assertEqual(self.commands, [])
        self.assertFalse(self.journal.exists())
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'old0', b'old1'])
        self.assert_preserved()

    def test_bootstrap_failure_resumes_without_losing_original_backup(self):
        self.fail_bootstrap = True
        with self.assertRaisesRegex(RuntimeError, 'bootstrap'):
            deploy.main(self.args)
        self.assertEqual(json.loads(self.journal.read_text())['phase'], 'starting')
        self.fail_bootstrap = False
        deploy.main(self.args)
        self.args.action = 'rollback'
        deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'old0', b'old1'])
        self.assert_preserved()

    def test_partial_file_install_resumes_then_interrupted_rollback_resumes(self):
        original = deploy.atomic
        def interrupted(path, data, mode=0o600):
            if Path(path) == self.binaries[1]:
                raise OSError('interrupted install')
            original(path, data, mode)
        with patch('deploy.atomic', side_effect=interrupted):
            with self.assertRaisesRegex(OSError, 'interrupted install'):
                deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'new', b'old1'])
        deploy.main(self.args)
        self.args.action = 'rollback'
        self.fail_bootstrap = True
        with self.assertRaisesRegex(RuntimeError, 'bootstrap'):
            deploy.main(self.args)
        self.fail_bootstrap = False
        deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'old0', b'old1'])
        self.assert_preserved()

    def test_configuration_drift_does_not_stop_services(self):
        deploy.main(self.args)
        commands = self.commands[:]
        self.config.write_text('changed config')
        self.args.action = 'rollback'
        with self.assertRaisesRegex(RuntimeError, 'config changed'):
            deploy.main(self.args)
        self.assertEqual(self.commands, commands)

    def test_health_failure_can_roll_back_and_blocks_other_release(self):
        self.fail_health = True
        with self.assertRaisesRegex(RuntimeError, 'health'):
            deploy.main(self.args)
        self.args.tag = 'monitor-v0.1.1'
        with self.assertRaisesRegex(RuntimeError, 'another deployment'):
            deploy.main(self.args)
        self.args.tag = 'monitor-v0.1.0'
        self.args.action = 'rollback'
        self.fail_health = False
        deploy.main(self.args)
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'old0', b'old1'])
        self.assert_preserved()

    def test_incompatible_rollback_keeps_data_and_stops_services(self):
        deploy.main(self.args)
        self.fail_compatibility = True
        self.args.action = 'rollback'
        with self.assertRaisesRegex(RuntimeError, 'unsupported decision'):
            deploy.main(self.args)
        self.assertEqual(self.loaded, {"com.codex-issue-loop.supervisor": 999})
        self.assertEqual([p.read_bytes() for p in self.binaries], [b'new', b'new'])
        self.assert_preserved()

    def test_old_binary_or_missing_restart_is_not_success(self):
        deploy.main(self.args)
        self.binaries[0].write_bytes(b'old0')
        with self.assertRaisesRegex(RuntimeError, 'digest mismatch'):
            deploy.main(self.args)
        self.binaries[0].write_bytes(b'new')
        self.loaded[deploy.LABELS[0]] = 100
        with self.assertRaisesRegex(RuntimeError, 'not restarted'):
            deploy.main(self.args)

    def test_stale_prometheus_and_wrong_html_are_not_success(self):
        deploy.main(self.args)
        original = self.fetch
        for endpoint, value, message in [('freshness', {'status': 'success', 'data': {'result': [{'value': [0, '0']}]}}, 'stale'),
                                         ('/', b'wrong html', 'HTML mismatch')]:
            with patch('deploy.fetch', side_effect=lambda url, as_json=True: value if url.endswith(endpoint) else original(url, as_json)):
                with self.assertRaisesRegex(RuntimeError, message):
                    deploy.main(self.args)


class ServiceIdentityTests(unittest.TestCase):
    def test_loaded_plist_arguments_and_executable_inode(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            binary = root / 'monitor'
            binary.touch()
            service = {'label': 'fixture', 'plist': str(root / 'fixture.plist'), 'binary': str(binary),
                       'args': [str(binary), 'run', '--config', str(root / 'config')]}
            output = 'path = ' + service['plist'] + '\nprogram = ' + str(binary) + '\narguments = {\n' + '\n'.join(service['args']) + '\n}\npid = 123\n'
            files = 'p123\nftxt\ni' + str(binary.stat().st_ino) + '\nn' + str(binary) + '\n'
            for kind in ['valid', 'inode', 'args', 'plist']:
                printed = output.replace('\nrun\n', '\nserve\n') if kind == 'args' else output
                if kind == 'plist':
                    printed = printed.replace('fixture.plist', 'other.plist')
                with self.subTest(kind=kind), patch('deploy.command', side_effect=[printed, str(deploy.os.getuid()), files.replace('i' + str(binary.stat().st_ino), 'i0') if kind == 'inode' else files]):
                    if kind == 'valid':
                        self.assertEqual(deploy.inspect(service, 'gui/501'), 123)
                    else:
                        with self.assertRaises(RuntimeError):
                            deploy.inspect(service, 'gui/501')


class ReleaseTests(unittest.TestCase):
    tag = 'v1.2.3'

    def test_release_checks_before_execution(self):
        for failure in [None, 'checksum', 'attestation', 'commit', 'prerelease', 'draft', 'workflow', 'version', 'binary_commit', 'schema']:
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                calls = []
                tag, commit = self.tag, 'a' * 40
                independent = tag.startswith('monitor-v')
                workflow = 'monitor-release.yml' if independent else 'release.yml'
                def command(*args):
                    calls.append(args)
                    if args[:3] == ('gh', 'release', 'view'):
                        return json.dumps({'tagName': tag, 'isDraft': failure == 'draft', 'isPrerelease': failure == 'prerelease'})
                    if args[:3] == ('gh', 'run', 'list'):
                        self.assertEqual(args[args.index('--workflow') + 1], workflow)
                        return json.dumps([{'headSha': commit, 'headBranch': tag, 'status': 'completed', 'conclusion': 'failure' if failure == 'workflow' else 'success'}])
                    if args[:2] == ('gh', 'api'):
                        if '/ref/' in args[2]:
                            return json.dumps({'object': {'type': 'tag', 'sha': 'b' * 40}})
                        return json.dumps({'tag': tag, 'object': {'type': 'commit', 'sha': 'bad' if failure == 'commit' else commit}})
                    if args[:3] == ('gh', 'release', 'download'):
                        (directory / deploy.ASSET).write_bytes(b'binary')
                        assets = [deploy.ASSET]
                        if not independent:
                            assets.append('release-manifest.json')
                            (directory / 'release-manifest.json').write_text(json.dumps({'version': tag, 'commit': commit}))
                        self.assertEqual([args[i + 1] for i, value in enumerate(args) if value == '--pattern'], [deploy.ASSET, 'checksums.txt'] + ([] if independent else ['release-manifest.json']))
                        (directory / 'checksums.txt').write_text(''.join(deploy.digest(directory / asset) + '  ' + asset + '\n' for asset in assets))
                        if failure == 'checksum':
                            (directory / deploy.ASSET).write_bytes(b'tampered')
                    if args[:3] == ('gh', 'attestation', 'verify') and failure == 'attestation':
                        raise RuntimeError('attestation rejected')
                    return ''
                with patch('deploy.command', side_effect=command), patch('deploy.version', return_value={'version': 'wrong' if failure == 'version' else tag, 'commit': 'wrong' if failure == 'binary_commit' else commit, 'target': 'darwin/arm64', 'monitor_schema_version': 99 if failure == 'schema' else 1}) as execute:
                    if failure:
                        with self.assertRaises(RuntimeError):
                            deploy.verify_release(directory, tag, commit)
                        if failure in ['version', 'binary_commit', 'schema']:
                            execute.assert_called_once()
                        else:
                            execute.assert_not_called()
                    else:
                        deploy.verify_release(directory, tag, commit)
                        execute.assert_called_once()
                        attestations = [c for c in calls if c[:3] == ('gh', 'attestation', 'verify')]
                        self.assertEqual(len(attestations), 2 if independent else 3)
                        for c in attestations:
                            self.assertIn('--source-digest', c)
                            self.assertIn(commit, c)
                            self.assertIn(deploy.REPOSITORY + '/.github/workflows/' + workflow, c)


class IndependentReleaseTests(ReleaseTests):
    tag = 'monitor-v0.1.0'


if __name__ == '__main__':
    unittest.main()
