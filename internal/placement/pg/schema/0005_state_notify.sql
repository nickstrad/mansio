-- 0005_state_notify.sql — registry state notifications for the locator's cache (the claim-pending follow-up of
-- CP-5/CP-6). Applied once by Migrate; never edit after it is applied.
-- The locator answers "moving" while a database's placement already names its new owner but the registry still
-- says creating/moving (the owner's Claim has not landed). The flip of the registry row to active must reach the
-- locator at once, or resolves would answer "moving" until the cache TTL: every committed change of a
-- database's state or target sends pg_notify('database_state', database_id). Inserts are not notified: a new
-- row is "creating" with no placement row, which the locator never caches.
-- Like 0003, notifications only save work (SQL G-11): a lost one costs "moving" answers until the listener's
-- reconnect drops the cache.
begin;

create function database_state_notify() returns trigger language plpgsql as $$
begin
  perform pg_notify('database_state', new.database_id);
  return null;
end $$;

create trigger database_state_notify
  after update of state, target_vm on databases
  for each row
  when ((old.state, old.target_vm) is distinct from (new.state, new.target_vm))
  execute function database_state_notify();

insert into schema_version (version) values (5);

commit;
