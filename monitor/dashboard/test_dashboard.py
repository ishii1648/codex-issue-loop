import json
import re
import plistlib
from urllib.parse import urlparse, parse_qs
from unittest.mock import patch
import manage
from pathlib import Path
import subprocess
import tempfile
import unittest


class DashboardTests(unittest.TestCase):
    def test_prometheus_transport_and_freshness_gates(self):
        source = (Path(__file__).parents[1] / 'internal/app/http.go').read_text()
        endpoint = re.search(r'"(http://127.0.0.1:19090/api/v1/query[^" ]+)"', source).group(1)
        expression = parse_qs(urlparse(endpoint).query)['query'][0]
        series = [
            {'series': 'up{job="agent-loop-monitor"}', 'values': '1 1 1 1 0 1'},
            {'series': 'agent_loop_monitor_reference_time_seconds', 'values': '0 15 30 45 60 0'},
        ]
        cases = []
        for seconds, value in [(0, 0), (15, 15), (30, 30), (45, 45), (60, None), (75, 0)]:
            cases.append({'expr': expression, 'eval_time': str(seconds) + 's', 'exp_samples': [] if value is None else [{'labels': 'agent_loop_monitor_reference_time_seconds', 'value': value}]})
        payload = {'rule_files': [], 'evaluation_interval': '15s', 'tests': [{'interval': '15s', 'input_series': series, 'promql_expr_test': cases}]}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'test.json'
            path.write_text(json.dumps(payload))
            subprocess.run(['promtool', 'test', 'rules', str(path)], check=True)

    def test_prepare_without_grafana_preserves_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            binary, config = root / 'monitor', root / 'config.yaml'
            binary.touch()
            config.touch()
            for name in ['grafana', 'prometheus', 'state']:
                (root / name).mkdir()
                (root / name / 'existing').write_text('preserved')
            with patch('sys.argv', ['manage.py', 'prepare', '--root', str(root), '--binary', str(binary), '--config', str(config)]), patch('manage.shutil.which', side_effect=lambda name: '/bin/prometheus' if name == 'prometheus' else None), patch('manage.subprocess.run') as run:
                manage.main()
                run.assert_not_called()
            plists = sorted(root.glob('*.plist'))
            self.assertEqual([p.stem.rsplit('.', 1)[1] for p in plists], ['api', 'prometheus'])
            api, prometheus = [plistlib.loads(p.read_bytes()) for p in plists]
            self.assertEqual(api['ProgramArguments'], [str(binary), 'serve', '--config', str(config)])
            self.assertIn('--storage.tsdb.retention.time=30d', prometheus['ProgramArguments'])
            self.assertIn('--web.listen-address=127.0.0.1:19090', prometheus['ProgramArguments'])
            self.assertIn('scrape_interval: 15s', (root / 'prometheus.yml').read_text())
            self.assertIn("127.0.0.1:19110", (root / 'prometheus.yml').read_text())
            for name in ['grafana.ini', 'plugins', 'provisioning', 'dashboards']:
                self.assertFalse((root / name).exists())
            for name in ['grafana', 'prometheus', 'state']:
                self.assertEqual((root / name / 'existing').read_text(), 'preserved')
            subprocess.run(['promtool', 'check', 'config', str(root / 'prometheus.yml')], check=True)

    def test_service_actions_only_manage_api_and_prometheus(self):
        for action in ['start', 'stop', 'status', 'restart']:
            for loaded in [True, False]:
                with self.subTest(action=action, loaded=loaded), patch('sys.argv', ['manage.py', action]), patch('manage.subprocess.run') as run:
                    run.return_value.returncode = 0 if loaded else 1
                    manage.main()
                    commands = [call.args[0] for call in run.call_args_list]
                    self.assertFalse(any('grafana' in str(command) for command in commands))
                    operation = {'start': None if loaded else 'bootstrap', 'stop': 'bootout' if loaded else None, 'status': 'print', 'restart': 'kickstart' if loaded else 'bootstrap'}[action]
                    self.assertEqual([c[1] for c in commands], [op for _ in range(2) for op in (['print', operation] if operation else ['print'])])

    def test_restart_loaded_services_does_not_bootout(self):
        with patch('sys.argv', ['manage.py', 'restart']), patch('manage.subprocess.run') as run:
            run.return_value.returncode = 0
            manage.main()
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(sum(c[1:3] == ['kickstart', '-k'] for c in commands), 2)
        self.assertFalse(any(c[1] in ['bootstrap', 'bootout'] for c in commands))

    def test_restart_unloaded_services_bootstraps(self):
        with patch('sys.argv', ['manage.py', 'restart']), patch('manage.subprocess.run') as run:
            run.return_value.returncode = 1
            manage.main()
        self.assertEqual(sum(c.args[0][1] == 'bootstrap' for c in run.call_args_list), 2)


if __name__ == '__main__':
    unittest.main()
