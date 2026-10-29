\set VERBOSITY verbose
\echo == 5b/9b FK to uuid PK
ALTER TABLE public.ORDER_ ADD CONSTRAINT FK_ORDER_ON_SIM FOREIGN KEY (SIM_ID) REFERENCES public.SIM (ID);
ALTER TABLE public.SIM_EVENT ADD CONSTRAINT FK_SIMEVENT_ON_SIM FOREIGN KEY (SIM_ID) REFERENCES public.SIM (ID);
\echo == 7c index on uuid
CREATE INDEX IDX_ORDER_SIM ON public.ORDER_(SIM_ID);
\echo == 19 expression index
create index IDX_SIM_MSISDN_LOWER on SIM (lower(MSISDN));
\echo == BLOB -> OID
CREATE TABLE public.KF_BLOB (ID INT, DATA OID);
\echo == pgjdbc getPrimaryKeys (two _pg_expandarray in the select list)
SELECT result.TABLE_NAME, result.COLUMN_NAME, result.KEY_SEQ FROM (SELECT ct.relname AS TABLE_NAME, a.attname AS COLUMN_NAME, (information_schema._pg_expandarray(i.indkey)).n AS KEY_SEQ, information_schema._pg_expandarray(i.indkey) AS KEYS, a.attnum AS A_ATTNUM FROM pg_catalog.pg_class ct JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid) JOIN pg_catalog.pg_index i ON (a.attrelid = i.indrelid) WHERE ct.relname = 'order_' AND i.indisprimary) result where result.A_ATTNUM = (result.KEYS).x;
