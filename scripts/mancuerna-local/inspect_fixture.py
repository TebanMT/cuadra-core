#!/usr/bin/env python3
"""Print aggregate evidence from an explicitly synthetic, owned local fixture."""
import json
from pathlib import Path
import sys
import local


def main():
    state = local.path_arg(sys.argv[1])
    manifest, identity = local.validate_state(state)
    config = local.json_private(state / 'config.json')
    local.require(config['verify']['account_sid'] == 'AC'+'0'*32 and config['verify']['password'] == 'a'*32,
                  'Only synthetic fixture configuration is accepted')
    raw = local.sql(state, identity, Path(manifest['pg_bin']), """
SELECT json_build_object(
  'accounts', (SELECT count(*) FROM mancuerna.accounts),
  'devices', (SELECT count(*) FROM mancuerna.devices),
  'sessions', (SELECT count(*) FROM mancuerna.sessions),
  'live_sessions', (SELECT count(*) FROM mancuerna.sessions WHERE revoked_at IS NULL AND expires_at>now()),
  'max_generation', (SELECT max(generation)::text FROM mancuerna.sessions),
  'session_rotations', (SELECT count(*) FROM mancuerna.session_rotations),
  'prepared_recovery_keys', (SELECT count(*) FROM mancuerna.recovery_keys WHERE prepared_at IS NOT NULL),
  'consumed_attempts', (SELECT count(*) FROM mancuerna.auth_challenges WHERE consumed_at IS NOT NULL),
  'profile_states', (SELECT coalesce(json_agg(s),'[]'::json) FROM (SELECT revision::text,weight_unit,count(*) AS count FROM mancuerna.accounts GROUP BY revision,weight_unit ORDER BY revision,weight_unit) s),
  'command_receipts', (SELECT count(*) FROM mancuerna.operation_receipts),
  'personal_exercises', (SELECT count(*) FROM mancuerna.personal_exercises),
  'routines', (SELECT count(*) FROM mancuerna.routines),
  'routine_drafts', (SELECT count(*) FROM mancuerna.routine_drafts),
  'journal_groups', (SELECT count(*) FROM mancuerna.sync_changes),
  'snapshots', (SELECT count(*) FROM mancuerna.sync_snapshots),
  'vault_keys', (SELECT count(*) FROM mancuerna.vault_keys),
  'device_proofs', (SELECT count(*) FROM mancuerna.device_proofs),
  'receipt_purposes', (SELECT coalesce(json_object_agg(purpose,n),'{}'::json) FROM (SELECT purpose,count(*) n FROM mancuerna.auth_receipts GROUP BY purpose) r),
  'attempt_states', (SELECT coalesce(json_agg(s),'[]'::json) FROM (SELECT candidate_platform AS platform,status,channel,provider_reference IS NOT NULL AS has_provider_reference,provider_operation_kind AS operation,attempts,count(*) AS count FROM mancuerna.auth_challenges GROUP BY candidate_platform,status,channel,provider_reference IS NOT NULL,provider_operation_kind,attempts ORDER BY candidate_platform,status) s)
)
""")
    print(json.dumps(json.loads(raw), sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('Synthetic fixture inspection failed; no private output printed', file=sys.stderr)
        sys.exit(1)
