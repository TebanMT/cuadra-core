#!/usr/bin/env python3
"""Real PostgreSQL, disposable socket-only cluster, no .env or external DSN.

Never connects to an existing database. Every process receives an allowlisted
environment. Artifacts contain synthetic fixtures only and remain private.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unicodedata
from urllib.parse import urlencode

HERE = Path(__file__).resolve().parent
CORE = HERE.parents[1]
PG = Path(os.environ.get('MANCUERNA_PG_BIN', '/Applications/Postgres.app/Contents/Versions/latest/bin'))
ART = Path(tempfile.mkdtemp(prefix='mancuerna-cuenta-', dir='/tmp'))
os.chmod(ART, 0o700)
ENV = {k: v for k, v in os.environ.items() if k in ('PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE')}
ENV.update(PGHOST=str(ART), PGPORT='55449', PGUSER='mc_owner', PGDATABASE='mancuerna_test', PGSSLMODE='disable', TINTA_MODE='test')
passed = []
started = False
start = time.monotonic()


def run(args, name, env=None, cwd=None, expect_failure=False):
    with (ART / (name + '.log')).open('w') as out:
        p = subprocess.run([str(x) for x in args], env=env or ENV, cwd=cwd or CORE, text=True, stdout=out, stderr=subprocess.STDOUT)
    output = (ART / (name + '.log')).read_text()
    if (p.returncode != 0) != expect_failure:
        raise RuntimeError(name + ': ' + output[-6000:])
    return output


def psql(query, role='mc_owner', fail=False):
    p = subprocess.run([str(PG / 'psql'), '-XqAt', '-v', 'ON_ERROR_STOP=1'], input=query, text=True, capture_output=True, env={**ENV, 'PGUSER': role})
    if fail:
        if p.returncode == 0:
            raise AssertionError('Expected PostgreSQL rejection')
        return p.stderr
    if p.returncode:
        raise RuntimeError(p.stderr)
    return p.stdout.strip()


def check(name, value=True):
    if not value:
        raise AssertionError(name)
    passed.append(name)
    print('PASS ' + name, flush=True)


def schema():
    output = subprocess.check_output([str(PG / 'pg_dump'), '--schema-only', '--schema=public', '--schema=tinta_private', '--no-owner'], env=ENV, text=True)
    return '\n'.join(line for line in output.splitlines() if not line.startswith(('\\restrict', '\\unrestrict')))


def dsn(role):
    # URI permits legacy tests to replace the user; explicit host query pins
    # libpq/pgx to this Unix socket even after that replacement. No TCP listener.
    return f'postgresql://{role}@localhost:55449/mancuerna_test?' + urlencode({'host': str(ART), 'sslmode': 'disable'})


INITIAL_CATALOG = (
    ('27ec6e97-0f44-4031-9af8-abe3989bacc3', 'Sentadilla con barra', 'external'),
    ('795a80db-c6da-45c2-aa32-45888d585f9f', 'Sentadilla con mancuernas', 'external'),
    ('47220b2f-7d85-4718-8e8b-8f27ca252c05', 'Prensa de piernas', 'external'),
    ('54413b67-3406-4c65-aff2-ef54ff2b9347', 'Peso muerto con barra', 'external'),
    ('83e869fb-0bc4-415d-a955-4ecec09c7529', 'Peso muerto rumano con mancuernas', 'external'),
    ('4d13ea4c-c306-4110-bbba-e145cffd7eab', 'Extensión de piernas en máquina', 'external'),
    ('031a9f30-8626-4cf9-be95-4be39a1dd756', 'Curl femoral en máquina', 'external'),
    ('45ab8340-2432-43a6-9327-f570cf4b2186', 'Elevación de talones con mancuernas', 'external'),
    ('bf840aa1-3655-4681-8760-0ac0c39200af', 'Press de banca con barra', 'external'),
    ('27375a19-2ac3-4f6b-a6fd-323e410d9c34', 'Press de pecho con mancuernas', 'external'),
    ('69ac779d-aa1b-462e-b68a-6af4941b110d', 'Press de hombros con mancuernas', 'external'),
    ('d1d1db91-1d23-45ad-bf69-42da879cbe43', 'Elevaciones laterales con mancuernas', 'external'),
    ('8c178569-f15f-4b02-965a-399f53c2b33f', 'Remo con barra', 'external'),
    ('4270d41e-1eed-40e7-be33-d89925f0d654', 'Remo con mancuerna', 'external'),
    ('2a124eaa-95a9-4e1b-9daa-6d1ade6bfc3f', 'Jalón al pecho en polea', 'external'),
    ('c220393a-ceff-48d0-999a-e716c5128331', 'Curl de bíceps con mancuernas', 'external'),
    ('db2a4fdc-a79d-4666-8f7e-18ec28abb40c', 'Extensión de tríceps en polea', 'external'),
    ('068a2af4-18ea-46e6-b231-eec6e154b74c', 'Flexiones sin lastre', 'none'),
    ('008a2ee7-391c-447a-97d9-7765addcd3f5', 'Dominadas sin lastre', 'none'),
    ('74e0866d-4167-4a03-82c8-bac6515c4f54', 'Sentadilla sin carga', 'none'),
)


def check_initial_catalog():
    rows = json.loads(psql('SELECT json_agg(t ORDER BY id) FROM mancuerna.catalog_exercises t'))
    expected = []
    for exercise_id, name, mode in sorted(INITIAL_CATALOG):
        # These Spanish names use NFD mark removal, case folding and folded
        # whitespace, matching the product search normalizer for this alphabet.
        folded = ''.join(c for c in unicodedata.normalize('NFD', name) if unicodedata.category(c) != 'Mn')
        expected.append({'id': exercise_id, 'name': name, 'search_key': ' '.join(folded.casefold().split()),
                         'load_mode': mode, 'notes': '', 'revision': 1, 'archived_at': None, 'deleted_at': None})
    actual = [{k: v for k, v in row.items() if k not in ('created_at', 'updated_at')} for row in rows]
    check('Initial catalog has twenty stable names, exact load modes, folded search and no account ownership', actual == expected)
    check('Initial catalog timestamps exist and personal schema remains six',
          all(row['created_at'] and row['updated_at'] for row in rows) and
          psql('SELECT max(version) FROM mancuerna.schema_versions') == '6')
    snapshot = "SELECT json_build_object('catalog',(SELECT json_agg(t ORDER BY id) FROM mancuerna.catalog_exercises t),'migration',(SELECT row_to_json(m) FROM public._migrations m WHERE version=65))"
    original = psql(snapshot)
    seed = (CORE / 'db_migrations/postgres/065_mancuerna_initial_catalog.sql').read_text()
    psql(seed)
    check('Direct catalog migration replay preserves every row and migration timestamp', original == psql(snapshot))
    # Replay the actual DML inside a rollback-only adversarial fixture. An
    # edited/archived/deleted seed and an unrelated existing row must survive.
    body = '\n'.join(line for line in seed.splitlines() if line.strip() not in ('BEGIN;', 'COMMIT;'))
    preserved = psql("""BEGIN;
        UPDATE mancuerna.catalog_exercises SET name='Nombre editado',search_key='nombre editado',load_mode='none',notes='Nota existente',revision=9007199254740993,archived_at='2001-01-01T00:00:00Z',created_at='2000-01-01T00:00:00Z',updated_at='2001-01-01T00:00:00Z' WHERE id='27ec6e97-0f44-4031-9af8-abe3989bacc3';
        UPDATE mancuerna.catalog_exercises SET deleted_at='2002-01-01T00:00:00Z',revision=2 WHERE id='795a80db-c6da-45c2-aa32-45888d585f9f';
        INSERT INTO mancuerna.catalog_exercises(id,name,search_key,load_mode) VALUES('10000000-0000-4000-8000-000000000065','Fila ajena al catálogo inicial','fila ajena al catalogo inicial','none');
        """ + snapshot + ';\n' + body + '\n' + snapshot + ';\nROLLBACK;')
    # json_agg of composite records can span physical lines. Decode exactly
    # two JSON documents instead of treating each output line as one snapshot.
    retained_before, offset = json.JSONDecoder().raw_decode(preserved)
    retained_after = json.loads(preserved[offset:])
    check('Catalog replay never overwrites edits, revives archived/deleted identities or removes unrelated rows',
          retained_before == retained_after and original == psql(snapshot))


result = {'passed': False, 'cases': passed, 'artifacts': str(ART)}
try:
    run([PG / 'initdb', '-D', ART / 'pg', '-U', 'mc_owner', '--auth-local=trust', '--auth-host=reject', '--no-locale', '--encoding=UTF8'], 'initdb')
    run([PG / 'pg_ctl', '-D', ART / 'pg', '-l', ART / 'postgres.log', '-o', f"-c listen_addresses='' -k {ART} -p 55449", '-w', 'start'], 'start')
    started = True
    run([PG / 'createdb', '--maintenance-db=template1', 'mancuerna_test'], 'createdb')
    result['postgres'] = psql('SHOW server_version')
    result['go'] = run(['go', 'version'], 'go-version').strip()
    run(['go', 'build', '-tags', 'server', '-o', ART / 'migrate', './cmd/migrate'], 'build-migrate')
    legacy = ART / 'legacy-migrations'
    legacy.mkdir()
    for migration in (CORE / 'db_migrations/postgres').glob('*.sql'):
        if int(migration.name.split('_')[0]) <= 58:
            shutil.copy2(migration, legacy / migration.name)
    owner_env = {**ENV, 'DATABASE_URL': dsn('mc_owner')}
    run([ART / 'migrate', '--dir', legacy], 'migrate-legacy', owner_env)
    psql('CREATE ROLE mc_tinta LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE')
    run([ART / 'migrate', '--dir', legacy, '--app-role', 'mc_tinta'], 'grant-legacy', owner_env)
    psql("INSERT INTO public.gyms(id,gym_id) VALUES('10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001'),('10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002'); INSERT INTO public.products(id,gym_id,name,price,stock) VALUES('20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000001','Producto A',10,1),('20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000002','Producto B',20,2)")
    before = schema()
    data_before = psql('SELECT json_agg(t ORDER BY id) FROM public.products t')
    legacy_env = {**ENV, 'MANCUERNA_LEGACY_DSN': dsn('mc_tinta')}
    run(['go', 'run', '-tags', 'server', HERE / 'legacy_probe.go'], 'legacy-before', legacy_env)
    run([ART / 'migrate', '--dir', CORE / 'db_migrations/postgres'], 'missing-roles', owner_env, expect_failure=True)
    check('Missing roles abort migration without partial schema', psql("SELECT to_regnamespace('mancuerna') IS NULL AND (SELECT max(version)=58 FROM public._migrations)") == 't')
    roles = (CORE / 'scripts/db/mancuerna_roles.sql').read_text()
    psql('CREATE ROLE mancuerna_data LOGIN')
    psql(roles, fail=True)
    check('Unsafe pre-existing group role rejected and unchanged', psql("SELECT rolcanlogin FROM pg_roles WHERE rolname='mancuerna_data'") == 't')
    psql('DROP ROLE mancuerna_data')
    psql(roles)
    psql(roles)
    run([ART / 'migrate', '--dir', CORE / 'db_migrations/postgres'], 'migrate-personal', owner_env)
    run([ART / 'migrate', '--dir', CORE / 'db_migrations/postgres'], 'migrate-repeat', owner_env)
    check('Migrations 059-065 additive and repeatable', before == schema() and psql('SELECT max(version) FROM public._migrations') == '65')
    check_initial_catalog()
    check('Existing Tinta data unchanged', data_before == psql('SELECT json_agg(t ORDER BY id) FROM public.products t'))
    check('Signup intent and recovery confirmation persisted', psql("SELECT count(*) FROM information_schema.columns WHERE table_schema='mancuerna' AND ((table_name='auth_challenges' AND column_name='proposed_account_id') OR (table_name='recovery_keys' AND column_name='prepared_at'))") == '2')
    psql('CREATE ROLE mc_data LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE; GRANT mancuerna_data TO mc_data; CREATE ROLE mc_auth LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE; GRANT mancuerna_auth TO mc_auth')
    check('Data runtime can read the initial catalog', psql('SELECT count(*) FROM mancuerna.catalog_exercises', 'mc_data') == '20')
    for statement in (
        "INSERT INTO mancuerna.catalog_exercises(id,name,search_key,load_mode) VALUES('10000000-0000-4000-8000-000000000065','Forbidden','forbidden','none')",
        "UPDATE mancuerna.catalog_exercises SET notes='Forbidden' WHERE id='27ec6e97-0f44-4031-9af8-abe3989bacc3'",
        "DELETE FROM mancuerna.catalog_exercises WHERE id='27ec6e97-0f44-4031-9af8-abe3989bacc3'",
        'TRUNCATE mancuerna.catalog_exercises',
    ):
        psql(statement, 'mc_data', fail=True)
    for role in ('mc_auth', 'mc_tinta'):
        psql('SELECT * FROM mancuerna.catalog_exercises', role, fail=True)
    check('Catalog seed grants no runtime writes or auth/Tinta visibility', psql("""
        SELECT has_table_privilege('mc_data','mancuerna.catalog_exercises','SELECT')
          AND NOT has_table_privilege('mc_data','mancuerna.catalog_exercises','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
          AND NOT has_table_privilege('mc_auth','mancuerna.catalog_exercises','SELECT')
          AND NOT has_table_privilege('mc_tinta','mancuerna.catalog_exercises','SELECT')
    """) == 't')
    for role, table in [('mc_data', 'sessions'), ('mc_auth', 'workouts'), ('mc_tinta', 'accounts')]:
        psql('SELECT * FROM mancuerna.' + table, role, fail=True)
    for role in ('mc_data', 'mc_auth'):
        psql('SELECT * FROM public.products', role, fail=True)
    check('Personal auth/data and Tinta runtime privileges separated')
    test_env = {**ENV, 'DATABASE_URL': dsn('mc_owner'), 'MANCUERNA_TEST_DATA_DSN': dsn('mc_data'), 'MANCUERNA_TEST_AUTH_DSN': dsn('mc_auth'), 'MANCUERNA_TEST_OWNER_DSN': dsn('mc_owner')}
    # Startup-role adversarial tests temporarily grant unsafe privileges. Run
    # packages in sequence so unrelated pool validation cannot see that fixture.
    personal_tests = run(['go', 'test', '-p', '1', '-race', '-count=1', '-v', '-tags', 'server,integration', './src/shared/domain', './src/modules/personalaccounts/...', './src/modules/training/...', './src/shared/sync/personal/...', './cmd/server'], 'personal-tests', test_env)
    for required in ('TestPersonalPostgresScopesRollbackAndPooling', 'TestPersonalPostgresMutexAndConsistentSnapshot', 'TestPersonalPostgresAuthAttemptIsolation', 'TestPersonalPostgresMixedAttemptFailsAtRLS', 'TestPersonalPostgresRejectsUnsafePrivileges', 'TestPersonalPostgresTenantUoWRejectsPersonalContext', 'TestTenantRLSRuntimePoolAndFailClosed', 'TestProfilePostgresCASIsolationAndRollback', 'TestProfilePostgresConcurrentWritersOnlyOneWins', 'TestProfilePostgresInactiveAndSoftDeletedCannotWrite', 'TestOTPBudgetPostgresConcurrentInstancesRespectGlobalLimit', 'TestOTPBudgetPostgresSharesChannelsSeparatesChecksAndRollsBack', 'TestOTPBudgetPostgresRejectingOneDimensionDoesNotConsumeOthers', 'TestOTPBudgetPostgresFixedWindowAndCrossWindowBlock', 'TestOTPBudgetPostgresDifferentWindowsSerializeBeforeReadingBlock', 'TestOTPBudgetPostgresRejectsRetainedSnapshotsAndReadOnly', 'TestAuthReceiptPostgresIsolationExpiryAndRollback', 'TestAuthReceiptPostgresAttemptSecretRequired', 'TestIdentityAccessPostgresSerializesPhoneWithoutAuthorizingCandidate', 'TestIdentityAccessPostgresRollsBackCreationAndScopesReads', 'TestIdentityAccessPostgresRejectsInvalidOrMixedScopes', 'TestIdentityAccessPostgresRejectsUnavailableAndOccupiedCandidate', 'TestIdentityAccessPostgresRevalidatesAfterPhoneLock', 'TestStartAuthPostgresConcurrentRetriesSendOnceAfterCommit', 'TestStartAuthPostgresUncertainRestartAndBudgetRollback', 'TestAttemptPostgresBindingIsolationAndImmutableFields', 'TestAttemptPostgresDurableUncertaintyCASAndRollback', 'TestAttemptPostgresConcurrentReservationOnlyOneWins', 'TestAttemptPostgresVerifiedBindingAndConsumeAtomic', 'TestAttemptPostgresProviderExpiryAndDiagnostics', 'TestAuthSessionPostgresBoundSignupAndCallerRollback', 'TestAuthSessionPostgresIsolationAndNoOverwrite', 'TestAuthSessionPostgresRevocationIsAtomicAndIdempotent', 'TestAuthSessionPostgresInactiveStatesAndCurrentClock', 'TestFinalizeAccessPostgresSignupAndLostResponseAreAtomic', 'TestFinalizeAccessPostgresFailureRollsBackAllAuthority', 'TestFinalizeAccessPostgresResumeRequiresRegisteredDeviceSignature', 'TestRecoveryKeyPostgresIsolationAndPreparation', 'TestRecoveryKeyPostgresOneLiveAndImmutableVerifier', 'TestRecoveryKeyPostgresConsumeReplacementRollsBackTogether', 'TestRecoveryKeyPostgresRevocationAndInactiveAccount', 'TestRecoveryKeyPostgresBoundSignupRollback', 'TestFinalizeAccessPostgresRejectsPriorCheckReceiptAndHash', 'TestPhoneCheckPostgresDirectProofIdempotencyAndDetachedCommit', 'TestPhoneGetPostgresCannotCreditFetchApprovalOrMissing', 'TestPhoneResendPostgresExplicitCooldownFallbackAndBudgetRollback', 'TestPhoneCheckPostgresRacedReceiptRollsBackExtraBudget', 'TestPhoneCheckPostgresBudgetExhaustionRollsBackAttemptAndReceipt', 'TestPhoneResendPostgresFetchBeforeExplicitReleaseAndLateCheckIgnored', 'TestPhoneGetPostgresStaleFetchCannotOverwriteResend', 'TestPhoneCheckPostgresReceiptFailureAndMandatoryConfiguration', 'TestAccessHTTPPostgresSignupStatusReplayAndExistingAccount', 'TestMePostgresProjectionAndSessionIsolation', 'TestMePostgresRechecksConcurrentRevocationAndInactiveState', 'TestConfirmRecoveryKeyPostgresAtomicConcurrentAndDurableReplay', 'TestConfirmRecoveryKeyPostgresRollbackIsolationAndRevokedActor', 'TestMancuernaMountedWithRestrictedPoolsAndNoImplicitProviderCall', 'TestRefreshSessionPostgresConcurrentReplayRotatesOnceAndPreservesLogout', 'TestRefreshSessionPostgresRejectsReuseAndInvalidProofWithoutRevocation', 'TestRefreshSessionPostgresExpiryRevocationAndRollback', 'TestRefreshSessionPostgresResolverAndRepositoryIsolation', 'TestLogoutPostgresCurrentAndHistoricalTicketsRevokeExactSession', 'TestLogoutPostgresConcurrentIdempotenceAndRollback', 'TestLogoutPostgresIsolationInactiveAccountAndNoAmbientScope', 'TestDeviceProofPostgresConcurrentStartCompleteAndNoImplicitConsumption', 'TestDeviceProofPostgresRollbackAndSingleConsumptionWithEffectReceipt', 'TestDeviceProofPostgresActorIsolationAndCompositeSessionDeviceFK', 'TestDeviceProofPostgresOriginalExpiryWrongTargetAndRevocation', 'TestDeviceProofHTTPPostgresSignedContractReplayAndRevocation', 'TestMancuernaLocalPreparesOnlyLoopbackWithExistingGuardAndRestrictedPools', 'TestRotateRecoveryKeyPostgresConcurrentReplacementAndHonestPreparation', 'TestRotateRecoveryKeyPostgresReplayBoundToActorWindowAndLiveKey', 'TestRotateRecoveryKeyPostgresFailureRestoresPriorKitAndProof', 'TestRotateRecoveryKeyPostgresHistoricalGenerationAndIsolation', 'TestProfileCommandPostgresConcurrentReplayAndAtomicJournal', 'TestProfileCommandPostgresConflictPreservesProposalAndDependencies', 'TestProfileCommandPostgresRollbackAndCrossAccountIDCollision', 'TestProfileCommandPostgresActorOriginAndReceiptIsolation', 'TestVaultPostgresConcurrentCreationNeverOverwritesOrStoresPlaintext', 'TestVaultPostgresLogoutNewAuthorizationRecoversSameKeyWithPreviousMaster', 'TestVaultPostgresProofAndAuditRollbackAreAtomic', 'TestVaultPostgresReplayRequiresExactActorOriginalWindowAndLiveState', 'TestVaultPostgresEnvelopeContextAndMixedScopeFailClosed', 'TestVaultHTTPPostgresPublicProofReplayAndActorIsolation', 'TestSyncPullPostgresWholeGroupsPaginationAndLostResponse', 'TestSyncPullPostgresCursorIsolationCompactionEpochAndBigInt', 'TestSyncPullPostgresRevocationWinsBeforeReadAndMixedScope', 'TestSyncPullHTTPPostgresContractAndRevokedJWT', 'TestDevicesPostgresKeysetPrecisionOwnerEpochAndRevokedActor', 'TestDevicesPostgresProofBindingConcurrentReplayAndTargetIsolation', 'TestDevicesPostgresOthersCancelsOnlyDependentUnconsumedApprovals', 'TestDevicesPostgresSelfAllRollbackAndLateOutcomeCannotRevive', 'TestDevicesPostgresSingleSelfAndGlobalReceiptCollisionFailClosed', 'TestDevicesHTTPPostgresPublicProofExactRevokeAndAll', 'TestSyncPullPostgresBoundsMaterializedBytesWithoutSkippingLargeGroups', 'TestSnapshotCommandPostgresWaitedRevocationRejectsStaleView', 'TestSnapshotCommandPostgresStrictScopeRollbackAndConsistentView', 'TestSnapshotHTTPPostgresManifestPagesPullAndRevokedJWT'):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required real integration test did not pass: ' + required)
    for required in (
        'TestDeviceAuthorizationPostgresConcurrentStartAndBoundCandidate',
        'TestDeviceAuthorizationPostgresApprovalSeparateReceiptsAndReadOnlyResult',
        'TestDeviceAuthorizationPostgresDenyApproveRaceAndExpiry',
        'TestDeviceAuthorizationPostgresRollbackAndCandidateUUIDNeverOverwrite',
        'TestDeviceAuthorizationPostgresPhoneAndActorBindingsFailClosed',
        'TestDeviceAuthorizationPostgresListCursorOwnerEpochAndTerminalRequests',
        'TestDeviceAuthorizationPostgresCandidateAuditRequiresExactScopeAndReceipt',
        'TestDeviceAuthorizationHTTPPostgresCandidateOnlyResultAndActorIsolation',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required real integration test did not pass: ' + required)
    for required in (
        'TestSnapshotPostgresInitialProfileReplayIsolationAndExpiry',
        'TestSnapshotPostgresConsistentCutAndAtomicRollback',
        'TestSnapshotPostgresPagesLatestTombstonesAndDrift',
        'TestSnapshotPostgresBytePagesAndCompaction',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required real snapshot test did not pass: ' + required)
    for required in (
        'TestCompleteRecoveryPostgresConcurrentLastSlotAndCandidateOnlyReplay',
        'TestCompleteRecoveryPostgresRejectedChecksPersistOnceWithoutBinding',
        'TestCompleteRecoveryPostgresPhoneConfirmationOccupiedAndVerifierRotation',
        'TestCompleteRecoveryPostgresAuditFailureRollsBackEveryEffect',
        'TestCompleteRecoveryPostgresTwoAttemptsOneKitAndOccupiedDevice',
        'TestCompleteRecoveryPostgresScopeDirectOTPAndPhoneLockGuards',
        'TestCompleteRecoveryPostgresResultOwnerRevocationAndReplacedKit',
        'TestRecoveryHTTPPostgresVerifiedPhoneLostResponseAndRetiredSessions',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required real recovery test did not pass: ' + required)
    for required in (
        'TestPersonalPostgresPreparedAuthScopeAndAtomicity',
        'TestPersonalPostgresPreparedRunsBeforeAccountMutex',
        'TestPhoneChangePostgresSessionIdentityLostResponseAndRefresh',
        'TestPhoneChangePostgresScopeInitiatorAndSamePhoneFailClosed',
        'TestPhoneChangePostgresNegativeCheckAndUncertaintyNeverResend',
        'TestPhoneChangePostgresEffectRollbackAndExpiredProofRetry',
        'TestPhoneChangePostgresCompetingAccountsAndBoundAttemptCancellation',
        'TestPhoneChangePostgresConcurrentLastSlotAndOriginalResponseWindow',
        'TestPhoneChangePostgresRevocationDuringCheckAndStartRollback',
        'TestPhoneChangeHTTPPostgresProofBodyBindingAndLostResponse',
        'TestPersonalAuthResultPostgresPhoneCommittedReadOnlyAndActorScope',
        'TestPersonalAuthResultPostgresUncommittedPhoneHasNoResult',
        'TestPersonalAuthResultPostgresRecoveryKitLiveActorAndReplacement',
        'TestPersonalAuthResultPostgresExpiryRevocationAndCipherBinding',
        'TestPhoneChangeAttemptPostgresUnknownSendExplicitSMSAndLimitedStatus',
        'TestPhoneChangeAttemptPostgresFetchNeverApprovesAndNewCheckRequiresProof',
        'TestPhoneChangeAttemptPostgresBindingScopeAndRevocationBeforeProvider',
        'TestPhoneChangeAttemptPostgresConcurrentResendAndLateRevocation',
        'TestPhoneChangeAttemptRejectsInheritedAuthorityBeforeDependencies',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required real phone-change test did not pass: ' + required)
    for required in (
        'TestDeletionPostgresAtomicAcceptanceLostResponseAndIsolation',
        'TestDeletionPostgresPolicyProofAndCallerFailureRollback',
        'TestDeletionPostgresResultSecretOperationAndScopeBinding',
        'TestDeletionPostgresConcurrentRequestAndDataBoundary',
        'TestDeletionPostgresEffectFailureAndPureScope',
        'TestDeletionHTTPPostgresProofLimitedResultAndPolicyGate',
        'TestDeletionRequiresExplicitPolicyAndPreservesFailedState',
        'TestDeletionResultHashBindsOperationAndCanonicalSecret',
        'TestDeletionRejectsScopesAndMissingPolicyBeforeDependencies',
        'TestResultAccessSeparatesPersonalAndLimitedAuthority',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required deletion test did not pass: ' + required)
    for required in (
        'TestPersonalExercisePostgresConcurrentReplayAndCanonicalJournal',
        'TestPersonalExercisePostgresIdempotencyAndCrossAccountCollisions',
        'TestPersonalExercisePostgresRollbackAndDurableValidation',
        'TestPersonalExercisePostgresActorOriginAndPureDataScope',
        'TestPersonalExercisePostgresRepositoryNeverOverwritesAndRedactsErrors',
        'TestTrainingCommandHTTPPostgresClosedDispatchReplayAndPull',
        'TestExerciseListPostgresUnionIdentityActiveScopeAndCursor',
        'TestExerciseListPostgresUnicodeLiteralSearchAndKeysetBoundaries',
        'TestExerciseListPostgresCatalogRequiresActiveActorAndDataRole',
        'TestRoutineReadPostgresSearchAndKeysetBoundaries',
        'TestRoutineReadPostgresUnicodeFoldCursorContinuity',
        'TestRoutineReadPostgresDraftTombstoneVersionAndRevocation',
        'TestRoutineCreatePostgresConcurrentIncompleteDraftAndCompleteGroup',
        'TestRoutineCreatePostgresReferencesOwnerAndReplayBeforeAvailability',
        'TestRoutineCreatePostgresCollisionAndRollbackAcrossBothAggregates',
        'TestRoutineCreatePostgresRejectsWithdrawnActorMixedScopeAndInvalidBase',
        'TestRoutineCreatePostgresCanonicalAndConservativeStorageLimits',
        'TestRoutineDraftSavePostgresChainAndDraftOnlyProjection',
        'TestRoutineDraftSavePostgresDependenciesRemainExactAndDurable',
        'TestRoutineDraftSavePostgresConcurrentCASAndConflictPreservation',
        'TestRoutineDraftSavePostgresRollbackAndReopenKeepRootUnchanged',
        'TestRoutineDraftSavePostgresHistoricalReferencesRequireExactAppearance',
        'TestRoutineDraftSavePostgresAuthorityScopeAndOwnerAreRequired',
        'TestRoutineDraftSavePostgresLimitsFailClosedWithoutPartialEffects',
        'TestRoutinePublishPostgresConcurrentReplayCompleteGroupAndHash',
        'TestRoutinePublishPostgresBothCASAndDependenciesPrecedeSnapshots',
        'TestRoutinePublishPostgresGlobalIdentityCollisionsRollbackSavepoint',
        'TestRoutinePublishPostgresRollbackAndDatabaseImmutability',
        'TestRoutinePublishPostgresRenameArchiveAndExactHistoricalSnapshots',
        'TestRoutinePublishPostgresValidationScopeAndWithdrawnActor',
        'TestRoutinePublishPostgresVersionAndGroupSizeLimitsRemainTerminal',
        'TestSyncCommandsPostgresCanonicalGroupLimitAndSequenceWidth',
        'TestSyncCommandsPostgresJSONBPreflightAllowsDurableRejection',
        'TestSyncCommandsPostgresInvalidGroupsAndSequenceExhaustionLeaveNoEffects',
        'TestSyncCommandsPostgresCommitRechecksSequenceAndRollsBackEarlierEffects',
        'TestCommandCapturePostgresExactMetadataReplayAndIndependentServerTime',
        'TestCommandCapturePostgresNewRowsRequiredLegacyNullAndRollback',
        'TestWorkoutStartPostgresReplayCompleteSnapshotAndIndependentIdentities',
        'TestWorkoutStartPostgresHistoricalVersionAndMaterialValidation',
        'TestWorkoutStartPostgresGlobalCollisionsAndAtomicRollback',
        'TestWorkoutStartPostgresScopeActorAndHistoricalOrigin',
        'TestWorkoutStartPostgresSealAndRepositoryBoundary',
        'TestWorkoutStartPostgresTimezoneAndOriginalCapture',
        'TestWorkoutStartPostgresMaximumSnapshotAndPreflightLimit',
        'TestWorkoutSetCreatePostgresExactLoadsAndHistoricalSnapshots',
        'TestWorkoutSetCreatePostgresReplayConcurrentIndependentSets',
        'TestWorkoutSetCreatePostgresObservedDependenciesRemainExact',
        'TestWorkoutSetCreatePostgresCollisionsAndAtomicRollback',
        'TestWorkoutSetCreatePostgresFinalizedReviewAndDeletedProposal',
        'TestWorkoutSetCreatePostgresScopeOriginAndObservedValidation',
        'TestWorkoutSetCreatePostgresLifetimeIDLimitSerializes',
        'TestWorkoutSetCreatePostgresReviewRollbackAndPreflight',
        'TestPushWorkoutsRejectsMissingDependenciesAndInvalidWholeBatchBeforeIO',
        'TestPushWorkoutsRejectsActorMixedScopesAndPreflightFailuresWithoutStarting',
        'TestPushWorkoutsPreflightsUniqueOriginsThenRunsDetachedCommandsInOrder',
        'TestPushWorkoutsPreservesResultsAndMapsIndividualFailuresWithoutLeakingErrors',
        'TestPushWorkoutsActorRevocationStopsRemainderAndKeepsEarlierCommit',
        'TestPushWorkoutsDeadlineAndCancellationFillEveryUnstartedResult',
        'TestPushWorkoutsMixedUnionPreflightsSetAndPreservesOrderAndOriginalBody',
        'TestOfflineCommandClosedUnionPreservesCaptureAndRejectsAmbiguity',
    ):
        if '--- PASS: ' + required not in personal_tests:
            raise AssertionError('Required training command test did not pass: ' + required)
    if '--- SKIP:' in personal_tests:
        raise AssertionError('Personal suite silently skipped a test; inspect personal-tests.log')
    result['personal_test_count'] = sum(line.startswith('--- PASS: ') for line in personal_tests.splitlines())
    check('Personal UoW, security, profile and OTP budget on restricted PostgreSQL runtimes; zero skipped tests')
    run(['go', 'run', '-tags', 'server', HERE / 'legacy_probe.go'], 'legacy-after', legacy_env)
    run(['go', 'test', '-count=1', '-tags', 'server,integration', './src/shared/auth', './src/shared/sync/...'], 'legacy-tests', {**ENV, 'DATABASE_URL': dsn('mc_owner')})
    check('Existing Tinta auth and tenant UoW before/after; sync tests')
    run(['go', 'build', '-tags', 'server', '-o', ART / 'server', './cmd/server'], 'build-server')
    run(['go', 'build', '-tags', 'sidecar', '-o', ART / 'sidecar', './cmd/sidecar'], 'build-sidecar')
    check('Server and sidecar compile with personal schema isolated')
    result['passed'] = True
except Exception as error:
    result['error'] = str(error)
    print(str(error), file=sys.stderr)
finally:
    if started:
        try:
            run([PG / 'pg_ctl', '-D', ART / 'pg', '-m', 'immediate', '-w', 'stop'], 'stop')
        except Exception as error:
            result['passed'] = False
            result['stop_error'] = str(error)
    result['seconds'] = round(time.monotonic() - start, 2)
    sources = list((CORE / 'src/shared/sync/personal').rglob('*.go')) + list((CORE / 'cmd/server').glob('*.go')) + list((CORE / 'src/shared/domain').glob('personal*.go')) + list((CORE / 'src/modules/personalaccounts').rglob('*.go')) + list((CORE / 'src/modules/training').rglob('*.go')) + list((CORE / 'contracts/mancuerna/v1/fixtures').glob('*.json')) + [CORE / 'src/shared/domain/tenant.go', CORE / 'src/shared/domain/uow_postgres.go', CORE / 'db_migrations/postgres/059_mancuerna_personal.sql', CORE / 'db_migrations/postgres/060_mancuerna_attempt_operations.sql', CORE / 'db_migrations/postgres/061_mancuerna_refresh_logout_tickets.sql', CORE / 'db_migrations/postgres/062_mancuerna_device_proof_binding.sql', CORE / 'db_migrations/postgres/063_mancuerna_snapshot_manifest.sql', CORE / 'db_migrations/postgres/064_mancuerna_command_capture.sql', CORE / 'scripts/db/mancuerna_roles.sql', Path(__file__).resolve()]
    sources.append(CORE / 'src/modules/training/infraestructure/timezone/iana_names.txt')
    sources.append(CORE / 'db_migrations/postgres/065_mancuerna_initial_catalog.sql')
    result['sources'] = {str(p.relative_to(CORE)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(sources)}
    (ART / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print('Evidence: ' + str(ART / 'result.json'), flush=True)
sys.exit(0 if result['passed'] else 1)
