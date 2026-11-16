// Validates current output and frozen S00 compatibility fixtures.
const fs=require('fs'),path=require('path'),assert=require('node:assert/strict');
const root=path.resolve(__dirname,'..');
const Ajv=require('ajv/dist/2020.js');
const ajv=new Ajv({strict:true,allErrors:true});
ajv.addFormat('date-time',{type:'string',validate:x=>/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{3})?Z$/.test(x)&&!isNaN(Date.parse(x))&&new Date(x).toISOString().replace('.000Z','Z')===x.replace('.000Z','Z')});
const api=JSON.parse(fs.readFileSync(path.join(root,'openapi.json')));
ajv.addSchema({$id:'mancuerna',$defs:JSON.parse(JSON.stringify(api.components.schemas).replaceAll('#/components/schemas/','#/$defs/'))});
let schemas=0,endpointShapes=0;
for(const name of Object.keys(api.components.schemas)){ajv.compile({$ref:'mancuerna#/$defs/'+name});schemas++;}
function externalRefs(x){if(Array.isArray(x))return x.map(externalRefs);if(x&&typeof x==='object')return Object.fromEntries(Object.entries(x).map(([k,v])=>[k,k==='$ref'?'mancuerna'+v.replace('#/components/schemas/','#/$defs/'):externalRefs(v)]));return x;}
for(const route of Object.values(api.paths))for(const op of Object.values(route)){
 for(const response of Object.values(op.responses)){ajv.compile(externalRefs(response.content['application/json'].schema));endpointShapes++;}
 for(const p of op.parameters||[])ajv.compile(externalRefs(p.schema));
}
const fixtures=[...JSON.parse(fs.readFileSync(path.join(root,'fixtures/legacy-1.0.0.json'))),...JSON.parse(fs.readFileSync(path.join(root,'fixtures/contracts.json'))),...JSON.parse(fs.readFileSync(path.join(root,'fixtures/journeys.json')))];
for(const f of fixtures){const validate=ajv.compile({$ref:'mancuerna#/$defs/'+f.schema});assert.equal(validate(f.value),f.valid,f.name+': '+JSON.stringify(validate.errors));}
const proseExamples=JSON.parse(fs.readFileSync(path.join(root,'fixtures/prose-1.0.0.json')));
assert.equal(proseExamples.length,4);
for(const [i,name] of ['Command','Push','OperationResult','PullResult'].entries()){
 const validate=ajv.compile({$ref:'mancuerna#/$defs/'+name});assert.ok(validate(proseExamples[i]),'prose '+name+': '+JSON.stringify(validate.errors));
}
assert.equal(new Set(Object.values(api.paths).flatMap(x=>Object.values(x)).map(x=>x.operationId)).size, Object.values(api.paths).flatMap(x=>Object.values(x)).length);
assert.equal(api.paths['/sync/push'].post.security[0].Personal.length,0);
console.log(JSON.stringify({schemas,endpointShapes,fixtures:fixtures.length,proseExamples:proseExamples.length,passed:true}));
