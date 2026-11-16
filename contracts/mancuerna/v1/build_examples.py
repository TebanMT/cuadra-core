#!/usr/bin/env python3
"""Concrete shared journey examples. Schema-valid examples, not recorded HTTP runs."""
import copy,json
from pathlib import Path
P=Path(__file__).resolve().parent
api=json.loads((P/'openapi.json').read_text());samples={x['schema']:x['value'] for x in json.loads((P/'fixtures/contracts.json').read_text()) if x['valid']}
def uid(n):return f'00000000-0000-4000-8000-{n:012x}'
D=uid(1);A=uid(2);R=uid(3);V=uid(4);DAY=uid(5);EX=uid(6);CAT=uid(7);W=uid(8);WE=uid(9);SER=uid(10)
rows=[]
def emit(flow,name,schema,value,expect):rows.append({'flow':flow,'name':name,'schema':schema,'valid':True,'value':value,'expected_behavior':expect})
def req(flow,name,path,updates,expect):
 schema=api['paths'][path]['post']['requestBody']['content']['application/json']['schema']['$ref'].split('/')[-1]
 value=copy.deepcopy(samples[schema]);value.update(updates);emit(flow,name,schema,value,expect)
def cmd(flow,name,kind,entity,base,payload,expect):
 v={'operation_id':uid(100+len(rows)),'origin_device_id':D,'type':kind,'entity_id':entity,'base':base,'captured_at':'2026-10-15T18:00:00Z','payload':payload}
 emit(flow,name,'Command',v,expect);return v['operation_id']
req('F01','start_new_account','/auth/start',{'attempt_id':uid(11),'new_account_id':A,'phone':'+525500000001','candidate':{'id':D,'platform':'android','label':'Teléfono de Ana','public_key':'a'*43}},'Sólo intento enmascarado; sin datos de cuenta.')
req('F01','check_otp','/auth/check',{'attempt_id':uid(11),'code':'123456'},'Sesión sólo si alta ganadora; una cuenta existente exige autorización adicional.')
req('F01','prepare_recovery','/me/recovery-key',{'id':uid(12)},'Requiere sesión + prueba reciente; kit nunca en journal.')
req('F01','confirm_recovery_saved','/me/recovery-key/confirm',{'recovery_key_id':uid(12),'saved':True},'Sesión activa propia; confirmar sólo kit vigente, idempotente. No emitir ni rotar clave; recovery_prepared requiere prepared_at.')
cmd('F01','profile','profile.update',A,{'revision':'1'},{'display_name':'Ana','weight_unit':'kg'},'Perfil propio, sin gym obligatorio.')
draft={'format_version':1,'title':'Tres días','days':[{'id':uid(13),'label':'Día A','exercises':[{'id':uid(14),'exercise_ref':{'source':'catalog','id':CAT},'prescribed_sets':3,'prescribed_reps':'8–12','rest_seconds':90,'notes':''}]}]}
cmd('F01','create_routine','routine.create',R,{'revision':'0'},{'draft':draft},'Crea rutina y borrador revision 1 juntos.')
cmd('F01','publish','routine.publish',R,{'revision':'1'},{'draft_revision':'1','version_id':V,'days':[{'draft_day_id':uid(13),'id':DAY,'exercises':[{'draft_exercise_id':uid(14),'id':EX}]}]},'Publica agregado inmutable y mueve puntero una vez.')
version={'id':V,'routine_id':R,'revision':'1','version_number':1,'title_snapshot':'Tres días','format_version':1,'source_draft_revision':'1','content_hash':'a'*64,'published_at':'2026-10-15T18:00:00Z','days':[{'id':DAY,'label':'Día A','position':0,'exercises':[{'id':EX,'exercise_ref':{'source':'catalog','id':CAT},'name_snapshot':'Press de pecho','load_mode_snapshot':'external','position':0,'prescribed_sets':3,'prescribed_reps':'8–12','rest_seconds':90,'notes':''}]}]}
emit('F01','download_version','RoutineVersion',version,'Hash ilustrativo: el servidor calcula el digest real al publicar; descargar conjunto completo.')
start=cmd('F02','start_offline','workout.start',W,{'revision':'0'},{'routine_version_id':V,'routine_day_id':DAY,'version_hash':'a'*64,'started_at':'2026-10-15T18:10:00Z','timezone':'America/Mexico_City','exercises':[{'source_exercise_id':EX,'id':WE}]},'Guardar snapshot+outbox; el servidor verificará la versión antes de aceptar.')
created=cmd('F02','capture_set','workout.set.create',SER,{'revision':'0'},{'workout_id':W,'workout_exercise_id':WE,'observed_workout':{'after_operation_id':start},'position_hint':0,'load':{'load_mode':'external','weight_value':'30.000','weight_unit':'kg'},'repetitions':10},'Reintento usa este mismo ID y payload. Cierre/reapertura preserva dato+cola.')
corrected=cmd('F02','correct_before_ack','workout.set.update',SER,{'after_operation_id':created},{'workout_id':W,'observed_workout':{'after_operation_id':created},'load':{'load_mode':'external','weight_value':'32.500','weight_unit':'kg'},'repetitions':10},'Resolver bases de serie y workout del recibo anterior, no la última revisión.')
cmd('F02','finish','workout.complete',W,{'after_operation_id':corrected},{'finished_at':'2026-10-15T18:40:00Z'},'Admite entrenamiento incompleto; no inventa otras dos series.')
req('F03','request_device','/auth/device-authorizations',{'attempt_id':uid(20),'id':uid(21)},'Sólo intento OTP verificado; aún no descarga historial.')
req('F03','approve_candidate','/me/device-authorizations/{id}/decision',{'decision':'approve'},'Equipo anterior prueba control y aprueba candidato exacto; consulta del intento obtiene sesión.')
cmd('F04','duplicate','routine.duplicate',uid(22),{'revision':'0'},{'source_version_id':V,'title':'Mi variante','days':[{'source_day_id':DAY,'id':uid(23),'exercises':[{'source_exercise_id':EX,'id':uid(24)}]}]},'Crea borrador editable con IDs nuevos, sin historial ni selección.')
cmd('F04','archive_original','routine.archive',R,{'revision':'2'},{},'Retira biblioteca activa; workout existente no cambia.')
cmd('F05','correction_A','workout.set.update',SER,{'revision':'2'},{'workout_id':W,'observed_workout':{'revision':'4'},'load':{'load_mode':'external','weight_value':'35.000','weight_unit':'kg'},'repetitions':8},'Rama A puede ganar; avanza revisión de serie y workout.')
cmd('F05','correction_B','workout.set.update',SER,{'revision':'2'},{'workout_id':W,'observed_workout':{'revision':'4'},'load':{'load_mode':'external','weight_value':'32.500','weight_unit':'kg'},'repetitions':12},'Rama B llega con misma base: conservar como conflicto, no LWW.')
cmd('F05','independent_set','workout.set.create',uid(25),{'revision':'0'},{'workout_id':W,'workout_exercise_id':WE,'observed_workout':{'revision':'4'},'position_hint':1,'load':{'load_mode':'external','weight_value':'32.500','weight_unit':'kg'},'repetitions':10},'ID diferente se conserva aunque valores coincidan; si terminó mientras tanto, revisión pendiente.')
req('F06','revoke_others','/me/devices/revoke',{'scope':'others'},'Revocar bajo mismo mutex que escrituras; no borrar entrenamiento.')
req('F06','recover_new_number','/auth/recovery/complete',{'attempt_id':uid(26),'recovery_key_id':uid(12),'recovery_secret':'r'*43,'confirm_phone_change':True},'Intento recovery verificó número nuevo libre; kit + OTP, consumo/rotación atómicos.')
req('F07','bootstrap','/sync/snapshots',{'protocol_version':1},'Materializar vista consistente y conservar overlay local hasta promoción completa.')
emit('F07','old_protocol','Error',{'code':'UPGRADE_REQUIRED','message':'Actualiza la app para sincronizar.','retryable':False,'request_id':uid(27),'action':'update_app','fields':[]},'No borrar datos locales, cola ni rutina descargada.')
req('F08','offline_logout_intent','/auth/logout',{'revocation_ticket':'l'*43},'Retirar credenciales locales; guardar sólo ticket limitado hasta confirmar revocación remota.')
req('F08','delete_account','/me/deletion',{'confirmation':'delete_my_account','policy_version':'policy-approved-in-S11','result_secret':'d'*43},'Sólo con política configurada y prueba reciente; no habilitar con valor de ejemplo.')
(P/'fixtures/journeys.json').write_text(json.dumps(rows,ensure_ascii=False,indent=2)+'\n')
print(f'{len(rows)} concrete examples covering '+', '.join(sorted({r['flow'] for r in rows})))
