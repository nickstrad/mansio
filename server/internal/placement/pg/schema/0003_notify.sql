-- 0003_notify.sql — placement change notifications for the locator's cache. Applied once by Migrate; never
-- edit after it is applied.
-- Every committed change to a placement row sends pg_notify('placement', database_id || ':' || epoch). The
-- epoch is in the payload so two changes in one transaction never collapse into one notification (Postgres
-- folds identical payloads) and a late notification can be told from a fresh one. Insert and delete notify
-- too: a database created or dropped while a locator caches it.
-- Notifications only save work: a lost one costs a wrong_owner and a re-resolve. Nothing that must
-- not be lost (moves to resume, VM-loss work) rides on this channel; those are table scans.
begin;

create function placement_notify() returns trigger language plpgsql as $$
begin
  if tg_op = 'DELETE' then
    perform pg_notify('placement', old.database_id || ':' || old.epoch);
  else
    perform pg_notify('placement', new.database_id || ':' || new.epoch);
  end if;
  return null;
end $$;

create trigger placement_notify
  after insert or update or delete on placement
  for each row execute function placement_notify();

insert into schema_version (version) values (3);

commit;
