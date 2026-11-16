#!/usr/bin/env python3
"""Canonical contract source. No third-party packages; regenerate, never edit outputs."""
import json
from pathlib import Path
ROOT = Path(__file__).resolve().parent
S = {}
def text(n=200, minimum=0, **kw): return dict(type='string', minLength=minimum, maxLength=n, **kw)
def enum(*values): return {'type':'string','enum':list(values)}
def const(value): return {'const':value}
def integer(lo=0, hi=2147483647): return {'type':'integer','minimum':lo,'maximum':hi}
def arr(item, maximum=100, minimum=0): return {'type':'array','items':item,'minItems':minimum,'maxItems':maximum}
def obj(properties, optional=()): return {'type':'object','properties':properties,'required':[k for k in properties if k not in optional],'additionalProperties':False}
def ref(name): return {'$ref':'#/components/schemas/'+name}
def union(*items): return {'oneOf':list(items)}
def nullable(item): return {'anyOf':[item,{'type':'null'}]}
def add(name, schema): S[name]=schema; return ref(name)
UUID=add('UUID',text(36,36,pattern=r'^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'))
Rev=add('Revision',text(19,1,pattern=r'^(0|[1-9][0-9]{0,18})$'))
Date=add('Timestamp',text(24,20,format='date-time',pattern=r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{3})?Z$'))
Secret=add('Secret',text(512,32,pattern=r'^[A-Za-z0-9_.-]+$'))
Hash=add('SHA256',text(64,64,pattern=r'^[0-9a-f]{64}$'))
Phone=add('Phone',text(16,9,pattern=r'^\+[1-9][0-9]{7,14}$'))
Cursor=add('Cursor',text(2048,32,pattern=r'^[A-Za-z0-9_.-]+$'))
Base=add('Base',union(obj({'revision':Rev}),obj({'after_operation_id':UUID})))
Unit=enum('kg','lb'); Mode=enum('external','none')
ExerciseRef=add('ExerciseRef',obj({'source':enum('catalog','personal'),'id':UUID}))
Load=add('Load',union(obj({'load_mode':const('external'),'weight_value':text(11,1,pattern=r'^(0|[1-9][0-9]{0,6})(\.[0-9]{1,3})?$'),'weight_unit':Unit}),obj({'load_mode':const('none'),'weight_value':{'type':'null'},'weight_unit':{'type':'null'}})))
# Draft rows are deliberately nullable/incomplete, while published rows are complete.
DraftExercise=add('DraftExercise',obj({'id':UUID,'exercise_ref':nullable(ExerciseRef),'prescribed_sets':nullable(integer(1,100)),'prescribed_reps':text(80),'rest_seconds':nullable(integer(0,3600)),'notes':text(1000)}))
DraftDay=add('DraftDay',obj({'id':UUID,'label':text(80),'exercises':arr(DraftExercise,50)}))
Draft=add('Draft',obj({'format_version':const(1),'title':text(120),'days':arr(DraftDay,14)}))
DayMap=add('DayIDMap',obj({'draft_day_id':UUID,'id':UUID,'exercises':arr(obj({'draft_exercise_id':UUID,'id':UUID}),50,1)}))
DuplicateMap=add('DuplicateDayMap',obj({'source_day_id':UUID,'id':UUID,'exercises':arr(obj({'source_exercise_id':UUID,'id':UUID}),50,1)}))
commands={
 'profile.update':obj({'display_name':text(80,1),'weight_unit':Unit}),
 'personal_exercise.create':obj({'name':text(120,1),'load_mode':Mode,'notes':text(1000)}),
 'personal_exercise.update':obj({'name':text(120,1),'load_mode':Mode,'notes':text(1000)}),
 'personal_exercise.archive':obj({}),
 'routine.create':obj({'draft':Draft}),
 'routine.draft.save':obj({'content':Draft,'base_version_id':nullable(UUID)}),
 'routine.draft.discard':obj({}),
 'routine.publish':obj({'draft_revision':Rev,'version_id':UUID,'days':arr(DayMap,14,1)}),
 'routine.duplicate':obj({'source_version_id':UUID,'title':text(120,1),'days':arr(DuplicateMap,14,1)}),
 'routine.archive':obj({}), 'routine.restore':obj({}), 'routine.delete':obj({}),
 'workout.start':obj({'routine_version_id':UUID,'routine_day_id':UUID,'version_hash':Hash,'started_at':Date,'timezone':text(80,1),'exercises':arr(obj({'source_exercise_id':UUID,'id':UUID}),50,1)}),
 'workout.set.create':obj({'workout_id':UUID,'workout_exercise_id':UUID,'observed_workout':Base,'position_hint':integer(0,9999),'load':Load,'repetitions':integer(0,10000)}),
 'workout.set.update':obj({'workout_id':UUID,'observed_workout':Base,'load':Load,'repetitions':integer(0,10000)}),
 'workout.set.delete':obj({'workout_id':UUID,'observed_workout':Base}),
 'workout.complete':obj({'finished_at':Date}), 'workout.delete':obj({}),
 'conflict.resolve':obj({'conflict_revision':Rev,'choice':enum('keep_current','apply_proposal','copy'),'expected_current':Base,'copy_ids':arr(obj({'source_id':UUID,'new_id':UUID}),1100)},('copy_ids',)),
}
command_refs=[]; offline_refs=[]
for name,payload in commands.items():
 key='Command_'+name.replace('.','_')
 r=add(key,obj({'operation_id':UUID,'origin_device_id':UUID,'type':const(name),'entity_id':UUID,'base':Base,'captured_at':Date,'payload':payload}))
 command_refs.append(r)
 if name.startswith('workout.') or name=='conflict.resolve': offline_refs.append(r)
Command=add('Command',union(*command_refs)); OfflineCommand=add('OfflineCommand',union(*offline_refs))
Entity=enum('profile','personal_exercise','routine','routine_draft','routine_version','workout','workout_set','conflict')
Affected=add('Affected',obj({'entity_type':Entity,'entity_id':UUID,'revision':Rev}))
error_codes=['VALIDATION_ERROR','NOT_FOUND','PHONE_UNAVAILABLE','AUTHORIZATION_REQUIRED','AUTH_REQUIRED','ATTEMPT_EXPIRED','INVALID_PROOF','VERIFICATION_FAILED','RECOVERY_UNAVAILABLE','SESSION_REVOKED','ACCOUNT_UNAVAILABLE','RATE_LIMITED','IDEMPOTENCY_MISMATCH','REVISION_CONFLICT','ENTITY_DELETED','DEPENDENCY_PENDING','DEPENDENCY_BLOCKED','CURSOR_EXPIRED','EPOCH_CHANGED','UPGRADE_REQUIRED','TEMPORARY_UNAVAILABLE','POLICY_NOT_CONFIGURED','RESULT_EXPIRED','LIMIT_EXCEEDED']
Error=add('Error',obj({'code':enum(*error_codes),'message':text(240,1),'retryable':{'type':'boolean'},'request_id':UUID,'action':enum('correct','retry','wait','authenticate','authorize_device','recover','review','bootstrap','update_app','contact_support','none'),'fields':arr(obj({'path':text(200,1),'code':text(80,1)}),50),'retry_after_seconds':integer(0,86400)},('retry_after_seconds',)))
results=[]
for outcome in ['accepted','accepted_with_review','conflict','dependency_pending','dependency_blocked','rejected','retry']:
 p={'operation_id':UUID,'outcome':const(outcome)}
 if outcome in ['accepted','accepted_with_review']:
  p.update(affected=arr(Affected,1004,1), committed_at=Date, result=union(obj({}),obj({'version_id':UUID,'version_number':integer(1),'content_hash':Hash}),obj({'conflict_id':UUID})))
  if outcome=='accepted_with_review': p['conflict_id']=UUID
 elif outcome=='conflict': p.update(error=Error,conflict_id=UUID)
 else: p.update(error=Error)
 results.append(add('Result_'+outcome,obj(p)))
Result=add('OperationResult',union(*results))
Push=add('Push',obj({'protocol_version':const(1),'operations':arr(OfflineCommand,50,1)}))
PushResult=add('PushResult',obj({'protocol_version':const(1),'results':arr(Result,50,1)}))
# Canonical read shapes; no arbitrary JSON data blobs in the transport contract.
Common={'id':UUID,'revision':Rev,'created_at':Date,'updated_at':Date,'deleted_at':nullable(Date)}
Profile=add('Profile',obj({'id':UUID,'display_name':text(80),'weight_unit':Unit,'revision':Rev}))
Exercise=add('Exercise',obj({**Common,'source':enum('catalog','personal'),'name':text(120,1),'load_mode':Mode,'notes':text(1000),'archived_at':nullable(Date)}))
Routine=add('Routine',obj({**Common,'title':text(120),'status':enum('draft','ready','archived'),'current_version_id':nullable(UUID),'duplicated_from_version_id':nullable(UUID)}))
DraftRecord=add('RoutineDraft',obj({'id':UUID,'revision':Rev,'base_version_id':nullable(UUID),'content':nullable(Draft),'deleted_at':nullable(Date)}))
PublishedExercise=add('PublishedExercise',obj({'id':UUID,'exercise_ref':ExerciseRef,'name_snapshot':text(120,1),'load_mode_snapshot':Mode,'position':integer(0,49),'prescribed_sets':integer(1,100),'prescribed_reps':text(80,1),'rest_seconds':nullable(integer(0,3600)),'notes':text(1000)}))
PublishedDay=add('PublishedDay',obj({'id':UUID,'label':text(80,1),'position':integer(0,13),'exercises':arr(PublishedExercise,50,1)}))
Version=add('RoutineVersion',obj({'id':UUID,'routine_id':UUID,'revision':const('1'),'version_number':integer(1),'title_snapshot':text(120,1),'format_version':const(1),'source_draft_revision':Rev,'content_hash':Hash,'published_at':Date,'days':arr(PublishedDay,14,1)}))
WorkoutExercise=add('WorkoutExercise',obj({**S['PublishedExercise']['properties'],'source_exercise_id':UUID}))
Workout=add('Workout',obj({**Common,'routine_version_id':UUID,'routine_day_id':UUID,'routine_title_snapshot':text(120,1),'day_label_snapshot':text(80,1),'state':enum('in_progress','completed'),'started_at':Date,'finished_at':nullable(Date),'timezone':text(80,1),'finalized_revision':nullable(Rev),'needs_review':{'type':'boolean'},'origin_device_id':UUID,'exercises':arr(WorkoutExercise,50,1)}))
Set=add('WorkoutSet',obj({**Common,'workout_id':UUID,'workout_exercise_id':UUID,'origin_device_id':UUID,'position_hint':integer(0,9999),'load':Load,'repetitions':integer(0,10000),'captured_at':Date}))
Conflict=add('Conflict',obj({**Common,'operation_id':UUID,'entity_type':Entity,'entity_id':UUID,'base_revision':Rev,'current_revision':Rev,'proposal':Command,'reason':enum('revision','deleted','finalization'),'state':enum('open','resolved','superseded'),'resolution_operation_id':nullable(UUID)}))
Change=add('Change',union(*[obj({'entity_type':const(kind),'entity_id':UUID,'revision':Rev,'data':ref(schema)}) for kind,schema in [('profile','Profile'),('personal_exercise','Exercise'),('routine','Routine'),('routine_draft','RoutineDraft'),('routine_version','RoutineVersion'),('workout','Workout'),('workout_set','WorkoutSet'),('conflict','Conflict')]]))
Group=add('ChangeGroup',obj({'sequence':Rev,'operation_id':UUID,'changes':arr(Change,1004,1)}))
Pull=add('PullResult',obj({'protocol_version':const(1),'epoch':UUID,'groups':arr(Group,100),'next_cursor':Cursor,'has_more':{'type':'boolean'}}))
Capabilities=add('Capabilities',obj({'protocol_version':const(1),'minimum_protocol_version':integer(1,1),'minimum_app_version':text(30,1),'max_push_operations':const(50),'max_request_bytes':const(2097152),'max_group_bytes':const(1048576),'features':arr(enum('own_routines','workouts','personal_sync'),3),'policy_version':nullable(text(80,1))}))
Snapshot=add('Snapshot',obj({'id':UUID,'epoch':UUID,'through_sequence':Rev,'page_count':integer(1,100000),'page_hashes':arr(Hash,100000,1),'expires_at':Date,'cursor':Cursor,'capabilities':Capabilities}))
Page=add('SnapshotPage',obj({'snapshot_id':UUID,'page_number':integer(0,99999),'hash':Hash,'records':arr(Change,1000)}))
Me=add('Me',obj({'profile':Profile,'phone_masked':text(32,1),'status':enum('active','deletion_pending'),'security_version':Rev,'recovery_prepared':{'type':'boolean'},'capabilities':Capabilities}))
DeviceInput=add('CandidateDevice',obj({'id':UUID,'platform':enum('android','ios'),'label':text(80,1),'public_key':text(43,43,pattern=r'^[A-Za-z0-9_-]{43}$')}))
Device=add('Device',obj({'id':UUID,'platform':enum('android','ios'),'label':text(80,1),'authorized_at':Date,'revoked_at':nullable(Date),'last_seen_at':nullable(Date),'current':{'type':'boolean'}}))
Tokens=add('Session',obj({'account_id':UUID,'device_id':UUID,'session_id':UUID,'access_token':Secret,'access_expires_at':Date,'refresh_token':Secret,'refresh_expires_at':Date,'generation':Rev,'revocation_ticket':Secret}))
Kit=add('RecoveryKit',obj({'id':UUID,'secret':Secret,'generation':Rev}))
Attempt=add('Attempt',obj({'id':UUID,'state':enum('pending','uncertain','verified','authorization_required','authorization_pending','denied','consumed','expired','cancelled'),'destination_masked':text(32,1),'nonce':Secret,'expires_at':Date,'resend_after_seconds':integer(0,3600)}))
AuthResult=add('AuthResult',union(obj({'status':const('pending'),'attempt':Attempt}),obj({'status':const('authorization_required'),'attempt':Attempt}),obj({'status':const('authorized'),'session':Tokens,'recovery_kit':nullable(Kit)})))
Authorization=add('DeviceAuthorization',obj({'id':UUID,'candidate':DeviceInput,'comparison_code':text(6,6,pattern=r'^[0-9]{6}$'),'status':enum('pending','approved','denied','expired','cancelled'),'created_at':Date,'expires_at':Date},('created_at',)))
Proof=add('DeviceProof',obj({'id':UUID,'nonce':Secret,'purpose':enum('authorize_device','revoke_devices','recovery_key','phone_change','delete_account','vault_unlock'),'request_hash':Hash,'expires_at':Date}))
ProofToken=add('ProofToken',obj({'proof_token':Secret,'expires_at':Date}))
Ack=add('Acknowledgement',obj({'operation_id':UUID,'status':enum('completed','deletion_pending'),'completed_at':Date}))
DecisionResultBinding={'account_id':UUID,'authorization_id':UUID,'operation_id':UUID,'original_session_id':UUID,'original_device_id':UUID,'decision':enum('approve','deny'),'request_hash':Hash,'checked_at':Date}
DecisionResult=add('DeviceAuthorizationDecisionResult',union(
 obj({**DecisionResultBinding,'status':const('completed'),'completed_at':Date}),
 obj({**DecisionResultBinding,'status':enum('not_applied','unresolved'),'completed_at':{'type':'null'}})))
Vault=add('Vault',obj({'id':UUID,'key_version':text(80,1),'data_key':text(43,43,pattern=r'^[A-Za-z0-9_-]{43}$')}))
Channel=enum('whatsapp','sms')
paths={}
def endpoint(method,path,request,response,security='Personal',query=None,summary='',proof=False):
 operation={'operationId':method+'_'+path.strip('/').replace('/','_').replace('{','').replace('}','').replace('-','_'),'summary':summary or path,'security':[] if security is None else [{security:[]}],'responses':{'200':{'description':'Resultado tipado','content':{'application/json':{'schema':response}}},**{str(code):{'description':label,'content':{'application/json':{'schema':Error}}} for code,label in [(400,'Entrada inválida'),(401,'Credencial inválida'),(403,'Operación no autorizada'),(404,'No encontrado'),(409,'Conflicto o reintento incompatible'),(410,'Intento/cursor/resultado vencido'),(413,'Límite de tamaño'),(426,'Actualización necesaria'),(429,'Límite temporal'),(503,'Servicio no disponible')]}}}
 parameters=[]
 for name in __import__('re').findall(r'\{(\w+)\}',path): parameters.append({'in':'path','name':name,'required':True,'schema':integer(0,99999) if name=='number' else enum('catalog','personal') if name=='source' else UUID})
 for name,schema in (query or {}).items(): parameters.append({'in':'query','name':name,'required':False,'schema':schema})
 if method=='post': parameters.append({'in':'header','name':'Idempotency-Key','required':True,'schema':UUID})
 if proof: parameters.append({'in':'header','name':'X-Device-Proof','required':True,'schema':Secret})
 if parameters: operation['parameters']=parameters
 if request:
  reqname=operation['operationId']+'_Request'
  operation['requestBody']={'required':True,'content':{'application/json':{'schema':add(reqname,request)}}}
 paths.setdefault(path,{})[method]=operation
endpoint('post','/auth/start',obj({'attempt_id':UUID,'phone':Phone,'country':text(2,2,pattern=r'^[A-Z]{2}$'),'channel':Channel,'purpose':enum('access','recovery'),'candidate':DeviceInput,'new_account_id':UUID,'continuation_secret':Secret}),Attempt,None)
endpoint('post','/auth/attempts/{id}/resend',obj({'channel':Channel}),Attempt,'Attempt')
endpoint('post','/auth/check',obj({'attempt_id':UUID,'code':text(10,4,pattern=r'^[0-9]{4,10}$'),'device_signature':text(86,86,pattern=r'^[A-Za-z0-9_-]{86}$')},('device_signature',)),AuthResult,'Attempt')
endpoint('get','/auth/attempts/{id}',None,AuthResult,'Attempt')
endpoint('post','/auth/device-authorizations',obj({'attempt_id':UUID,'id':UUID}),Authorization,'Attempt')
endpoint('get','/me/device-authorizations',None,obj({'items':arr(Authorization,100),'next_cursor':nullable(Cursor)}),query={'cursor':Cursor,'limit':integer(1,100)})
endpoint('post','/me/device-authorizations/{id}/decision',obj({'decision':enum('approve','deny')}),Ack,proof=True)
decision_result_path='/me/device-authorizations/{id}/decisions/{operation_id}'
endpoint('get',decision_result_path,None,DecisionResult,query={'original_session_id':UUID,'original_device_id':UUID,'decision':enum('approve','deny')},summary='Consultar una decisión anterior sin volver a ejecutarla')
for parameter in paths[decision_result_path]['get']['parameters']:
 if parameter['in']=='query': parameter['required']=True
endpoint('post','/auth/recovery/complete',obj({'attempt_id':UUID,'recovery_key_id':UUID,'recovery_secret':Secret,'confirm_phone_change':{'type':'boolean'}}),AuthResult,'Attempt')
endpoint('post','/auth/refresh',obj({'refresh_token':Secret,'device_signature':text(86,86,pattern=r'^[A-Za-z0-9_-]{86}$')}),Tokens,None)
endpoint('get','/me',None,Me)
endpoint('get','/me/devices',None,obj({'items':arr(Device,100),'next_cursor':nullable(Cursor)}),query={'cursor':Cursor,'limit':integer(1,100)})
endpoint('post','/me/devices/{id}/revoke',obj({}),Ack,proof=True)
endpoint('post','/me/devices/revoke',obj({'scope':enum('others','all')}),Ack,proof=True)
endpoint('post','/auth/logout',obj({'revocation_ticket':Secret}),Ack,None)
endpoint('post','/me/recovery-key',obj({'id':UUID}),Kit,proof=True)
endpoint('post','/me/recovery-key/confirm',obj({'recovery_key_id':UUID,'saved':const(True)}),Ack)
endpoint('post','/me/proofs',obj({'id':UUID,'purpose':S['DeviceProof']['properties']['purpose'],'request_hash':Hash}),Proof)
endpoint('post','/me/proofs/{id}/complete',obj({'signature':text(86,86,pattern=r'^[A-Za-z0-9_-]{86}$')}),ProofToken)
endpoint('post','/me/phone-change/start',obj({'attempt_id':UUID,'phone':Phone,'country':text(2,2,pattern=r'^[A-Z]{2}$'),'channel':Channel,'continuation_secret':Secret}),Attempt,proof=True)
endpoint('post','/me/phone-change/complete',obj({'attempt_id':UUID,'code':text(10,4,pattern=r'^[0-9]{4,10}$'),'continuation_secret':Secret}),Tokens,proof=True)
endpoint('post','/me/deletion',obj({'confirmation':const('delete_my_account'),'policy_version':text(80,1),'result_secret':Secret}),Ack,proof=True)
endpoint('get','/auth/results/{id}',None,union(Ack,Tokens,Kit,AuthResult),'Result')
paths['/auth/results/{id}']['get']['security'].append({'Personal':[]})
endpoint('post','/me/vaults',obj({'id':UUID}),Vault)
endpoint('post','/me/vaults/{id}/unlock',obj({}),Vault,proof=True)
for path,schema,filters in [('/exercises',Exercise,{'query':text(120)}),('/routines',Routine,{'status':enum('active','archived','all'),'query':text(120)}),('/workouts',Workout,{}),('/conflicts',Conflict,{'status':enum('open','resolved','superseded')})]:
 endpoint('get',path,None,obj({'items':arr(schema,100),'next_cursor':nullable(Cursor)}),query={'cursor':Cursor,'limit':integer(1,100),**filters})
for path,schema in [('/routines/{id}',Routine),('/routines/{id}/draft',DraftRecord),('/routine-versions/{id}',Version),('/workouts/{id}',obj({'workout':Workout,'sets':arr(Set,1000)})),('/conflicts/{id}',Conflict)]: endpoint('get',path,None,schema)
endpoint('get','/exercises/{source}/{id}/last-workout',None,obj({'workout':nullable(Workout),'sets':arr(Set,1000)}),query={'exclude_workout_id':UUID})
endpoint('post','/commands',Command,Result)
endpoint('post','/sync/push',Push,PushResult)
endpoint('get','/sync/pull',None,Pull,query={'cursor':Cursor,'limit':integer(1,100)})
endpoint('post','/sync/snapshots',obj({'protocol_version':const(1)}),Snapshot)
endpoint('get','/sync/snapshots/{id}/pages/{number}',None,Page)
O={'openapi':'3.1.0','info':{'title':'Mancuerna etapa 1','version':'1.0.3','description':'Contrato adoptado de S00; disponibilidad de endpoints según implementación. Reglas semánticas: README.md y referencia S00 SECURITY.md/SYNC.md.'},'servers':[{'url':'/api/mancuerna/v1'}],'paths':paths,'components':{'securitySchemes':{'Personal':{'type':'http','scheme':'bearer','bearerFormat':'JWT (aud=mancuerna, clave separada)'},'Attempt':{'type':'apiKey','in':'header','name':'X-Attempt-Secret'},'Result':{'type':'apiKey','in':'header','name':'X-Result-Secret'}},'schemas':S}}
def save(path,value): (ROOT/path).write_text(json.dumps(value,ensure_ascii=False,indent=2)+'\n')
save('openapi.json',O)
# Shared types are mechanical; refinements remain runtime schema/domain validation.
def ts(x):
 if '$ref' in x:return x['$ref'].split('/')[-1]
 if 'const'in x:return json.dumps(x['const'])
 if 'enum'in x:return ' | '.join(json.dumps(v) for v in x['enum'])
 if 'oneOf'in x or 'anyOf'in x:return '('+' | '.join(ts(v) for v in x.get('oneOf',x.get('anyOf')))+')'
 t=x.get('type')
 if t=='object':return '{ '+ '; '.join(json.dumps(k)+('' if k in x.get('required',[]) else '?')+': '+ts(v) for k,v in x['properties'].items())+' }'
 if t=='array':return 'Array<'+ts(x['items'])+'>'
 return {'string':'string','integer':'number','boolean':'boolean','null':'null'}[t]
(ROOT/'types.ts').write_text('// Generated by build_contract.py; do not edit. Validate API data against OpenAPI.\n'+ '\n'.join('export type '+k+' = '+ts(v)+';' for k,v in S.items())+'\n')
# Deterministic minimum examples, plus adversarial fixtures. Not a fake server.
counter=0
def sample(x):
 global counter
 if '$ref'in x:
  name=x['$ref'].split('/')[-1]
  if name=='UUID':counter+=1;return f'00000000-0000-4000-8000-{counter:012x}'
  return sample(S[name])
 if 'const'in x:return x['const']
 if 'enum'in x:return x['enum'][0]
 if 'oneOf'in x or 'anyOf'in x:return sample(x.get('oneOf',x.get('anyOf'))[0])
 t=x['type']
 if t=='object':return {k:sample(v) for k,v in x['properties'].items() if k in x['required']}
 if t=='array':return [sample(x['items']) for _ in range(x.get('minItems',0))]
 if t=='null':return None
 if t=='integer':return x.get('minimum',0)
 if t=='boolean':return True
 if x.get('format')=='date-time':return '2026-10-15T18:00:00Z'
 p=x.get('pattern','')
 if p==r'^(0|[1-9][0-9]{0,18})$':return '1'
 if p.startswith(r'^\+'):return '+525500000001'
 if p.startswith(r'^(0|'):return '30.000'
 if p==r'^[A-Z]{2}$':return 'MX'
 if p==r'^[0-9]{4,10}$':return '123456'
 if p==r'^[0-9]{6}$':return '123456'
 return 'a'*max(1,x.get('minLength',0))
fixtures=[]
for name in list(S):
 # Each named schema and each union alternative is exercised separately.
 fixtures.append({'name':name,'schema':name,'valid':True,'value':sample(ref(name))})
for name in ['Command_profile_update','Command_workout_set_create','post_auth_start_Request']:
 value=sample(S[name]);value['account_id']=sample(UUID)
 fixtures.append({'name':name+'_injected_owner','schema':name,'valid':False,'value':value})
fixtures.extend([
 {'name':'decision_not_applied','schema':'DeviceAuthorizationDecisionResult','valid':True,'value':{**sample(DecisionResult),'status':'not_applied','completed_at':None}},
 {'name':'decision_unresolved','schema':'DeviceAuthorizationDecisionResult','valid':True,'value':{**sample(DecisionResult),'status':'unresolved','completed_at':None}},
 {'name':'decision_completed_without_time','schema':'DeviceAuthorizationDecisionResult','valid':False,'value':{**sample(DecisionResult),'completed_at':None}},
 {'name':'decision_absent_with_completion','schema':'DeviceAuthorizationDecisionResult','valid':False,'value':{**sample(DecisionResult),'status':'not_applied'}},
 {'name':'decision_result_no_credentials','schema':'DeviceAuthorizationDecisionResult','valid':False,'value':{**sample(DecisionResult),'access_token':'a'*43}},
 {'name':'recovery_not_saved','schema':'post_me_recovery_key_confirm_Request','valid':False,'value':{'recovery_key_id':sample(UUID),'saved':False}},
 {'name':'recovery_confirm_injected_owner','schema':'post_me_recovery_key_confirm_Request','valid':False,'value':{'recovery_key_id':sample(UUID),'saved':True,'account_id':sample(UUID)}},
 {'name':'negative_weight','schema':'Load','valid':False,'value':{'load_mode':'external','weight_value':'-1','weight_unit':'kg'}},
 {'name':'no_load_with_weight','schema':'Load','valid':False,'value':{'load_mode':'none','weight_value':'0','weight_unit':'kg'}},
 {'name':'too_many_decimals','schema':'Load','valid':False,'value':{'load_mode':'external','weight_value':'1.2345','weight_unit':'kg'}},
 {'name':'ambiguous_base','schema':'Base','valid':False,'value':{'revision':'1','after_operation_id':sample(UUID)}},
 {'name':'unsafe_numeric_revision','schema':'Revision','valid':False,'value':9007199254740993},
 {'name':'negative_revision','schema':'Revision','valid':False,'value':'-1'},
 {'name':'phone_spaces','schema':'Phone','valid':False,'value':'+52 55 0000 0001'},
 {'name':'no_load_explicit','schema':'Load','valid':True,'value':{'load_mode':'none','weight_value':None,'weight_unit':None}},
 {'name':'zero_load_explicit','schema':'Load','valid':True,'value':{'load_mode':'external','weight_value':'0','weight_unit':'kg'}},
 {'name':'routine_is_not_offline','schema':'OfflineCommand','valid':False,'value':sample(S['Command_routine_create'])},
 {'name':'empty_push','schema':'Push','valid':False,'value':{'protocol_version':1,'operations':[]}},
 {'name':'oversized_push','schema':'Push','valid':False,'value':{'protocol_version':1,'operations':[sample(S['Command_workout_delete']) for _ in range(51)]}},
])
save('fixtures/contracts.json',fixtures)
route_types=[]
for path,methods in paths.items():
 for method,operation in methods.items():
  body=operation.get('requestBody',{}).get('content',{}).get('application/json',{}).get('schema',{'type':'null'})
  response=operation['responses']['200']['content']['application/json']['schema']
  params=obj({p['name']:p['schema'] for p in operation.get('parameters',[]) if p['in']=='path'})
  route_types.append(json.dumps(method.upper()+' '+path)+': { body: '+ts(body)+'; response: '+ts(response)+'; path: '+ts(params)+' }')
with (ROOT/'types.ts').open('a') as out:out.write('export type Routes = { '+ '; '.join(route_types)+' };\n')
(ROOT/'fixtures/consumer.ts').write_text('// Generated positive fixtures: independent TypeScript consumer.\nimport type * as API from \"../types\";\n'+'\n'.join('const example'+str(i)+' = '+json.dumps(f['value'],ensure_ascii=False)+' satisfies API.'+f['schema']+';' for i,f in enumerate(fixtures) if f['valid'])+'\n')
print(f'Generated {len(paths)} paths, {len(S)} schemas, {len(commands)} commands, {len(fixtures)} fixtures')
