"""Offline boundary tests; all credentials and destinations are synthetic."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location('mancuerna_local', Path(__file__).with_name('local.py'))
local = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(local)


class LocalToolTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='mc-local-unit-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        os.chmod(self.root, 0o700)

    def private(self, name, text):
        p = self.root / name
        local.write_new(p, text.encode())
        return p

    def sources(self, extra=''):
        credentials = self.private('credentials', 'TWILIO_ACCOUNT_SID=AC' + '0'*32 + '\nTWILIO_AUTH_TOKEN=' + 'a'*32 + '\nDATABASE_URL=forbidden-production\n' + extra)
        verify = self.private('verify', 'MANCUERNA_VERIFY_SERVICE_SID=VA' + '0'*32 + '\nMANCUERNA_TEST_ALLOWED_DESTINATIONS=+525555559901,+525555559902\nMANCUERNA_TEST_BUDGET_USD=5\n')
        return credentials, verify

    def test_whitelisted_literal_values_and_no_fallback(self):
        c, v = self.sources()
        with mock.patch.dict(os.environ, {'TWILIO_ACCOUNT_SID':'must-never-be-used', 'TWILIO_AUTH_TOKEN':'ambient-secret'}):
            result = local.source_configuration(c,v,600,60,6)
        self.assertEqual(result['verify']['account_sid'],'AC'+'0'*32)
        self.assertEqual(result['verify']['validity_seconds'],600)
        self.assertEqual(result['recovery_limit'],{'window_seconds':60,'max_attempts':6})
        self.assertNotIn('DATABASE_URL', json.dumps(result))
        c.write_text('TWILIO_ACCOUNT_SID=AC'+'0'*32+'\n')
        with self.assertRaises(local.LocalError):
            local.source_configuration(c,v,600,60,6)

    def test_duplicate_and_expanded_credentials_fail_without_evaluation(self):
        for i, text in enumerate(('TWILIO_AUTH_TOKEN=x\nTWILIO_AUTH_TOKEN=y', 'TWILIO_AUTH_TOKEN=$(touch marker)', 'TWILIO_AUTH_TOKEN=$TOKEN', 'TWILIO_AUTH_TOKEN=`echo hello`')):
            p=self.private('source'+str(i), text+'\nTWILIO_ACCOUNT_SID=AC'+'0'*32)
            with self.assertRaises(local.LocalError):
                local.env_values(p,local.CREDENTIAL_KEYS)
        self.assertFalse((self.root/'marker').exists())

    def test_dedicated_file_rejects_aliases_quotes_extra_budget_and_duplicate_destinations(self):
        c,v=self.sources()
        base=v.read_text()
        variants=(base+'UNKNOWN=x\n',base.replace('MANCUERNA_VERIFY_SERVICE_SID','TWILIO_VERIFY_SERVICE_SID'),base.replace('USD=5','USD=5.00'),base.replace('+525555559902','+525555559901'),base.replace('DESTINATIONS=+','DESTINATIONS="+'))
        for text in variants:
            v.write_text(text)
            with self.assertRaises(local.LocalError):
                local.source_configuration(c,v,600,60,6)

    def test_ttl_and_recovery_policy_have_no_defaults(self):
        c,v=self.sources()
        for ttl,window,maximum in ((0,60,6),(119,60,6),(86401,60,6),(600,0,6),(600,60,0)):
            with self.assertRaises(local.LocalError):
                local.source_configuration(c,v,ttl,window,maximum)
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            local.main(['setup','--state-dir',str(self.root/'state')])

    def test_private_file_permissions_links_fifo_and_size(self):
        p=self.private('private','safe')
        os.chmod(p,0o644)
        with self.assertRaises(local.LocalError): local.read_private(p)
        os.chmod(p,0o600)
        linked=self.root/'link';linked.symlink_to(p)
        with self.assertRaises(OSError):local.read_private(linked)
        hard=self.root/'hard';os.link(p,hard)
        with self.assertRaises(local.LocalError):local.read_private(p)
        hard.unlink()
        fifo=self.root/'fifo';os.mkfifo(fifo,0o600)
        with self.assertRaises(local.LocalError):local.read_private(fifo)
        with self.assertRaises(local.LocalError):local.read_private(p,2)
        with self.assertRaises(FileExistsError):local.write_new(p,b'overwrite')
        self.assertEqual(p.read_text(),'safe')

    def test_incomplete_setup_fails_before_external_commands(self):
        state=self.root/'state';state.mkdir(mode=0o700)
        with mock.patch.object(local,'run',side_effect=AssertionError('unexpected subprocess')):
            with self.assertRaises(OSError):local.validate_state(state)

    def test_environment_drops_database_provider_proxy_and_app_settings(self):
        injected={'DATABASE_URL':'forbidden','PGHOST':'remote.invalid','PGPASSFILE':'/private','MANCUERNA_CONFIG_FILE':'/secret','TWILIO_AUTH_TOKEN':'secret','HTTP_PROXY':'http://external.invalid','PYTHONPATH':'/malicious'}
        with mock.patch.dict(os.environ,injected):
            cleaned=local.clean_env()
        self.assertFalse(set(injected)&set(cleaned))
        self.assertEqual(cleaned['GOPROXY'],'off')

    def test_private_errors_and_subprocess_output_are_not_printed(self):
        secret='synthetic-secret-sentinel'
        c=self.private('bad',secret)
        err=io.StringIO()
        with contextlib.redirect_stderr(err):
            result=local.main(['status','--state-dir',str(c)])
        self.assertEqual(result,1)
        self.assertNotIn(secret,err.getvalue())
        process=subprocess.CompletedProcess([],1,secret.encode(),secret.encode())
        with mock.patch.object(local.subprocess,'run',return_value=process):
            with self.assertRaisesRegex(local.LocalError,'fixed stage'):
                local.run(['synthetic'],'fixed stage')

    def test_stop_child_only_signals_unreaped_owned_process_object(self):
        exited=mock.Mock();exited.poll.return_value=0
        local.stop_child(exited)
        exited.send_signal.assert_not_called()
        active=mock.Mock();active.poll.return_value=None
        local.stop_child(active)
        active.send_signal.assert_called_once()
        active.wait.assert_called_once_with(timeout=8)

    def test_runtime_lock_serializes_setup_and_supervision(self):
        with local.lock(self.root/'lock'):
            with self.assertRaises(local.LocalError):
                with local.lock(self.root/'lock'):pass
        with local.lock(self.root/'lock'):pass

    def test_json_duplicate_properties_fail_closed(self):
        p=self.private('duplicate','{"format":1,"format":2}')
        with self.assertRaises(local.LocalError):local.json_private(p)


if __name__=='__main__':
    unittest.main()
