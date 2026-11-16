-- Initial Tinta exercise names, authored for this catalog; no instructions.
-- Stable identities are additive only: a repeat must never overwrite edits or
-- revive an archived/deleted row. This forward-only seed changes no schema,
-- personal schema version, roles or account-owned data.
BEGIN;
INSERT INTO mancuerna.catalog_exercises(id,name,search_key,load_mode,notes,revision) VALUES
 ('27ec6e97-0f44-4031-9af8-abe3989bacc3','Sentadilla con barra','sentadilla con barra','external','',1),
 ('795a80db-c6da-45c2-aa32-45888d585f9f','Sentadilla con mancuernas','sentadilla con mancuernas','external','',1),
 ('47220b2f-7d85-4718-8e8b-8f27ca252c05','Prensa de piernas','prensa de piernas','external','',1),
 ('54413b67-3406-4c65-aff2-ef54ff2b9347','Peso muerto con barra','peso muerto con barra','external','',1),
 ('83e869fb-0bc4-415d-a955-4ecec09c7529','Peso muerto rumano con mancuernas','peso muerto rumano con mancuernas','external','',1),
 ('4d13ea4c-c306-4110-bbba-e145cffd7eab','Extensión de piernas en máquina','extension de piernas en maquina','external','',1),
 ('031a9f30-8626-4cf9-be95-4be39a1dd756','Curl femoral en máquina','curl femoral en maquina','external','',1),
 ('45ab8340-2432-43a6-9327-f570cf4b2186','Elevación de talones con mancuernas','elevacion de talones con mancuernas','external','',1),
 ('bf840aa1-3655-4681-8760-0ac0c39200af','Press de banca con barra','press de banca con barra','external','',1),
 ('27375a19-2ac3-4f6b-a6fd-323e410d9c34','Press de pecho con mancuernas','press de pecho con mancuernas','external','',1),
 ('69ac779d-aa1b-462e-b68a-6af4941b110d','Press de hombros con mancuernas','press de hombros con mancuernas','external','',1),
 ('d1d1db91-1d23-45ad-bf69-42da879cbe43','Elevaciones laterales con mancuernas','elevaciones laterales con mancuernas','external','',1),
 ('8c178569-f15f-4b02-965a-399f53c2b33f','Remo con barra','remo con barra','external','',1),
 ('4270d41e-1eed-40e7-be33-d89925f0d654','Remo con mancuerna','remo con mancuerna','external','',1),
 ('2a124eaa-95a9-4e1b-9daa-6d1ade6bfc3f','Jalón al pecho en polea','jalon al pecho en polea','external','',1),
 ('c220393a-ceff-48d0-999a-e716c5128331','Curl de bíceps con mancuernas','curl de biceps con mancuernas','external','',1),
 ('db2a4fdc-a79d-4666-8f7e-18ec28abb40c','Extensión de tríceps en polea','extension de triceps en polea','external','',1),
 ('068a2af4-18ea-46e6-b231-eec6e154b74c','Flexiones sin lastre','flexiones sin lastre','none','',1),
 ('008a2ee7-391c-447a-97d9-7765addcd3f5','Dominadas sin lastre','dominadas sin lastre','none','',1),
 ('74e0866d-4167-4a03-82c8-bac6515c4f54','Sentadilla sin carga','sentadilla sin carga','none','',1)
ON CONFLICT(id) DO NOTHING;
INSERT INTO public._migrations(version,name) VALUES(65,'065_mancuerna_initial_catalog.sql') ON CONFLICT(version) DO NOTHING;
COMMIT;
