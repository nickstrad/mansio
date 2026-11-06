-- count_databases.sql — live (not dropped) databases: of tenant $1, and in all. Create on first use checks its
-- caps with this (controld locator.max_dbs_per_tenant, max_dbs). Creates are rare; the total is a scan.
select (select count(*)
          from databases
         where tenant_id = $1
           and state <> 'dropped') as tenant_live,
       (select count(*)
          from databases
         where state <> 'dropped') as total_live;
