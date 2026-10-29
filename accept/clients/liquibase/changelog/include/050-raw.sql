--liquibase formatted sql

--changeset app:17
create view V_SIM_SOLD as select ID, MSISDN, PRICE from SIM where STATUS = 'SOLD';
--rollback drop view V_SIM_SOLD;

--changeset app:18 splitStatements:true endDelimiter:;
create table AUDIT_LOG (ID bigint primary key, MSG text, TS timestamp default now());
insert into AUDIT_LOG (ID, MSG) values (1, 'liquibase raw sql');
--rollback drop table AUDIT_LOG;

--changeset app:19 labels:gap-expr-index
create index IDX_SIM_MSISDN_LOWER on SIM (lower(MSISDN));
--rollback drop index IDX_SIM_MSISDN_LOWER;

--changeset app:20
alter table SIM add constraint CHK_SIM_PRICE check (PRICE >= 0);
--rollback alter table SIM drop constraint CHK_SIM_PRICE;
