-- get_schema.sql — the text of schema $2 if app $1 registered it. No row ⇒ owner.ErrNotFound (unknown
-- fingerprint, or another app's).
select s.body
  from app_schemas as a
       join schemas as s
         on s.fingerprint = a.fingerprint
 where a.app = $1
   and a.fingerprint = $2;
